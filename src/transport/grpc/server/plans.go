package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"log/slog"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	plansv1 "github.com/ogen-app/ogen/gen/plans/v1"
	"github.com/ogen-app/ogen/src/domain/entitlements"
	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/eventhub"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/logging"
)

// planAdminService adapts the tier-version + assignment repositories to the
// generated PlanAdminServiceServer (CON-294): the operator-facing surface Harbor
// uses to author, publish, retire and assign versioned tier entitlements
// (CON-243). All tables are global, so no tenantctx is threaded here — the work
// is cross-tenant by design and gated only by the shared bearer token.
type planAdminService struct {
	plansv1.UnimplementedPlanAdminServiceServer
	versions    repository.TenantTierVersionRepository
	assignments repository.TenantTierAssignmentRepository
	catalog     *entitlements.Catalog
	resolver    *entitlements.Resolver
	hub         eventhub.Hub // CON-295: entitlement-invalidation events (nil-safe)
}

func newPlanAdminService(
	versions repository.TenantTierVersionRepository,
	assignments repository.TenantTierAssignmentRepository,
	catalog *entitlements.Catalog,
	resolver *entitlements.Resolver,
	hub eventhub.Hub,
) *planAdminService {
	return &planAdminService{versions: versions, assignments: assignments, catalog: catalog, resolver: resolver, hub: hub}
}

// --- reads ---

func (s *planAdminService) ListTierVersions(ctx context.Context, req *plansv1.ListTierVersionsRequest) (*plansv1.ListTierVersionsResponse, error) {
	tierID := strings.TrimSpace(req.GetTierId())
	if tierID == "" {
		return nil, status.Error(codes.InvalidArgument, "tier_id is required")
	}
	versions, err := s.versions.ListByTier(ctx, tierID)
	if err != nil {
		return nil, s.internal(ctx, "list tier versions", err)
	}
	ids := make([]string, len(versions))
	for i := range versions {
		ids[i] = versions[i].ID
	}
	prices, err := s.versions.PricesByVersionIDs(ctx, ids)
	if err != nil {
		return nil, s.internal(ctx, "list tier versions", err)
	}
	counts, err := s.versions.OpenAssignmentCounts(ctx, ids)
	if err != nil {
		return nil, s.internal(ctx, "list tier versions", err)
	}
	out := make([]*plansv1.TierVersion, 0, len(versions))
	for i := range versions {
		tv, err := tierVersionProto(&versions[i], prices[versions[i].ID], counts[versions[i].ID])
		if err != nil {
			return nil, s.internal(ctx, "list tier versions", err)
		}
		out = append(out, tv)
	}
	return &plansv1.ListTierVersionsResponse{Versions: out}, nil
}

func (s *planAdminService) GetTierVersion(ctx context.Context, req *plansv1.GetTierVersionRequest) (*plansv1.GetTierVersionResponse, error) {
	id := strings.TrimSpace(req.GetId())
	if id == "" {
		return nil, status.Error(codes.InvalidArgument, "id is required")
	}
	tv, err := s.versionProto(ctx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, status.Error(codes.NotFound, "tier version not found")
		}
		return nil, s.internal(ctx, "get tier version", err)
	}
	return &plansv1.GetTierVersionResponse{Version: tv}, nil
}

func (s *planAdminService) ListFeatures(ctx context.Context, _ *plansv1.ListFeaturesRequest) (*plansv1.ListFeaturesResponse, error) {
	features := s.catalog.Features()
	out := make([]*plansv1.Feature, 0, len(features))
	for _, f := range features {
		out = append(out, featureProto(f))
	}
	return &plansv1.ListFeaturesResponse{Features: out}, nil
}

func (s *planAdminService) GetTenantEntitlements(ctx context.Context, req *plansv1.GetTenantEntitlementsRequest) (*plansv1.GetTenantEntitlementsResponse, error) {
	tenantID := strings.TrimSpace(req.GetTenantId())
	if tenantID == "" {
		return nil, status.Error(codes.InvalidArgument, "tenant_id is required")
	}
	res, err := s.resolver.ResolveCurrent(ctx, tenantID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, status.Error(codes.NotFound, "no entitlements resolved for this tenant")
		}
		return nil, s.internal(ctx, "get tenant entitlements", err)
	}
	tv, err := s.versionProto(ctx, res.VersionID)
	if err != nil {
		return nil, s.internal(ctx, "get tenant entitlements", err)
	}
	return &plansv1.GetTenantEntitlementsResponse{TenantId: tenantID, Version: tv}, nil
}

// --- version authoring ---

func (s *planAdminService) CreateTierVersion(ctx context.Context, req *plansv1.CreateTierVersionRequest) (*plansv1.CreateTierVersionResponse, error) {
	tierID := strings.TrimSpace(req.GetTierId())
	if tierID == "" {
		return nil, status.Error(codes.InvalidArgument, "tier_id is required")
	}

	// Entitlements + prices come either from a cloned version or from the request.
	var ents map[string]any
	var prices []models.TenantTierVersionPrice
	if cloneID := strings.TrimSpace(req.GetCloneFromVersionId()); cloneID != "" {
		src, err := s.versions.GetByID(ctx, cloneID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil, status.Error(codes.NotFound, "clone_from_version_id not found")
			}
			return nil, s.internal(ctx, "create tier version", err)
		}
		ents = src.Entitlements
		srcPrices, err := s.versions.PricesByVersion(ctx, cloneID)
		if err != nil {
			return nil, s.internal(ctx, "create tier version", err)
		}
		if prices, err = pricesFromModels(srcPrices); err != nil {
			return nil, s.internal(ctx, "create tier version", err)
		}
	} else {
		ents = req.GetEntitlements().AsMap()
		var err error
		if prices, err = pricesFromProto(req.GetPrices()); err != nil {
			return nil, s.internal(ctx, "create tier version", err)
		}
	}
	if err := s.catalog.Validate(ents); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	next, err := s.versions.NextVersion(ctx, tierID)
	if err != nil {
		return nil, s.internal(ctx, "create tier version", err)
	}
	id, err := models.NewID()
	if err != nil {
		return nil, s.internal(ctx, "create tier version", err)
	}
	v := &models.TenantTierVersion{
		ID:           id,
		TierID:       tierID,
		Version:      next,
		Status:       models.TierVersionStatusDraft,
		Purchasable:  req.GetPurchasable(),
		Entitlements: ents,
		CreatedAt:    time.Now().UTC(),
	}
	if err := s.versions.Create(ctx, v, prices); err != nil {
		switch pgCode(err) {
		case pgFKViolation:
			return nil, status.Errorf(codes.FailedPrecondition, "no such tier %q", tierID)
		case pgUniqueViolation:
			// NextVersion + Create is not atomic; a concurrent create can grab the
			// same (tier_id, version). Surface the unique violation as a retryable
			// Aborted rather than an opaque Internal.
			return nil, status.Error(codes.Aborted, "a version was concurrently allocated for this tier; retry")
		default:
			return nil, s.internal(ctx, "create tier version", err)
		}
	}
	slog.InfoContext(ctx, "tier version created", logging.AttrComponent, "grpcserver", "version_id", id, "tier_id", tierID, "version", next)
	tv, err := s.versionProto(ctx, id)
	if err != nil {
		return nil, s.internal(ctx, "create tier version", err)
	}
	return &plansv1.CreateTierVersionResponse{Version: tv}, nil
}

func (s *planAdminService) UpdateTierVersionDraft(ctx context.Context, req *plansv1.UpdateTierVersionDraftRequest) (*plansv1.UpdateTierVersionDraftResponse, error) {
	id := strings.TrimSpace(req.GetId())
	if id == "" {
		return nil, status.Error(codes.InvalidArgument, "id is required")
	}
	ents := req.GetEntitlements().AsMap()
	if err := s.catalog.Validate(ents); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	prices, err := pricesFromProto(req.GetPrices())
	if err != nil {
		return nil, s.internal(ctx, "update tier version draft", err)
	}
	v := &models.TenantTierVersion{ID: id, Purchasable: req.GetPurchasable(), Entitlements: ents}
	if err := s.versions.UpdateDraft(ctx, v, prices); err != nil {
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return nil, status.Error(codes.NotFound, "tier version not found")
		case errors.Is(err, repository.ErrVersionNotDraft):
			return nil, status.Error(codes.FailedPrecondition, "only a draft version can be edited")
		default:
			return nil, s.internal(ctx, "update tier version draft", err)
		}
	}
	slog.InfoContext(ctx, "tier version draft updated", logging.AttrComponent, "grpcserver", "version_id", id)
	tv, err := s.versionProto(ctx, id)
	if err != nil {
		return nil, s.internal(ctx, "update tier version draft", err)
	}
	return &plansv1.UpdateTierVersionDraftResponse{Version: tv}, nil
}

func (s *planAdminService) PublishTierVersion(ctx context.Context, req *plansv1.PublishTierVersionRequest) (*plansv1.PublishTierVersionResponse, error) {
	id := strings.TrimSpace(req.GetId())
	if id == "" {
		return nil, status.Error(codes.InvalidArgument, "id is required")
	}
	changeReason := strings.TrimSpace(req.GetChangeReason())
	if changeReason == "" {
		return nil, status.Error(codes.InvalidArgument, "change_reason is required to publish")
	}
	if err := s.versions.Publish(ctx, id, changeReason, time.Now().UTC()); err != nil {
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return nil, status.Error(codes.NotFound, "tier version not found")
		case errors.Is(err, repository.ErrVersionNotDraft):
			return nil, status.Error(codes.FailedPrecondition, "only a draft version can be published")
		default:
			return nil, s.internal(ctx, "publish tier version", err)
		}
	}
	slog.InfoContext(ctx, "tier version published", logging.AttrComponent, "grpcserver", "version_id", id)
	tv, err := s.versionProto(ctx, id)
	if err != nil {
		return nil, s.internal(ctx, "publish tier version", err)
	}
	s.warnIfManyActiveVersions(ctx, tv.GetTierId())
	return &plansv1.PublishTierVersionResponse{Version: tv}, nil
}

func (s *planAdminService) RetireTierVersion(ctx context.Context, req *plansv1.RetireTierVersionRequest) (*plansv1.RetireTierVersionResponse, error) {
	id := strings.TrimSpace(req.GetId())
	if id == "" {
		return nil, status.Error(codes.InvalidArgument, "id is required")
	}
	force := req.GetForce()
	reassignTo := strings.TrimSpace(req.GetReassignToVersionId())
	if force && reassignTo != "" {
		return nil, status.Error(codes.InvalidArgument, "force and reassign_to_version_id are mutually exclusive")
	}
	if reassignTo != "" && reassignTo == id {
		return nil, status.Error(codes.InvalidArgument, "reassign_to_version_id must differ from the version being retired")
	}
	// Validate the reassignment target up front for clean error mapping; the repo
	// re-checks it under lock inside the transaction.
	if reassignTo != "" {
		target, err := s.versions.GetByID(ctx, reassignTo)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil, status.Error(codes.NotFound, "reassign_to_version_id not found")
			}
			return nil, s.internal(ctx, "retire tier version", err)
		}
		if target.Status != models.TierVersionStatusActive {
			return nil, status.Error(codes.FailedPrecondition, "reassign_to_version_id must be an active version")
		}
	}

	reassigned, err := s.versions.Retire(ctx, id, repository.RetireOptions{Force: force, ReassignToVersionID: reassignTo}, time.Now().UTC())
	if err != nil {
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return nil, status.Error(codes.NotFound, "tier version not found")
		case errors.Is(err, repository.ErrVersionNotActive):
			return nil, status.Error(codes.FailedPrecondition, "only an active version can be retired")
		case errors.Is(err, repository.ErrReassignTargetNotFound):
			return nil, status.Error(codes.NotFound, "reassign_to_version_id not found")
		case errors.Is(err, repository.ErrReassignTargetNotActive):
			return nil, status.Error(codes.FailedPrecondition, "reassign_to_version_id must be an active version")
		case errors.Is(err, repository.ErrReassignTargetIsSelf):
			return nil, status.Error(codes.InvalidArgument, "reassign_to_version_id must differ from the version being retired")
		case errors.Is(err, repository.ErrVersionHasLiveAssignments):
			return nil, s.liveAssignmentError(ctx, id)
		default:
			return nil, s.internal(ctx, "retire tier version", err)
		}
	}
	metricTierVersionsRetired.Add(1)
	if reassigned > 0 {
		metricTierVersionReassignments.Add(int64(reassigned))
	}
	slog.InfoContext(ctx, "tier version retired", logging.AttrComponent, "grpcserver",
		"version_id", id, "force", force, "reassign_to_version_id", reassignTo, "reassigned", reassigned)
	tv, err := s.versionProto(ctx, id)
	if err != nil {
		return nil, s.internal(ctx, "retire tier version", err)
	}
	s.warnIfManyActiveVersions(ctx, tv.GetTierId())
	return &plansv1.RetireTierVersionResponse{Version: tv, ReassignedCount: int32(reassigned)}, nil
}

func (s *planAdminService) DeleteTierVersion(ctx context.Context, req *plansv1.DeleteTierVersionRequest) (*plansv1.DeleteTierVersionResponse, error) {
	id := strings.TrimSpace(req.GetId())
	if id == "" {
		return nil, status.Error(codes.InvalidArgument, "id is required")
	}
	if err := s.versions.DeleteDraft(ctx, id); err != nil {
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return nil, status.Error(codes.NotFound, "tier version not found")
		case errors.Is(err, repository.ErrVersionNotDraft):
			return nil, status.Error(codes.FailedPrecondition, "only a draft version can be deleted")
		default:
			return nil, s.internal(ctx, "delete tier version", err)
		}
	}
	metricTierVersionsDeleted.Add(1)
	slog.InfoContext(ctx, "tier version deleted", logging.AttrComponent, "grpcserver", "version_id", id)
	return &plansv1.DeleteTierVersionResponse{}, nil
}

// --- assignment inspection ---

func (s *planAdminService) ListTierVersionAssignments(ctx context.Context, req *plansv1.ListTierVersionAssignmentsRequest) (*plansv1.ListTierVersionAssignmentsResponse, error) {
	versionID := strings.TrimSpace(req.GetTierVersionId())
	if versionID == "" {
		return nil, status.Error(codes.InvalidArgument, "tier_version_id is required")
	}
	limit := int(req.GetLimit())
	switch {
	case limit <= 0:
		limit = defaultAssignmentPageSize
	case limit > maxAssignmentPageSize:
		limit = maxAssignmentPageSize
	}
	offset := int(req.GetOffset())
	if offset < 0 {
		offset = 0
	}
	rows, err := s.versions.TenantsOnVersion(ctx, versionID, limit, offset)
	if err != nil {
		return nil, s.internal(ctx, "list tier version assignments", err)
	}
	total, err := s.versions.OpenAssignmentCount(ctx, versionID)
	if err != nil {
		return nil, s.internal(ctx, "list tier version assignments", err)
	}
	out := make([]*plansv1.VersionAssignment, 0, len(rows))
	for i := range rows {
		out = append(out, &plansv1.VersionAssignment{
			TenantId:   rows[i].TenantID,
			TenantName: rows[i].TenantName,
			ValidFrom:  timestamppb.New(rows[i].ValidFrom),
		})
	}
	return &plansv1.ListTierVersionAssignmentsResponse{Assignments: out, Total: int32(total)}, nil
}

// liveAssignmentError builds the FailedPrecondition returned when a retire is
// refused, naming a bounded sample of the blocking tenants so the operator can
// act without a second call.
func (s *planAdminService) liveAssignmentError(ctx context.Context, versionID string) error {
	const sampleSize = 10
	sample, err := s.versions.TenantsOnVersion(ctx, versionID, sampleSize, 0)
	if err != nil || len(sample) == 0 {
		return status.Error(codes.FailedPrecondition, "tier version still has live assignments; pass reassign_to_version_id to migrate them, or force to grandfather")
	}
	ids := make([]string, 0, len(sample))
	for i := range sample {
		ids = append(ids, sample[i].TenantID)
	}
	return status.Error(codes.FailedPrecondition, fmt.Sprintf(
		"tier version still has live assignments (%s); pass reassign_to_version_id to migrate them, or force to grandfather",
		strings.Join(ids, ", ")))
}

// warnIfManyActiveVersions logs when a tier carries more than a few concurrent
// active versions — grandfathering each carries a maintenance cost (CON-243 §10).
// Best-effort; a read error is swallowed.
func (s *planAdminService) warnIfManyActiveVersions(ctx context.Context, tierID string) {
	if tierID == "" {
		return
	}
	vers, err := s.versions.ListByTier(ctx, tierID)
	if err != nil {
		return
	}
	active := 0
	for i := range vers {
		if vers[i].Status == models.TierVersionStatusActive {
			active++
		}
	}
	if active > maxHealthyActiveVersions {
		slog.WarnContext(ctx, "tier has many concurrent active versions", logging.AttrComponent, "grpcserver",
			"tier_id", tierID, "active_versions", active)
	}
}

// --- tenant assignment ---

func (s *planAdminService) SetTenantTierVersion(ctx context.Context, req *plansv1.SetTenantTierVersionRequest) (*plansv1.SetTenantTierVersionResponse, error) {
	tenantID := strings.TrimSpace(req.GetTenantId())
	versionID := strings.TrimSpace(req.GetTierVersionId())
	if tenantID == "" || versionID == "" {
		return nil, status.Error(codes.InvalidArgument, "tenant_id and tier_version_id are required")
	}
	reason := strings.TrimSpace(req.GetReason())
	if reason == "" {
		reason = models.AssignmentReasonOperatorSet
	}
	if !assignableReason(reason) {
		return nil, status.Error(codes.InvalidArgument, "reason must be one of upgrade, downgrade, migration_accepted, grandfathered, operator_set")
	}
	v, err := s.versions.GetByID(ctx, versionID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, status.Error(codes.NotFound, "tier version not found")
		}
		return nil, s.internal(ctx, "set tenant tier version", err)
	}
	// Only an active version is assignable — a draft is not yet published and a
	// retired version must not gain new live assignments (the lifecycle is
	// draft -> active -> retired).
	if v.Status != models.TierVersionStatusActive {
		return nil, status.Errorf(codes.FailedPrecondition, "cannot assign a tenant to a %s version; only active versions are assignable", v.Status)
	}
	ok, err := s.assignments.Reassign(ctx, tenantID, v.TierID, &versionID, reason, time.Now().UTC())
	if err != nil {
		return nil, s.internal(ctx, "set tenant tier version", err)
	}
	if !ok {
		return nil, status.Error(codes.NotFound, "tenant not found")
	}
	slog.InfoContext(ctx, "tenant tier version set", logging.AttrComponent, "grpcserver", "tenant_id", tenantID, "version_id", versionID, "reason", reason)
	// CON-295 §4: nudge the tenant's open tabs to refetch their entitlements.
	publishEntitlementChange(ctx, s.hub, tenantID)
	tv, err := s.versionProto(ctx, versionID)
	if err != nil {
		return nil, s.internal(ctx, "set tenant tier version", err)
	}
	return &plansv1.SetTenantTierVersionResponse{TenantId: tenantID, Version: tv}, nil
}

// --- helpers ---

func (s *planAdminService) versionProto(ctx context.Context, id string) (*plansv1.TierVersion, error) {
	v, err := s.versions.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	prices, err := s.versions.PricesByVersion(ctx, id)
	if err != nil {
		return nil, err
	}
	count, err := s.versions.OpenAssignmentCount(ctx, id)
	if err != nil {
		return nil, err
	}
	return tierVersionProto(v, prices, count)
}

func (s *planAdminService) internal(ctx context.Context, op string, err error) error {
	slog.ErrorContext(ctx, "plan admin "+op, logging.AttrComponent, "grpcserver", logging.AttrError, err)
	return status.Error(codes.Internal, op+" failed")
}

// assignableReason reports whether reason is a valid operator-driven assignment
// reason. signup and migration_lapsed are system-generated and not accepted here.
func assignableReason(reason string) bool {
	switch reason {
	case models.AssignmentReasonUpgrade, models.AssignmentReasonDowngrade,
		models.AssignmentReasonMigrationAccepted, models.AssignmentReasonGrandfathered,
		models.AssignmentReasonOperatorSet:
		return true
	default:
		return false
	}
}

func tierVersionProto(v *models.TenantTierVersion, prices []models.TenantTierVersionPrice, liveCount int) (*plansv1.TierVersion, error) {
	ent, err := structpb.NewStruct(v.Entitlements)
	if err != nil {
		return nil, err
	}
	pp := make([]*plansv1.Price, 0, len(prices))
	for i := range prices {
		pp = append(pp, priceProto(&prices[i]))
	}
	out := &plansv1.TierVersion{
		Id:                  v.ID,
		TierId:              v.TierID,
		Version:             int32(v.Version),
		Status:              v.Status,
		Purchasable:         v.Purchasable,
		ChangeReason:        v.ChangeReason,
		Entitlements:        ent,
		Prices:              pp,
		LiveAssignmentCount: int32(liveCount),
		CreatedAt:           timestamppb.New(v.CreatedAt),
	}
	if v.PublishedAt != nil {
		out.PublishedAt = timestamppb.New(*v.PublishedAt)
	}
	if v.RetiredAt != nil {
		out.RetiredAt = timestamppb.New(*v.RetiredAt)
	}
	return out, nil
}

func priceProto(p *models.TenantTierVersionPrice) *plansv1.Price {
	cc := ""
	if p.CountryCode != nil {
		cc = *p.CountryCode
	}
	return &plansv1.Price{
		Currency:        p.Currency,
		BillingInterval: p.BillingInterval,
		NetMinor:        p.NetMinor,
		CountryCode:     cc,
	}
}

func featureProto(f entitlements.Feature) *plansv1.Feature {
	return &plansv1.Feature{
		Key:         f.Key,
		Name:        f.Name,
		Category:    f.Category,
		LinearIssue: f.LinearIssue,
		Status:      f.Status,
		ValueType:   f.ValueType,
		IsMaterial:  f.IsMaterial,
		Reset_:      f.Reset,
		Description: f.Description,
	}
}

// pricesFromProto builds price models from the proto input, minting an id for
// each. An empty country_code stays a nil (default) row.
func pricesFromProto(in []*plansv1.Price) ([]models.TenantTierVersionPrice, error) {
	out := make([]models.TenantTierVersionPrice, 0, len(in))
	for _, p := range in {
		id, err := models.NewID()
		if err != nil {
			return nil, err
		}
		var cc *string
		if c := strings.TrimSpace(p.GetCountryCode()); c != "" {
			cc = &c
		}
		out = append(out, models.TenantTierVersionPrice{
			ID:              id,
			Currency:        p.GetCurrency(),
			BillingInterval: p.GetBillingInterval(),
			NetMinor:        p.GetNetMinor(),
			CountryCode:     cc,
		})
	}
	return out, nil
}

// pricesFromModels clones price models onto fresh ids (for CreateTierVersion's
// clone_from_version_id path).
func pricesFromModels(in []models.TenantTierVersionPrice) ([]models.TenantTierVersionPrice, error) {
	out := make([]models.TenantTierVersionPrice, 0, len(in))
	for i := range in {
		id, err := models.NewID()
		if err != nil {
			return nil, err
		}
		p := in[i]
		p.ID = id
		p.TierVersionID = ""
		out = append(out, p)
	}
	return out, nil
}
