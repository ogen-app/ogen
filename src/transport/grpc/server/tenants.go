package server

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	tenantsv1 "github.com/ogen-app/ogen/gen/tenants/v1"
	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/logging"
)

// tenantAdminService adapts the tenant/tier/group repositories to the generated
// TenantAdminServiceServer (CON-208). It is the operator-facing surface Harbor
// uses to read and manage tenant classification. All three repositories operate
// on global tables, so no tenantctx is threaded here — the work is cross-tenant
// by design and gated only by the shared bearer token (see server.go).
type tenantAdminService struct {
	tenantsv1.UnimplementedTenantAdminServiceServer
	tierRepo       repository.TenantTierRepository
	groupRepo      repository.TenantGroupRepository
	tenantRepo     repository.TenantRepository
	versionRepo    repository.TenantTierVersionRepository
	assignmentRepo repository.TenantTierAssignmentRepository
}

func newTenantAdminService(
	tierRepo repository.TenantTierRepository,
	groupRepo repository.TenantGroupRepository,
	tenantRepo repository.TenantRepository,
	versionRepo repository.TenantTierVersionRepository,
	assignmentRepo repository.TenantTierAssignmentRepository,
) *tenantAdminService {
	return &tenantAdminService{
		tierRepo:       tierRepo,
		groupRepo:      groupRepo,
		tenantRepo:     tenantRepo,
		versionRepo:    versionRepo,
		assignmentRepo: assignmentRepo,
	}
}

// Postgres SQLSTATEs surfaced by the repositories and mapped to status codes.
const (
	pgUniqueViolation = "23505"
	pgFKViolation     = "23503"
)

// hexColor matches an optional #RRGGBB label. An empty string means "no color"
// and is allowed separately.
var hexColor = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

// --- reads ---

func (s *tenantAdminService) ListTiers(ctx context.Context, _ *tenantsv1.ListTiersRequest) (*tenantsv1.ListTiersResponse, error) {
	tiers, err := s.tierRepo.List(ctx)
	if err != nil {
		return nil, s.internal(ctx, "list tiers", err)
	}
	out := make([]*tenantsv1.Tier, 0, len(tiers))
	for i := range tiers {
		out = append(out, toTierProto(&tiers[i]))
	}
	return &tenantsv1.ListTiersResponse{Tiers: out}, nil
}

func (s *tenantAdminService) ListGroups(ctx context.Context, _ *tenantsv1.ListGroupsRequest) (*tenantsv1.ListGroupsResponse, error) {
	groups, err := s.groupRepo.List(ctx)
	if err != nil {
		return nil, s.internal(ctx, "list groups", err)
	}
	out := make([]*tenantsv1.Group, 0, len(groups))
	for i := range groups {
		out = append(out, toGroupProto(&groups[i]))
	}
	return &tenantsv1.ListGroupsResponse{Groups: out}, nil
}

func (s *tenantAdminService) GetTenant(ctx context.Context, req *tenantsv1.GetTenantRequest) (*tenantsv1.GetTenantResponse, error) {
	id := strings.TrimSpace(req.GetTenantId())
	if id == "" {
		return nil, status.Error(codes.InvalidArgument, "tenant_id is required")
	}
	tenant, err := s.tenantRepo.GetByIDWithClassification(ctx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, status.Error(codes.NotFound, "tenant not found")
		}
		return nil, s.internal(ctx, "get tenant", err)
	}
	return &tenantsv1.GetTenantResponse{Tenant: toTenantProto(tenant)}, nil
}

func (s *tenantAdminService) ListTenants(ctx context.Context, req *tenantsv1.ListTenantsRequest) (*tenantsv1.ListTenantsResponse, error) {
	wantStatus := strings.TrimSpace(req.GetStatus())
	if wantStatus != "" && !validTenantStatus(wantStatus) {
		return nil, status.Error(codes.InvalidArgument, "status must be one of active, suspended, deleted")
	}
	tenants, total, err := s.tenantRepo.ListWithClassification(ctx, repository.TenantListFilter{
		TierID:  strings.TrimSpace(req.GetTierId()),
		GroupID: strings.TrimSpace(req.GetGroupId()),
		Status:  wantStatus,
		Limit:   int(req.GetPageSize()),
		Offset:  int(req.GetOffset()),
	})
	if err != nil {
		return nil, s.internal(ctx, "list tenants", err)
	}
	out := make([]*tenantsv1.Tenant, 0, len(tenants))
	for i := range tenants {
		out = append(out, toTenantProto(&tenants[i]))
	}
	return &tenantsv1.ListTenantsResponse{Tenants: out, Total: int32(total)}, nil
}

// --- tier catalog writes ---

func (s *tenantAdminService) CreateTier(ctx context.Context, req *tenantsv1.CreateTierRequest) (*tenantsv1.CreateTierResponse, error) {
	name := strings.TrimSpace(req.GetName())
	if name == "" {
		return nil, status.Error(codes.InvalidArgument, "name is required")
	}
	if err := validateColor(req.GetColor()); err != nil {
		return nil, err
	}
	id, err := models.NewID()
	if err != nil {
		return nil, s.internal(ctx, "create tier", err)
	}
	now := time.Now().UTC()
	tier := &models.TenantTier{ID: id, Name: name, Color: req.GetColor(), Description: req.GetDescription(), CreatedAt: now, UpdatedAt: now}
	if err := s.tierRepo.Create(ctx, tier); err != nil {
		if pgCode(err) == pgUniqueViolation {
			return nil, status.Errorf(codes.AlreadyExists, "a tier named %q already exists", name)
		}
		return nil, s.internal(ctx, "create tier", err)
	}
	slog.InfoContext(ctx, "tenant tier created", logging.AttrComponent, "grpcserver", "tier_id", id, "name", name)
	return &tenantsv1.CreateTierResponse{Tier: toTierProto(tier)}, nil
}

func (s *tenantAdminService) UpdateTier(ctx context.Context, req *tenantsv1.UpdateTierRequest) (*tenantsv1.UpdateTierResponse, error) {
	id := strings.TrimSpace(req.GetId())
	if id == "" {
		return nil, status.Error(codes.InvalidArgument, "id is required")
	}
	name := strings.TrimSpace(req.GetName())
	if name == "" {
		return nil, status.Error(codes.InvalidArgument, "name is required")
	}
	if err := validateColor(req.GetColor()); err != nil {
		return nil, err
	}
	tier := &models.TenantTier{ID: id, Name: name, Color: req.GetColor(), Description: req.GetDescription()}
	if err := s.tierRepo.Update(ctx, tier); err != nil {
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return nil, status.Error(codes.NotFound, "tier not found")
		case pgCode(err) == pgUniqueViolation:
			return nil, status.Errorf(codes.AlreadyExists, "a tier named %q already exists", name)
		default:
			return nil, s.internal(ctx, "update tier", err)
		}
	}
	updated, err := s.tierRepo.GetByID(ctx, id)
	if err != nil {
		return nil, s.internal(ctx, "update tier", err)
	}
	slog.InfoContext(ctx, "tenant tier updated", logging.AttrComponent, "grpcserver", "tier_id", id)
	return &tenantsv1.UpdateTierResponse{Tier: toTierProto(updated)}, nil
}

func (s *tenantAdminService) DeleteTier(ctx context.Context, req *tenantsv1.DeleteTierRequest) (*tenantsv1.DeleteTierResponse, error) {
	id := strings.TrimSpace(req.GetId())
	if id == "" {
		return nil, status.Error(codes.InvalidArgument, "id is required")
	}
	if id == models.DefaultTierID {
		return nil, status.Error(codes.FailedPrecondition, "the default tier cannot be deleted")
	}
	deleted, err := s.tierRepo.Delete(ctx, id)
	if err != nil {
		if pgCode(err) == pgFKViolation {
			// Draft versions are removed with the tier (see the repository), so a
			// remaining FK is either an assigned tenant or a published version.
			// Name the actual blocker so the remediation is actionable.
			if fkReferencesVersions(err) {
				return nil, status.Error(codes.FailedPrecondition, "tier still has published versions and cannot be deleted")
			}
			return nil, status.Error(codes.FailedPrecondition, "tier is still assigned to one or more tenants; reassign them first")
		}
		return nil, s.internal(ctx, "delete tier", err)
	}
	if !deleted {
		return nil, status.Error(codes.NotFound, "tier not found")
	}
	slog.InfoContext(ctx, "tenant tier deleted", logging.AttrComponent, "grpcserver", "tier_id", id)
	return &tenantsv1.DeleteTierResponse{}, nil
}

// --- group catalog writes ---

func (s *tenantAdminService) CreateGroup(ctx context.Context, req *tenantsv1.CreateGroupRequest) (*tenantsv1.CreateGroupResponse, error) {
	name := strings.TrimSpace(req.GetName())
	if name == "" {
		return nil, status.Error(codes.InvalidArgument, "name is required")
	}
	if err := validateColor(req.GetColor()); err != nil {
		return nil, err
	}
	id, err := models.NewID()
	if err != nil {
		return nil, s.internal(ctx, "create group", err)
	}
	now := time.Now().UTC()
	group := &models.TenantGroup{ID: id, Name: name, Color: req.GetColor(), Description: req.GetDescription(), CreatedAt: now, UpdatedAt: now}
	if err := s.groupRepo.Create(ctx, group); err != nil {
		if pgCode(err) == pgUniqueViolation {
			return nil, status.Errorf(codes.AlreadyExists, "a group named %q already exists", name)
		}
		return nil, s.internal(ctx, "create group", err)
	}
	slog.InfoContext(ctx, "tenant group created", logging.AttrComponent, "grpcserver", "group_id", id, "name", name)
	return &tenantsv1.CreateGroupResponse{Group: toGroupProto(group)}, nil
}

func (s *tenantAdminService) UpdateGroup(ctx context.Context, req *tenantsv1.UpdateGroupRequest) (*tenantsv1.UpdateGroupResponse, error) {
	id := strings.TrimSpace(req.GetId())
	if id == "" {
		return nil, status.Error(codes.InvalidArgument, "id is required")
	}
	name := strings.TrimSpace(req.GetName())
	if name == "" {
		return nil, status.Error(codes.InvalidArgument, "name is required")
	}
	if err := validateColor(req.GetColor()); err != nil {
		return nil, err
	}
	group := &models.TenantGroup{ID: id, Name: name, Color: req.GetColor(), Description: req.GetDescription()}
	if err := s.groupRepo.Update(ctx, group); err != nil {
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return nil, status.Error(codes.NotFound, "group not found")
		case pgCode(err) == pgUniqueViolation:
			return nil, status.Errorf(codes.AlreadyExists, "a group named %q already exists", name)
		default:
			return nil, s.internal(ctx, "update group", err)
		}
	}
	updated, err := s.groupRepo.GetByID(ctx, id)
	if err != nil {
		return nil, s.internal(ctx, "update group", err)
	}
	slog.InfoContext(ctx, "tenant group updated", logging.AttrComponent, "grpcserver", "group_id", id)
	return &tenantsv1.UpdateGroupResponse{Group: toGroupProto(updated)}, nil
}

func (s *tenantAdminService) DeleteGroup(ctx context.Context, req *tenantsv1.DeleteGroupRequest) (*tenantsv1.DeleteGroupResponse, error) {
	id := strings.TrimSpace(req.GetId())
	if id == "" {
		return nil, status.Error(codes.InvalidArgument, "id is required")
	}
	deleted, err := s.groupRepo.Delete(ctx, id)
	if err != nil {
		return nil, s.internal(ctx, "delete group", err)
	}
	if !deleted {
		return nil, status.Error(codes.NotFound, "group not found")
	}
	slog.InfoContext(ctx, "tenant group deleted", logging.AttrComponent, "grpcserver", "group_id", id)
	return &tenantsv1.DeleteGroupResponse{}, nil
}

// --- tenant <-> group membership ---

func (s *tenantAdminService) AddTenantToGroup(ctx context.Context, req *tenantsv1.AddTenantToGroupRequest) (*tenantsv1.AddTenantToGroupResponse, error) {
	tenantID := strings.TrimSpace(req.GetTenantId())
	groupID := strings.TrimSpace(req.GetGroupId())
	if tenantID == "" || groupID == "" {
		return nil, status.Error(codes.InvalidArgument, "tenant_id and group_id are required")
	}
	if err := s.groupRepo.AddTenant(ctx, tenantID, groupID); err != nil {
		if pgCode(err) == pgFKViolation {
			return nil, status.Error(codes.NotFound, "tenant or group not found")
		}
		return nil, s.internal(ctx, "add tenant to group", err)
	}
	slog.InfoContext(ctx, "tenant added to group", logging.AttrComponent, "grpcserver", "tenant_id", tenantID, "group_id", groupID)
	return &tenantsv1.AddTenantToGroupResponse{}, nil
}

func (s *tenantAdminService) RemoveTenantFromGroup(ctx context.Context, req *tenantsv1.RemoveTenantFromGroupRequest) (*tenantsv1.RemoveTenantFromGroupResponse, error) {
	tenantID := strings.TrimSpace(req.GetTenantId())
	groupID := strings.TrimSpace(req.GetGroupId())
	if tenantID == "" || groupID == "" {
		return nil, status.Error(codes.InvalidArgument, "tenant_id and group_id are required")
	}
	// Idempotent: removing a non-member is a success, not an error.
	if _, err := s.groupRepo.RemoveTenant(ctx, tenantID, groupID); err != nil {
		return nil, s.internal(ctx, "remove tenant from group", err)
	}
	slog.InfoContext(ctx, "tenant removed from group", logging.AttrComponent, "grpcserver", "tenant_id", tenantID, "group_id", groupID)
	return &tenantsv1.RemoveTenantFromGroupResponse{}, nil
}

// --- tenant tier assignment ---

func (s *tenantAdminService) SetTenantTier(ctx context.Context, req *tenantsv1.SetTenantTierRequest) (*tenantsv1.SetTenantTierResponse, error) {
	tenantID := strings.TrimSpace(req.GetTenantId())
	tierID := strings.TrimSpace(req.GetTierId())
	if tenantID == "" || tierID == "" {
		return nil, status.Error(codes.InvalidArgument, "tenant_id and tier_id are required")
	}
	// CON-294: stamp an assignment to the tier's latest active version (nil when
	// the tier has none yet, e.g. a freshly created unpublished tier) so
	// tenants.tier_id and the tenant's open assignment stay consistent. Reassign
	// updates tier_id, closes the old open assignment, and opens the new one in
	// one transaction.
	var versionID *string
	switch v, verr := s.versionRepo.LatestActiveByTier(ctx, tierID); {
	case verr == nil:
		versionID = &v.ID
	case errors.Is(verr, sql.ErrNoRows):
		// tier has no active version — set tier_id only, no assignment stamped.
	default:
		return nil, s.internal(ctx, "set tenant tier", verr)
	}
	ok, err := s.assignmentRepo.Reassign(ctx, tenantID, tierID, versionID, models.AssignmentReasonOperatorSet, time.Now().UTC())
	if err != nil {
		if pgCode(err) == pgFKViolation {
			return nil, status.Error(codes.FailedPrecondition, "no such tier")
		}
		return nil, s.internal(ctx, "set tenant tier", err)
	}
	if !ok {
		return nil, status.Error(codes.NotFound, "tenant not found")
	}
	tenant, err := s.tenantRepo.GetByIDWithClassification(ctx, tenantID)
	if err != nil {
		return nil, s.internal(ctx, "set tenant tier", err)
	}
	slog.InfoContext(ctx, "tenant tier set", logging.AttrComponent, "grpcserver", "tenant_id", tenantID, "tier_id", tierID)
	return &tenantsv1.SetTenantTierResponse{Tenant: toTenantProto(tenant)}, nil
}

// --- tenant lifecycle status (CON-190) ---

func (s *tenantAdminService) SetTenantStatus(ctx context.Context, req *tenantsv1.SetTenantStatusRequest) (*tenantsv1.SetTenantStatusResponse, error) {
	tenantID := strings.TrimSpace(req.GetTenantId())
	newStatus := strings.TrimSpace(req.GetStatus())
	if tenantID == "" {
		return nil, status.Error(codes.InvalidArgument, "tenant_id is required")
	}
	if !validTenantStatus(newStatus) {
		return nil, status.Error(codes.InvalidArgument, "status must be one of active, suspended, deleted")
	}
	// The default tenant holds pre-CON-97 backfilled data and is the platform
	// fallback; it may not be suspended or deleted (mirrors the default-tier
	// delete protection in CON-208).
	if tenantID == models.DefaultTenantID && newStatus != models.TenantStatusActive {
		return nil, status.Error(codes.FailedPrecondition, "the default tenant cannot be suspended or deleted")
	}
	// reason is meaningful only for a suspension; clear it otherwise so a
	// reactivated/restored tenant carries no stale note.
	reason := ""
	if newStatus == models.TenantStatusSuspended {
		reason = strings.TrimSpace(req.GetReason())
	}
	ok, err := s.tenantRepo.SetStatus(ctx, tenantID, newStatus, reason, time.Now().UTC())
	if err != nil {
		return nil, s.internal(ctx, "set tenant status", err)
	}
	if !ok {
		return nil, status.Error(codes.NotFound, "tenant not found")
	}
	tenant, err := s.tenantRepo.GetByIDWithClassification(ctx, tenantID)
	if err != nil {
		return nil, s.internal(ctx, "set tenant status", err)
	}
	slog.InfoContext(ctx, "tenant status set", logging.AttrComponent, "grpcserver", "tenant_id", tenantID, "status", newStatus, "reason", reason)
	return &tenantsv1.SetTenantStatusResponse{Tenant: toTenantProto(tenant)}, nil
}

// --- helpers ---

// validTenantStatus reports whether s is one of the closed lifecycle statuses.
func validTenantStatus(s string) bool {
	switch s {
	case models.TenantStatusActive, models.TenantStatusSuspended, models.TenantStatusDeleted:
		return true
	default:
		return false
	}
}

// internal logs an unexpected error (name/id only, never sensitive values) and
// returns a generic Internal status so DB internals never cross the wire —
// mirroring secrets.go's mapStoreErr.
func (s *tenantAdminService) internal(ctx context.Context, op string, err error) error {
	slog.ErrorContext(ctx, "tenant admin "+op, logging.AttrComponent, "grpcserver", logging.AttrError, err)
	return status.Error(codes.Internal, op+" failed")
}

func validateColor(color string) error {
	if color == "" || hexColor.MatchString(color) {
		return nil
	}
	return status.Error(codes.InvalidArgument, "color must be empty or a #RRGGBB hex string")
}

// pgCode returns the Postgres SQLSTATE of err, or "" if err is not a PgError.
func pgCode(err error) string {
	if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok {
		return pgErr.Code
	}
	return ""
}

// fkReferencesVersions reports whether an FK-violation error came from the
// tenant_tier_versions -> tenant_tiers constraint (a published version) rather
// than tenants -> tenant_tiers (an assigned tenant). Falls back to false (the
// tenant message) when the constraint/table can't be identified.
func fkReferencesVersions(err error) bool {
	if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok {
		return strings.Contains(pgErr.ConstraintName, "tenant_tier_versions") ||
			pgErr.TableName == "tenant_tier_versions"
	}
	return false
}

func toTierProto(t *models.TenantTier) *tenantsv1.Tier {
	if t == nil {
		return nil
	}
	return &tenantsv1.Tier{
		Id:          t.ID,
		Name:        t.Name,
		Color:       t.Color,
		Description: t.Description,
		CreatedAt:   timestamppb.New(t.CreatedAt),
		UpdatedAt:   timestamppb.New(t.UpdatedAt),
	}
}

func toGroupProto(g *models.TenantGroup) *tenantsv1.Group {
	if g == nil {
		return nil
	}
	return &tenantsv1.Group{
		Id:          g.ID,
		Name:        g.Name,
		Color:       g.Color,
		Description: g.Description,
		CreatedAt:   timestamppb.New(g.CreatedAt),
		UpdatedAt:   timestamppb.New(g.UpdatedAt),
	}
}

func toTenantProto(t *models.Tenant) *tenantsv1.Tenant {
	groups := make([]*tenantsv1.Group, 0, len(t.Groups))
	for i := range t.Groups {
		groups = append(groups, toGroupProto(&t.Groups[i]))
	}
	return &tenantsv1.Tenant{
		Id:           t.ID,
		Name:         t.Name,
		Slug:         t.Slug,
		Tier:         toTierProto(t.Tier),
		Groups:       groups,
		CreatedAt:    timestamppb.New(t.CreatedAt),
		UpdatedAt:    timestamppb.New(t.UpdatedAt),
		Status:       t.Status,
		StatusReason: t.StatusReason,
	}
}
