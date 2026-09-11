package repository

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
)

// TenantTierAssignmentRepository is the append-only tenant -> tier-version
// history (CON-243). GLOBAL operator table. The tstzrange `valid` column has no
// native Go type, so writes go through a raw tstzrange(...) expression and reads
// project lower()/upper() into the model's scan-only ValidFrom/ValidTo.
type TenantTierAssignmentRepository interface {
	// CoveringAt returns the assignment whose validity range contains at, or
	// sql.ErrNoRows if the tenant has none then.
	CoveringAt(ctx context.Context, tenantID string, at time.Time) (*models.TenantTierAssignment, error)
	// Create appends an assignment with valid = [validFrom, validTo); a nil
	// validTo is open-ended (the tenant's current assignment).
	Create(ctx context.Context, a *models.TenantTierAssignment, validFrom time.Time, validTo *time.Time) error
}

type tenantTierAssignmentRepository struct {
	db *bun.DB
}

// NewTenantTierAssignmentRepository returns a Bun-backed repository.
func NewTenantTierAssignmentRepository(db *bun.DB) TenantTierAssignmentRepository {
	return &tenantTierAssignmentRepository{db: db}
}

func (r *tenantTierAssignmentRepository) CoveringAt(ctx context.Context, tenantID string, at time.Time) (*models.TenantTierAssignment, error) {
	a := new(models.TenantTierAssignment)
	err := r.db.NewSelect().Model(a).
		ColumnExpr("tta.id, tta.tenant_id, tta.tier_version_id, tta.reason, tta.notice_id, tta.created_at").
		ColumnExpr("lower(tta.valid) AS valid_from").
		ColumnExpr("upper(tta.valid) AS valid_to").
		Where("tta.tenant_id = ?", tenantID).
		Where("tta.valid @> ?::timestamptz", at).
		Limit(1).
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, sql.ErrNoRows
		}
		return nil, err
	}
	return a, nil
}

func (r *tenantTierAssignmentRepository) Create(ctx context.Context, a *models.TenantTierAssignment, validFrom time.Time, validTo *time.Time) error {
	_, err := r.db.NewInsert().Model(a).
		Value("valid", "tstzrange(?, ?, '[)')", validFrom, validTo).
		Exec(ctx)
	return err
}

// TierAssignmentBackfillReport summarises a BackfillTenantTierAssignments run.
type TierAssignmentBackfillReport struct {
	Candidates       int  `json:"candidates"`         // tenants lacking an open assignment
	Assigned         int  `json:"assigned"`           // assignments written (0 on a dry run)
	SkippedNoVersion int  `json:"skipped_no_version"` // tenants whose tier has no active version
	DryRun           bool `json:"dry_run"`
}

// BackfillTenantTierAssignments assigns every tenant that lacks an open
// (upper-infinity) tier-version assignment to its current tier's latest active
// version, with valid = [tenant.created_at, infinity) and reason 'signup'
// (CON-243 §13 Phase 4). Idempotent: a tenant that already has an open
// assignment is skipped, so it is safe on every boot. dryRun reports what would
// happen without writing.
func BackfillTenantTierAssignments(ctx context.Context, db *bun.DB, dryRun bool) (TierAssignmentBackfillReport, error) {
	ctx = tenantctx.WithSystem(ctx)
	report := TierAssignmentBackfillReport{DryRun: dryRun}

	var tenants []models.Tenant
	err := db.NewSelect().Model(&tenants).
		Column("id", "tier_id", "created_at").
		Where("tn.deleted_at IS NULL").
		Where("NOT EXISTS (SELECT 1 FROM tenant_tier_assignments a WHERE a.tenant_id = tn.id AND upper_inf(a.valid))").
		Scan(ctx)
	if err != nil {
		return report, err
	}
	report.Candidates = len(tenants)

	versionRepo := NewTenantTierVersionRepository(db)
	assignmentRepo := NewTenantTierAssignmentRepository(db)
	latestByTier := make(map[string]string) // tier_id -> version id ("" = no active version)

	resolveVersion := func(tierID string) (string, error) {
		if id, ok := latestByTier[tierID]; ok {
			return id, nil
		}
		v, verr := versionRepo.LatestActiveByTier(ctx, tierID)
		switch {
		case errors.Is(verr, sql.ErrNoRows):
			latestByTier[tierID] = ""
		case verr != nil:
			return "", verr
		default:
			latestByTier[tierID] = v.ID
		}
		return latestByTier[tierID], nil
	}

	for _, t := range tenants {
		versionID, verr := resolveVersion(t.TierID)
		if verr != nil {
			return report, verr
		}
		if versionID == "" {
			report.SkippedNoVersion++
			continue
		}
		if dryRun {
			continue
		}
		id, ierr := models.NewID()
		if ierr != nil {
			return report, ierr
		}
		a := &models.TenantTierAssignment{
			ID:            id,
			TenantID:      t.ID,
			TierVersionID: versionID,
			Reason:        models.AssignmentReasonSignup,
			CreatedAt:     time.Now().UTC(),
		}
		if cerr := assignmentRepo.Create(ctx, a, t.CreatedAt, nil); cerr != nil {
			// A concurrent backfill (rolling deploy) may have inserted the open
			// assignment first; the gist exclusion (23P01) rejects the overlap.
			// Treat that as already-done and stay idempotent.
			if isExclusionViolation(cerr) {
				continue
			}
			return report, cerr
		}
		report.Assigned++
	}
	return report, nil
}

// isExclusionViolation reports whether err is a Postgres exclusion-constraint
// violation (SQLSTATE 23P01).
func isExclusionViolation(err error) bool {
	pgErr, ok := errors.AsType[*pgconn.PgError](err)
	return ok && pgErr.Code == "23P01"
}
