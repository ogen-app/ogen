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

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	secretsv1 "github.com/ogen-app/ogen/gen/secrets/v1"
	tenantsv1 "github.com/ogen-app/ogen/gen/tenants/v1"
	"github.com/ogen-app/ogen/src/domain/entitlements"
	"github.com/ogen-app/ogen/src/genkit/modelprobe"
	"github.com/ogen-app/ogen/src/infra/eventhub"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/infra/secrets"
)

// authMetadataKey is the (lowercased) gRPC metadata key carrying the shared
// bearer token. gRPC lowercases metadata keys, so callers may send
// "Authorization" — it arrives here as "authorization".
const authMetadataKey = "authorization"

// Deps is everything the internal gRPC server's services read. Only Token is
// required at construction; tests leave unset the collaborators of services
// they don't exercise.
type Deps struct {
	// Token is the shared bearer token. Surrounding whitespace is trimmed; an
	// empty token is rejected.
	Token   string
	Secrets secrets.Store

	Tiers           repository.TenantTierRepository
	Groups          repository.TenantGroupRepository
	Tenants         repository.TenantRepository
	TierVersions    repository.TenantTierVersionRepository
	TierAssignments repository.TenantTierAssignmentRepository

	Platforms        repository.PlatformRepository
	PlatformLimits   repository.PlatformGlobalLimitsRepository
	FlowModelConfigs repository.FlowModelConfigRepository

	EmailLogs   repository.EmailLogRepository
	EmailEvents repository.EmailEventRepository
	EmailBodies repository.EmailBodyRepository
	// LiveEmailBodies fetches a rendered body from Resend when it is not
	// stored locally.
	LiveEmailBodies EmailBodyGetter
	// Users, AdminEmailEnqueuer and HarborBaseURL drive the admin
	// registration notification: owner lookup, durable per-recipient sends,
	// and the "View in Harbor" deep link.
	Users              repository.UserRepository
	AdminEmailEnqueuer AdminEmailEnqueuer
	HarborBaseURL      string

	Announcements repository.AnnouncementRepository
	// Hub carries the entitlement-invalidation event an operator tier change
	// publishes onto the tenant's /api/events stream.
	Hub eventhub.Hub
}

// New builds the internal gRPC server with every operator service registered
// behind one token interceptor. An empty token is rejected — the caller must
// decide NOT to start the server rather than run it unauthenticated.
func New(d Deps) (*grpc.Server, error) {
	// Env-configured secrets frequently arrive with a trailing newline (a very
	// common Railway / docker-compose paste mistake). Trim it here so the
	// byte-for-byte token compare in the interceptor doesn't silently reject an
	// otherwise-correct token.
	token := strings.TrimSpace(d.Token)
	if token == "" {
		return nil, errors.New("grpcserver: auth token is required")
	}
	srv := grpc.NewServer(
		grpc.StatsHandler(otelgrpc.NewServerHandler()),
		grpc.ChainUnaryInterceptor(tokenAuthInterceptor(token)),
	)
	// A malformed embedded feature catalog fails boot here.
	catalog, err := entitlements.LoadCatalog()
	if err != nil {
		return nil, err
	}
	resolver := entitlements.NewResolver(d.TierVersions, d.TierAssignments, d.Tenants, catalog)

	secretsv1.RegisterSecretsServiceServer(srv, newSecretsService(d.Secrets))
	tenantsv1.RegisterTenantAdminServiceServer(srv, newTenantAdminService(d.Tiers, d.Groups, d.Tenants, d.TierVersions, d.TierAssignments, d.Hub))
	registerPlatformAdmin(srv, d.Platforms, d.PlatformLimits)
	registerModelConfigAdmin(srv, d.FlowModelConfigs, modelprobe.New(d.Secrets))
	registerPlanAdmin(srv, d.TierVersions, d.TierAssignments, catalog, resolver, d.Hub)
	registerEmailAdmin(srv, d.EmailLogs, d.EmailEvents, d.EmailBodies, d.LiveEmailBodies, d.Tenants, d.Users, d.AdminEmailEnqueuer, d.HarborBaseURL)
	registerAnnouncementAdmin(srv, d.Announcements)
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
