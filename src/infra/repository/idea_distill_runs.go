package repository

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
)

// ErrDistillActive means the lens already has a distill run waiting or
// running.
var ErrDistillActive = errors.New("a distill run of this lens is already active")

const distillActiveIndex = "idea_distill_runs_active_uq"

// IdeaDistillRepository persists distill runs, the ideas they add and the
// ledger behind their monthly cap. Every query runs through the bun Model API
// so TenantScoped scopes it.
type IdeaDistillRepository interface {
	// Create inserts a pending run; ErrDistillActive when the lens already
	// has an active one.
	Create(ctx context.Context, run *models.IdeaDistillRun) error
	// Get returns sql.ErrNoRows for an absent run.
	Get(ctx context.Context, id string) (*models.IdeaDistillRun, error)
	// Active returns the lens's waiting or running run, sql.ErrNoRows when
	// there is none.
	Active(ctx context.Context, analysisID string) (*models.IdeaDistillRun, error)
	// ListByAnalysis returns the lens's latest runs, newest first.
	ListByAnalysis(ctx context.Context, analysisID string, limit int) ([]models.IdeaDistillRun, error)
	// SetStatus writes status with its failure code and reason; a final
	// status stamps finished_at.
	SetStatus(ctx context.Context, id string, status models.IdeaDistillStatus, code, reason string) error
	// SaveIdeas marks the run ready and inserts its ideas. It must run inside
	// the tx that locked the lens.
	SaveIdeas(ctx context.Context, tx bun.Tx, run *models.IdeaDistillRun, ideas []models.Idea) error
	// RecordCharge logs one run against the monthly cap and returns the
	// charge's id; DeleteCharge releases a charge whose run never finished.
	RecordCharge(ctx context.Context, runID string) (string, error)
	DeleteCharge(ctx context.Context, id string) error
	// CountChargesSince counts the tenant's charges at or after since.
	CountChargesSince(ctx context.Context, since time.Time) (int64, error)
}

type ideaDistillRepository struct {
	db *bun.DB
}

func NewIdeaDistillRepository(db *bun.DB) IdeaDistillRepository {
	return &ideaDistillRepository{db: db}
}

func (r *ideaDistillRepository) Create(ctx context.Context, run *models.IdeaDistillRun) error {
	if run.ID == "" {
		id, err := models.NewID()
		if err != nil {
			return err
		}
		run.ID = id
	}
	now := time.Now().UTC()
	run.Status, run.CreatedAt, run.UpdatedAt = models.IdeaDistillStatusPending, now, now
	_, err := r.db.NewInsert().Model(run).Exec(ctx)
	if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok && pgErr.Code == "23505" && pgErr.ConstraintName == distillActiveIndex {
		return ErrDistillActive
	}
	return err
}

func (r *ideaDistillRepository) Get(ctx context.Context, id string) (*models.IdeaDistillRun, error) {
	run := new(models.IdeaDistillRun)
	if err := r.db.NewSelect().Model(run).Where("idr.id = ?", id).Scan(ctx); err != nil {
		return nil, err
	}
	return run, nil
}

func (r *ideaDistillRepository) Active(ctx context.Context, analysisID string) (*models.IdeaDistillRun, error) {
	run := new(models.IdeaDistillRun)
	err := r.db.NewSelect().
		Model(run).
		Where("idr.analysis_id = ?", analysisID).
		Where("idr.status IN (?)", bun.List([]models.IdeaDistillStatus{models.IdeaDistillStatusPending, models.IdeaDistillStatusRunning})).
		Scan(ctx)
	if err != nil {
		return nil, err
	}
	return run, nil
}

func (r *ideaDistillRepository) ListByAnalysis(ctx context.Context, analysisID string, limit int) ([]models.IdeaDistillRun, error) {
	out := []models.IdeaDistillRun{}
	err := r.db.NewSelect().
		Model(&out).
		Where("idr.analysis_id = ?", analysisID).
		OrderExpr("idr.created_at DESC, idr.id DESC").
		Limit(limit).
		Scan(ctx)
	return out, err
}

func (r *ideaDistillRepository) SetStatus(ctx context.Context, id string, status models.IdeaDistillStatus, code, reason string) error {
	now := time.Now().UTC()
	q := r.db.NewUpdate().
		Model((*models.IdeaDistillRun)(nil)).
		Set("status = ?", status).
		Set("failure_code = ?", code).
		Set("failure_reason = ?", reason).
		Set("updated_at = ?", now).
		Where("id = ?", id)
	if !status.Active() {
		q = q.Set("finished_at = ?", now)
	}
	_, err := q.Exec(ctx)
	return err
}

func (r *ideaDistillRepository) SaveIdeas(ctx context.Context, tx bun.Tx, run *models.IdeaDistillRun, ideas []models.Idea) error {
	now := time.Now().UTC()
	run.Status, run.IdeasCount, run.FailureCode, run.FailureReason = models.IdeaDistillStatusReady, len(ideas), "", ""
	run.UpdatedAt, run.FinishedAt = now, &now
	if _, err := tx.NewUpdate().
		Model(run).
		Column("status", "ideas_count", "model_id", "failure_code", "failure_reason", "updated_at", "finished_at").
		WherePK().
		Exec(ctx); err != nil {
		return err
	}
	if len(ideas) == 0 {
		return nil
	}
	for i := range ideas {
		// One insert would give every idea the same created_at, leaving their
		// order to the random ids; a microsecond apart keeps the flow's order.
		if ideas[i].CreatedAt.IsZero() {
			ideas[i].CreatedAt = now.Add(time.Duration(i) * time.Microsecond)
		}
		ideas[i].DistillRunID = new(run.ID)
		ideas[i].AssetAnalysisID = new(run.AnalysisID)
		ideas[i].SourceAssetID = new(run.AssetID)
	}
	_, err := tx.NewInsert().Model(&ideas).Exec(ctx)
	return err
}

func (r *ideaDistillRepository) RecordCharge(ctx context.Context, runID string) (string, error) {
	id, err := models.NewID()
	if err != nil {
		return "", err
	}
	if _, err := r.db.NewInsert().Model(&models.IdeaDistillCharge{ID: id, RunID: runID}).Exec(ctx); err != nil {
		return "", err
	}
	return id, nil
}

func (r *ideaDistillRepository) DeleteCharge(ctx context.Context, id string) error {
	_, err := r.db.NewDelete().Model((*models.IdeaDistillCharge)(nil)).Where("id = ?", id).Exec(ctx)
	return err
}

// CountChargesSince scans a count(*) select: bun's Count skips the
// BeforeSelect hook, so it would count every tenant's charges.
func (r *ideaDistillRepository) CountChargesSince(ctx context.Context, since time.Time) (int64, error) {
	var n int64
	err := r.db.NewSelect().
		Model((*models.IdeaDistillCharge)(nil)).
		ColumnExpr("count(*)").
		Where("idl.created_at >= ?", since).
		Scan(ctx, &n)
	return n, err
}
