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
// history. GLOBAL operator table. The tstzrange `valid` column has no
// native Go type, so writes go through a raw tstzrange(...) expression and reads
// project lower()/upper() into the model's scan-only ValidFrom/ValidTo.
type TenantTierAssignmentRepository interface {
	// CoveringAt returns the assignment whose validity range contains at, or
	// sql.ErrNoRows if the tenant has none then.
	CoveringAt(ctx context.Context, tenantID string, at time.Time) (*models.TenantTierAssignment, error)
	// Create appends an assignment with valid = [validFrom, validTo); a nil
	// validTo is open-ended (the tenant's current assignment).
	Create(ctx context.Context, a *models.TenantTierAssignment, validFrom time.Time, validTo *time.Time) error
	// HasOpen reports whether the tenant currently has an open-ended (upper
	// infinity) assignment.
	HasOpen(ctx context.Context, tenantID string) (bool, error)
	// LatestClosedUpper returns the greatest upper bound among the tenant's
	// closed assignment ranges, or nil if it has none.
	LatestClosedUpper(ctx context.Context, tenantID string) (*time.Time, error)
	// Reassign points a tenant at a tier and, when tierVersionID is non-nil, opens
	// a new assignment to that version — in ONE transaction: update
	// tenants.tier_id (FK-validated), close the tenant's current open assignment,
	// then open [at, infinity) on the new version. Returns false if the tenant
	// does not exist or is soft-deleted. Used by SetTenantTierVersion (an explicit
	// version) and SetTenantTier (the tier's latest active version, or nil).
	Reassign(ctx context.Context, tenantID, tierID string, tierVersionID *string, reason string, at time.Time) (bool, error)
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

func (r *tenantTierAssignmentRepository) HasOpen(ctx context.Context, tenantID string) (bool, error) {
	return r.db.NewSelect().Model((*models.TenantTierAssignment)(nil)).
		Where("tta.tenant_id = ?", tenantID).
		Where("upper_inf(tta.valid)").
		Exists(ctx)
}

func (r *tenantTierAssignmentRepository) LatestClosedUpper(ctx context.Context, tenantID string) (*time.Time, error) {
	var upper *time.Time
	err := r.db.NewSelect().Model((*models.TenantTierAssignment)(nil)).
		ColumnExpr("max(upper(tta.valid))").
		Where("tta.tenant_id = ?", tenantID).
		Where("NOT upper_inf(tta.valid)").
		Scan(ctx, &upper)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return upper, nil
}

func (r *tenantTierAssignmentRepository) Reassign(ctx context.Context, tenantID, tierID string, tierVersionID *string, reason string, at time.Time) (bool, error) {
	var noUpper *time.Time
	var found bool
	err := r.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		// Point the tenant at the tier; the FK validates the tier exists (a bad
		// tier surfaces as 23503, which the caller maps to FailedPrecondition).
		res, err := tx.NewUpdate().Model((*models.Tenant)(nil)).
			Set("tier_id = ?", tierID).
			Set("updated_at = ?", at).
			Where("id = ?", tenantID).
			Where("deleted_at IS NULL").
			Exec(ctx)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil // tenant not found / soft-deleted; found stays false
		}
		found = true
		// Swap the assignment ONLY when the tier has a version to assign: close
		// the tenant's current open range (the append-only trigger's one permitted
		// update) and open the new one. When there is no version to assign (a tier
		// with no active version), update tier_id but leave the existing assignment
		// untouched — never strip a tenant's entitlements or rewrite history.
		if tierVersionID != nil {
			if _, err := tx.NewUpdate().Model((*models.TenantTierAssignment)(nil)).
				Set("valid = tstzrange(lower(valid), ?, '[)')", at).
				Where("tenant_id = ?", tenantID).
				Where("upper_inf(valid)").
				Exec(ctx); err != nil {
				return err
			}
			id, err := models.NewID()
			if err != nil {
				return err
			}
			a := &models.TenantTierAssignment{ID: id, TenantID: tenantID, TierVersionID: *tierVersionID, Reason: reason, CreatedAt: at}
			if _, err := tx.NewInsert().Model(a).
				Value("valid", "tstzrange(?, ?, '[)')", at, noUpper).
				Exec(ctx); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	return found, nil
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
// version, with valid = [tenant.created_at, infinity) and reason 'signup'.
// Idempotent: a tenant that already has an open assignment is skipped, so it
// is safe on every boot. dryRun reports what would happen without writing.
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

	versions := newLatestTierVersions(NewTenantTierVersionRepository(db))
	assignmentRepo := NewTenantTierAssignmentRepository(db)

	for _, t := range tenants {
		versionID, err := versions.resolve(ctx, t.TierID)
		if err != nil {
			return report, err
		}
		if versionID == "" {
			report.SkippedNoVersion++
			continue
		}
		if dryRun {
			continue
		}
		id, err := models.NewID()
		if err != nil {
			return report, err
		}
		a := &models.TenantTierAssignment{
			ID:            id,
			TenantID:      t.ID,
			TierVersionID: versionID,
			Reason:        models.AssignmentReasonSignup,
			CreatedAt:     time.Now().UTC(),
		}
		assigned, err := createOpenAssignment(ctx, assignmentRepo, a, t.CreatedAt)
		if err != nil {
			return report, err
		}
		if assigned {
			report.Assigned++
		}
	}
	return report, nil
}

// latestTierVersions memoises LatestActiveByTier per tier; "" records a
// tier with no active version.
type latestTierVersions struct {
	repo   TenantTierVersionRepository
	byTier map[string]string
}

func newLatestTierVersions(repo TenantTierVersionRepository) *latestTierVersions {
	return &latestTierVersions{repo: repo, byTier: make(map[string]string)}
}

func (m *latestTierVersions) resolve(ctx context.Context, tierID string) (string, error) {
	if id, ok := m.byTier[tierID]; ok {
		return id, nil
	}
	v, err := m.repo.LatestActiveByTier(ctx, tierID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		m.byTier[tierID] = ""
	case err != nil:
		return "", err
	default:
		m.byTier[tierID] = v.ID
	}
	return m.byTier[tierID], nil
}

// createOpenAssignment writes a with valid = [from, infinity). It reports
// false without error when a concurrent writer already opened one.
func createOpenAssignment(ctx context.Context, repo TenantTierAssignmentRepository, a *models.TenantTierAssignment, from time.Time) (bool, error) {
	err := repo.Create(ctx, a, from, nil)
	if err == nil {
		return true, nil
	}
	if !isExclusionViolation(err) {
		return false, err
	}
	return resumeAfterClosedHistory(ctx, repo, a, err)
}

// resumeAfterClosedHistory recovers from an exclusion violation on an open
// range. If an open assignment now exists (a concurrent backfill won the
// race) there is nothing to do. Otherwise the overlap is with closed
// history, so the open range resumes at the end of the latest closed
// assignment, leaving the tenant with exactly one open assignment.
// createErr is returned when there is no closed range to resume from.
func resumeAfterClosedHistory(ctx context.Context, repo TenantTierAssignmentRepository, a *models.TenantTierAssignment, createErr error) (bool, error) {
	open, err := repo.HasOpen(ctx, a.TenantID)
	if err != nil || open {
		return false, err
	}
	end, err := repo.LatestClosedUpper(ctx, a.TenantID)
	if err != nil {
		return false, err
	}
	if end == nil {
		return false, createErr
	}
	if err := repo.Create(ctx, a, *end, nil); err != nil {
		return false, err
	}
	return true, nil
}

// isExclusionViolation reports whether err is a Postgres exclusion-constraint
// violation (SQLSTATE 23P01).
func isExclusionViolation(err error) bool {
	pgErr, ok := errors.AsType[*pgconn.PgError](err)
	return ok && pgErr.Code == "23P01"
}
