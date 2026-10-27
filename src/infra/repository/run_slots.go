package repository

import (
	"context"
	"time"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/kernel/tenantctx"
	"github.com/ogen-app/ogen/src/usecase/runslot"
)

// RunSlots keeps detached-run slots (runslot.Store) on the rows of one run
// table: the run_token, run_heartbeat_at, run_done_at, run_error and
// run_error_code columns. Every statement is scoped to the context's tenant.
type RunSlots struct {
	db    bun.IDB
	table string
}

// NewIdeaDistillRunSlots keeps the slots of idea_distill_runs.
func NewIdeaDistillRunSlots(db bun.IDB) *RunSlots {
	return &RunSlots{db: db, table: "idea_distill_runs"}
}

// NewBrandDistillRunSlots keeps the slots of brand_distill_runs.
func NewBrandDistillRunSlots(db bun.IDB) *RunSlots {
	return &RunSlots{db: db, table: "brand_distill_runs"}
}

var _ runslot.Store = (*RunSlots)(nil)

func (r *RunSlots) update(ctx context.Context, id string) (*bun.UpdateQuery, error) {
	tid, ok := tenantctx.From(ctx)
	if !ok {
		return nil, tenantctx.ErrNoTenant
	}
	return r.db.NewUpdate().Table(r.table).Where("id = ?", id).Where("tenant_id = ?", tid), nil
}

func (r *RunSlots) Claim(ctx context.Context, id, token string, staleBefore time.Time) (bool, error) {
	q, err := r.update(ctx, id)
	if err != nil {
		return false, err
	}
	res, err := q.
		Set("run_token = ?", token).
		Set("run_heartbeat_at = now()").
		Set("run_done_at = NULL").
		Set("run_error = ''").
		Set("run_error_code = ''").
		Where("run_token IS NULL OR run_done_at IS NOT NULL OR run_heartbeat_at < ?", staleBefore).
		Exec(ctx)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

func (r *RunSlots) Heartbeat(ctx context.Context, id, token string) error {
	q, err := r.update(ctx, id)
	if err != nil {
		return err
	}
	_, err = q.Set("run_heartbeat_at = now()").Where("run_token = ? AND run_done_at IS NULL", token).Exec(ctx)
	return err
}

func (r *RunSlots) Finish(ctx context.Context, id, token, errText, code string) error {
	q, err := r.update(ctx, id)
	if err != nil {
		return err
	}
	_, err = q.
		Set("run_done_at = now()").
		Set("run_error = ?", errText).
		Set("run_error_code = ?", code).
		Where("run_token = ?", token).
		Exec(ctx)
	return err
}

func (r *RunSlots) Read(ctx context.Context, id string) (runslot.Row, error) {
	tid, ok := tenantctx.From(ctx)
	if !ok {
		return runslot.Row{}, tenantctx.ErrNoTenant
	}
	var row runslot.Row
	err := r.db.NewSelect().Table(r.table).
		Column("run_token", "run_heartbeat_at", "run_done_at", "run_error", "run_error_code").
		Where("id = ?", id).Where("tenant_id = ?", tid).
		Scan(ctx, &row)
	return row, err
}
