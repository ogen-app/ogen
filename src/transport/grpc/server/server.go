// Package server hosts Ogen's internal, operator-facing gRPC surface.
//
// Today it serves a single service — SecretsService — so Harbor (Ogen's
// operations dashboard) can list / rotate / clear the third-party API keys in
// the `secret` table through the SAME secrets.Store the rest of the app uses.
// Going through the Store (rather than the table) reuses the envelope crypto,
// the name allowlist, and the hot-reload subscription hooks with zero
// duplication.
//
// The listener is internal-only and every call is gated by a shared bearer
// token (constant-time compared). Transport is plain (insecure) like Ogen's
// pdf/video gRPC clients, so the token is only as safe as the network it
// crosses: the default bind is loopback-only (config.GRPCAddr) and, when
// exposed across hosts, the operator is expected to keep it on a private
// network / behind a NetworkPolicy and add mTLS at the infra or transport layer
// (grpc.Creds). Never carry the token over an untrusted path in cleartext.
package server

import (
	"context"
	"crypto/subtle"
	"errors"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	secretsv1 "github.com/ogen-app/ogen/gen/secrets/v1"
	tenantsv1 "github.com/ogen-app/ogen/gen/tenants/v1"
	"github.com/ogen-app/ogen/src/domain/entitlements"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/infra/secrets"
)

// authMetadataKey is the (lowercased) gRPC metadata key carrying the shared
// bearer token. gRPC lowercases metadata keys, so callers may send
// "Authorization" — it arrives here as "authorization".
const authMetadataKey = "authorization"

// New builds the internal gRPC server: a token-authenticated SecretsService
// backed by store, plus the CON-208 TenantAdminService backed by the tenant /
// tier / group repositories. The single token interceptor gates every RPC on
// both services. An empty token is rejected — the caller must decide NOT to
// start the server at all rather than run it unauthenticated.
func New(
	token string,
	store secrets.Store,
	tierRepo repository.TenantTierRepository,
	groupRepo repository.TenantGroupRepository,
	tenantRepo repository.TenantRepository,
	platformRepo repository.PlatformRepository,
	platformLimitsRepo repository.PlatformGlobalLimitsRepository,
	versionRepo repository.TenantTierVersionRepository,
	assignmentRepo repository.TenantTierAssignmentRepository,
	emailLogRepo repository.EmailLogRepository,
	emailEventRepo repository.EmailEventRepository,
	emailBodies EmailBodyGetter,
	announcementRepo repository.AnnouncementRepository,
) (*grpc.Server, error) {
	// Env-configured secrets frequently arrive with a trailing newline (a very
	// common Railway / docker-compose paste mistake). Trim it here so the
	// byte-for-byte token compare in the interceptor doesn't silently reject an
	// otherwise-correct token.
	token = strings.TrimSpace(token)
	if token == "" {
		return nil, errors.New("grpcserver: auth token is required")
	}
	srv := grpc.NewServer(
		grpc.ChainUnaryInterceptor(tokenAuthInterceptor(token)),
	)
	// CON-294: the versioned-tier-entitlement layer. Load the engineering-owned
	// feature catalog (fails fast on a malformed embed) and build the point-in-time
	// resolver both PlanAdminService and the CON-294 SetTenantTier stamp use.
	catalog, err := entitlements.LoadCatalog()
	if err != nil {
		return nil, err
	}
	resolver := entitlements.NewResolver(versionRepo, assignmentRepo, tenantRepo, catalog)

	secretsv1.RegisterSecretsServiceServer(srv, newSecretsService(store))
	// TenantAdminService also gets the version + assignment repos: SetTenantTier
	// now stamps a tenant_tier_assignment so tenants.tier_id and the open
	// assignment never drift (CON-294).
	tenantsv1.RegisterTenantAdminServiceServer(srv, newTenantAdminService(tierRepo, groupRepo, tenantRepo, versionRepo, assignmentRepo))
	registerPlatformAdmin(srv, platformRepo, platformLimitsRepo)
	registerPlanAdmin(srv, versionRepo, assignmentRepo, catalog, resolver)
	// CON-298: EmailAdminService serves a tenant's email history + per-email
	// detail (rendered body fetched live from Resend) to Harbor's Emails tab.
	registerEmailAdmin(srv, emailLogRepo, emailEventRepo, emailBodies)
	// CON-230: AnnouncementAdminService lets Harbor author informational
	// announcements (banners) and read their per-user click/dismiss engagement.
	registerAnnouncementAdmin(srv, announcementRepo)
	return srv, nil
}

// tokenAuthInterceptor rejects any unary call whose `authorization` metadata
// is not exactly `Bearer <token>`. The comparison is constant-time and the
// token is never logged.
func tokenAuthInterceptor(token string) grpc.UnaryServerInterceptor {
	want := []byte("Bearer " + token)
	return func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		md, ok := metadata.FromIncomingContext(ctx)
		if !ok {
			return nil, status.Error(codes.Unauthenticated, "missing metadata")
		}
		vals := md.Get(authMetadataKey)
		if len(vals) == 0 {
			return nil, status.Error(codes.Unauthenticated, "missing authorization")
		}
		// Trim surrounding whitespace before comparing: per HTTP semantics header
		// OWS (spaces/tabs) is insignificant, so a client token padded with a
		// trailing space still authenticates. (A raw newline can't traverse an
		// HTTP/2 header at all — the client transport rejects it before send — so
		// the env-newline paste footgun is caught server-side in New, not here.)
		// The trim only strips non-secret padding, so the constant-time compare
		// stays the sole gate on the token value. ConstantTimeCompare is
		// constant-time only for equal-length slices — it returns early (0) on a
		// length mismatch — so it hides the token's contents, not its length.
		got := strings.TrimSpace(vals[0])
		if subtle.ConstantTimeCompare([]byte(got), want) != 1 {
			return nil, status.Error(codes.Unauthenticated, "invalid token")
		}
		return handler(ctx, req)
	}
}
