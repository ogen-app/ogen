package repository

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
)

// ErrBrandDistillOpen means the tenant already has a brand distillation run
// being worked on.
var ErrBrandDistillOpen = errors.New("a brand distillation run is already open")

// ErrBrandDistillClosed means a run being worked on was closed meanwhile, e.g.
// discarded, so the job's write did not land.
var ErrBrandDistillClosed = errors.New("the brand distillation run is no longer open")

const brandDistillOpenIndex = "brand_distill_runs_open_uq"

// BrandDistillRepository persists brand distillation runs and the ledger
// behind their monthly cap. Queries go through the bun Model API so
// TenantScoped scopes them; the expiry sweep is the one cross-tenant write.
type BrandDistillRepository interface {
	// Create inserts a run as given (status included); ErrBrandDistillOpen
	// when it would be a second open run of the tenant.
	Create(ctx context.Context, db bun.IDB, run *models.BrandDistillRun) error
	// Get returns sql.ErrNoRows for an absent run.
	Get(ctx context.Context, id string) (*models.BrandDistillRun, error)
	// Open returns the tenant's open run, sql.ErrNoRows when there is none.
	Open(ctx context.Context) (*models.BrandDistillRun, error)
	// List returns the tenant's latest runs, newest first, without their
	// candidates or proposal.
	List(ctx context.Context, limit int) ([]models.BrandDistillRun, error)
	// Lock reads a run FOR UPDATE inside tx.
	Lock(ctx context.Context, tx bun.Tx, id string) (*models.BrandDistillRun, error)
	// Save writes the named columns of run and stamps updated_at; a closed
	// status also stamps finished_at once.
	Save(ctx context.Context, db bun.IDB, run *models.BrandDistillRun, columns ...string) error
	// SaveOpen is Save for the job: it writes only while the stored run is
	// still open, and returns ErrBrandDistillClosed when it is not.
	SaveOpen(ctx context.Context, run *models.BrandDistillRun, columns ...string) error
	// ExpireReadyBefore marks every tenant's ready runs finished before cutoff
	// expired and drops their working state. It returns how many it expired.
	ExpireReadyBefore(ctx context.Context, cutoff time.Time) (int64, error)
	// StampSourceRun records runID on the brand-source assets among ids that
	// have none yet.
	StampSourceRun(ctx context.Context, runID string, assetIDs []string) error
	// RecordCharge logs one run against the monthly cap; ReleaseCharge hands
	// a run that never finished back to the allowance.
	RecordCharge(ctx context.Context, runID string) error
	ReleaseCharge(ctx context.Context, runID string) error
	// CountChargesSince counts the tenant's charges at or after since.
	CountChargesSince(ctx context.Context, since time.Time) (int64, error)
}

type brandDistillRepository struct {
	db *bun.DB
}

func NewBrandDistillRepository(db *bun.DB) BrandDistillRepository {
	return &brandDistillRepository{db: db}
}

func (r *brandDistillRepository) Create(ctx context.Context, db bun.IDB, run *models.BrandDistillRun) error {
	if run.ID == "" {
		id, err := models.NewID()
		if err != nil {
			return err
		}
		run.ID = id
	}
	now := time.Now().UTC()
	run.CreatedAt, run.UpdatedAt = now, now
	if run.Sources == nil {
		run.Sources = []models.DistillSource{}
	}
	if run.Answers == nil {
		run.Answers = []models.DistillAnswer{}
	}
	if run.ModelIDs == nil {
		run.ModelIDs = models.StringMap{}
	}
	if run.CommittedIDs == nil {
		run.CommittedIDs = models.StringMap{}
	}
	_, err := db.NewInsert().Model(run).Exec(ctx)
	if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok && pgErr.Code == "23505" && pgErr.ConstraintName == brandDistillOpenIndex {
		return ErrBrandDistillOpen
	}
	return err
}

func (r *brandDistillRepository) Get(ctx context.Context, id string) (*models.BrandDistillRun, error) {
	run := new(models.BrandDistillRun)
	if err := r.db.NewSelect().Model(run).Where("bdr.id = ?", id).Scan(ctx); err != nil {
		return nil, err
	}
	return run, nil
}

func (r *brandDistillRepository) Open(ctx context.Context) (*models.BrandDistillRun, error) {
	run := new(models.BrandDistillRun)
	err := r.db.NewSelect().
		Model(run).
		Where("bdr.status IN (?)", bun.List(models.BrandDistillOpenStatuses)).
		Limit(1).
		Scan(ctx)
	if err != nil {
		return nil, err
	}
	return run, nil
}

func (r *brandDistillRepository) List(ctx context.Context, limit int) ([]models.BrandDistillRun, error) {
	out := []models.BrandDistillRun{}
	err := r.db.NewSelect().
		Model(&out).
		ExcludeColumn("candidates", "proposal").
		OrderExpr("bdr.created_at DESC, bdr.id DESC").
		Limit(limit).
		Scan(ctx)
	return out, err
}

func (r *brandDistillRepository) Lock(ctx context.Context, tx bun.Tx, id string) (*models.BrandDistillRun, error) {
	run := new(models.BrandDistillRun)
	if err := tx.NewSelect().Model(run).Where("bdr.id = ?", id).For("UPDATE").Scan(ctx); err != nil {
		return nil, err
	}
	return run, nil
}

func (r *brandDistillRepository) Save(ctx context.Context, db bun.IDB, run *models.BrandDistillRun, columns ...string) error {
	_, err := update(db, run, columns).Exec(ctx)
	return err
}

func (r *brandDistillRepository) SaveOpen(ctx context.Context, run *models.BrandDistillRun, columns ...string) error {
	res, err := update(r.db, run, columns).
		Where("bdr.status IN (?)", bun.List(models.BrandDistillOpenStatuses)).
		Exec(ctx)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrBrandDistillClosed
	}
	return nil
}

// update writes the named columns and stamps updated_at; a closed status also
// stamps finished_at once.
func update(db bun.IDB, run *models.BrandDistillRun, columns []string) *bun.UpdateQuery {
	now := time.Now().UTC()
	run.UpdatedAt = now
	columns = append(columns, "updated_at")
	if !run.Status.Open() && run.FinishedAt == nil {
		run.FinishedAt = &now
		columns = append(columns, "finished_at")
	}
	return db.NewUpdate().Model(run).Column(columns...).WherePK()
}

// ExpireReadyBefore runs raw SQL: it crosses tenants on purpose and must not
// be scoped to one.
func (r *brandDistillRepository) ExpireReadyBefore(ctx context.Context, cutoff time.Time) (int64, error) {
	res, err := r.db.NewRaw(`UPDATE brand_distill_runs
		SET status = ?, candidates = NULL, proposal = NULL, updated_at = now()
		WHERE status = ? AND finished_at < ?`,
		models.BrandDistillStatusExpired, models.BrandDistillStatusReady, cutoff,
	).Exec(ctx)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

func (r *brandDistillRepository) StampSourceRun(ctx context.Context, runID string, assetIDs []string) error {
	if len(assetIDs) == 0 {
		return nil
	}
	_, err := r.db.NewUpdate().
		Model((*models.Asset)(nil)).
		Set("origin_ref = jsonb_set(coalesce(origin_ref, '{}'::jsonb), '{run_id}', to_jsonb(?::text))", runID).
		Where("id IN (?)", bun.List(assetIDs)).
		Where("origin = ?", models.AssetOriginBrandDistill).
		Where("coalesce(origin_ref->>'run_id', '') = ''").
		Exec(ctx)
	return err
}

func (r *brandDistillRepository) RecordCharge(ctx context.Context, runID string) error {
	id, err := models.NewID()
	if err != nil {
		return err
	}
	_, err = r.db.NewInsert().Model(&models.BrandDistillCharge{ID: id, RunID: runID}).Exec(ctx)
	return err
}

func (r *brandDistillRepository) ReleaseCharge(ctx context.Context, runID string) error {
	_, err := r.db.NewDelete().Model((*models.BrandDistillCharge)(nil)).Where("run_id = ?", runID).Exec(ctx)
	return err
}

// CountChargesSince scans a count(*) select: bun's Count skips the
// BeforeSelect hook, so it would count every tenant's charges.
func (r *brandDistillRepository) CountChargesSince(ctx context.Context, since time.Time) (int64, error) {
	var n int64
	err := r.db.NewSelect().
		Model((*models.BrandDistillCharge)(nil)).
		ColumnExpr("count(*)").
		Where("bdl.created_at >= ?", since).
		Scan(ctx, &n)
	return n, err
}
