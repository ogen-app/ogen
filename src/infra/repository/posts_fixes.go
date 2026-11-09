package repository

import (
	"context"
	"time"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
)

// ScheduleChange sets one post's scheduled_at from From to To; nil is no
// date.
type ScheduleChange struct {
	PostID string
	From   *time.Time
	To     *time.Time
}

// scheduleChangeRow is a ScheduleChange as a VALUES row.
type scheduleChangeRow struct {
	ID     string     `bun:"id"`
	FromAt *time.Time `bun:"from_at,type:timestamptz"`
	ToAt   *time.Time `bun:"to_at,type:timestamptz"`
}

// movable are the statuses whose date is Ogen's alone: nothing is queued in
// Zernio for them.
var movable = []models.PostStatus{models.PostStatusPlanned, models.PostStatusDraft, models.PostStatusReadyForPublish}

func (r *postRepository) SetScheduleIf(ctx context.Context, db bun.IDB, changes []ScheduleChange, now time.Time) ([]string, error) {
	set := []string{}
	if len(changes) == 0 {
		return set, nil
	}
	if db == nil {
		db = r.db
	}
	rows := make([]scheduleChangeRow, len(changes))
	for i, c := range changes {
		rows[i] = scheduleChangeRow{ID: c.PostID, FromAt: c.From, ToAt: c.To}
	}
	// The TenantScoped hook adds the tenant predicate.
	_, err := db.NewUpdate().
		With("_data", db.NewValues(&rows)).
		Model((*models.Post)(nil)).
		TableExpr("_data").
		Set("scheduled_at = _data.to_at").
		Set("updated_at = ?", now).
		Where("po.id = _data.id").
		Where("po.scheduled_at IS NOT DISTINCT FROM _data.from_at").
		Where("po.status IN (?)", bun.List(movable)).
		Returning("po.id").
		Exec(ctx, &set)
	return set, err
}

// PostTypeChange changes one post's post type and status.
type PostTypeChange struct {
	PostID     string
	From, To   string
	FromStatus models.PostStatus
	ToStatus   models.PostStatus
}

// postTypeRow is a PostTypeChange as a VALUES row.
type postTypeRow struct {
	ID         string `bun:"id"`
	FromType   string `bun:"from_type"`
	ToType     string `bun:"to_type"`
	FromStatus string `bun:"from_status"`
	ToStatus   string `bun:"to_status"`
}

func (r *postRepository) SetPostTypeIf(ctx context.Context, db bun.IDB, changes []PostTypeChange, now time.Time) ([]string, error) {
	set := []string{}
	if len(changes) == 0 {
		return set, nil
	}
	if db == nil {
		db = r.db
	}
	rows := make([]postTypeRow, len(changes))
	for i, c := range changes {
		rows[i] = postTypeRow{ID: c.PostID, FromType: c.From, ToType: c.To, FromStatus: string(c.FromStatus), ToStatus: string(c.ToStatus)}
	}
	// Only a post whose content Ogen alone holds changes type: a scheduled
	// or published one has a copy outside it.
	_, err := db.NewUpdate().
		With("_data", db.NewValues(&rows)).
		Model((*models.Post)(nil)).
		TableExpr("_data").
		Set("platform_post_type = _data.to_type").
		Set("status = _data.to_status").
		Set("updated_at = ?", now).
		Where("po.id = _data.id").
		Where("coalesce(po.platform_post_type, '') = _data.from_type").
		Where("po.status = _data.from_status").
		Where("po.status IN (?)", bun.List(movable)).
		Returning("po.id").
		Exec(ctx, &set)
	return set, err
}
