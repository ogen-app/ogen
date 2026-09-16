package repository

import (
	"context"
	"time"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
)

// EmailEventRepository is the append-only Resend delivery/open/click timeline
// (CON-298), plus the denormalised rollup it maintains on email_logs.
type EmailEventRepository interface {
	// Record persists one webhook event and refreshes the parent log's rollup in
	// a single transaction. Ingestion is idempotent: a redelivery of the same
	// event (same svix_id) is a no-op, and because the rollup is RECOMPUTED from
	// the full event set rather than incremented, out-of-order and redelivered
	// events converge to the same result. The caller resolves EmailLogID from the
	// provider message id first (an unknown id is dropped before calling Record).
	Record(ctx context.Context, ev *models.EmailEvent) error
	// ListByEmailLogID returns one log's events oldest-first (the detail timeline).
	ListByEmailLogID(ctx context.Context, emailLogID string) ([]models.EmailEvent, error)
}

type emailEventRepository struct {
	db *bun.DB
}

// NewEmailEventRepository returns a Bun-backed EmailEventRepository.
func NewEmailEventRepository(db *bun.DB) EmailEventRepository {
	return &emailEventRepository{db: db}
}

func (r *emailEventRepository) Record(ctx context.Context, ev *models.EmailEvent) error {
	return r.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		// Dedupe redeliveries by the globally-unique Svix message id.
		if _, err := tx.NewInsert().
			Model(ev).
			On("CONFLICT (svix_id) DO NOTHING").
			Exec(ctx); err != nil {
			return err
		}
		return recomputeEmailRollup(ctx, tx, ev.EmailLogID)
	})
}

func (r *emailEventRepository) ListByEmailLogID(ctx context.Context, emailLogID string) ([]models.EmailEvent, error) {
	var events []models.EmailEvent
	if emailLogID == "" {
		return events, nil
	}
	err := r.db.NewSelect().
		Model(&events).
		Where("email_log_id = ?", emailLogID).
		Order("occurred_at ASC", "created_at ASC").
		Scan(ctx)
	return events, err
}

// recomputeEmailRollup rebuilds the denormalised engagement columns on an
// email_logs row from the authoritative email_events set, then advances the
// status without ever regressing it (a late "delivered" can't overwrite a
// "bounced"; a complaint that lands after an open still wins). Recomputing from
// source — rather than incrementing — is what makes redelivery and out-of-order
// ingestion safe.
func recomputeEmailRollup(ctx context.Context, tx bun.Tx, emailLogID string) error {
	var events []models.EmailEvent
	if err := tx.NewSelect().
		Model(&events).
		Where("email_log_id = ?", emailLogID).
		Order("occurred_at ASC").
		Scan(ctx); err != nil {
		return err
	}
	if len(events) == 0 {
		return nil
	}

	var (
		opens, clicks              int
		deliveredAt, firstOpenedAt time.Time
		lastEvent                  models.EmailEventType
		lastEventAt                time.Time
		implied                    models.EmailLogStatus
	)
	for i := range events {
		e := events[i]
		switch e.Type {
		case models.EmailEventOpened:
			opens++
			if firstOpenedAt.IsZero() || e.OccurredAt.Before(firstOpenedAt) {
				firstOpenedAt = e.OccurredAt
			}
		case models.EmailEventClicked:
			clicks++
		case models.EmailEventDelivered:
			if deliveredAt.IsZero() || e.OccurredAt.Before(deliveredAt) {
				deliveredAt = e.OccurredAt
			}
		}
		// last_event tracks the most recent event of ANY type (incl.
		// delivery_delayed), so operators see the true latest activity.
		if e.OccurredAt.After(lastEventAt) {
			lastEventAt = e.OccurredAt
			lastEvent = e.Type
		}
		if st := models.StatusForEvent(e.Type); st.Rank() > implied.Rank() {
			implied = st
		}
	}

	// Advance status only when the event-implied state out-ranks the current one.
	var cur models.EmailLog
	if err := tx.NewSelect().
		Model(&cur).
		Column("status").
		Where("id = ?", emailLogID).
		Scan(ctx); err != nil {
		return err
	}
	newStatus := cur.Status
	if implied != "" && implied.Rank() > cur.Status.Rank() {
		newStatus = implied
	}

	q := tx.NewUpdate().
		Model((*models.EmailLog)(nil)).
		Set("opens_count = ?", opens).
		Set("clicks_count = ?", clicks).
		Set("last_event = ?", lastEvent).
		Set("last_event_at = ?", lastEventAt).
		Set("status = ?", newStatus).
		Set("updated_at = now()").
		Where("id = ?", emailLogID)
	// First-occurrence timestamps are left NULL until the event exists.
	if !deliveredAt.IsZero() {
		q = q.Set("delivered_at = ?", deliveredAt)
	}
	if !firstOpenedAt.IsZero() {
		q = q.Set("first_opened_at = ?", firstOpenedAt)
	}
	_, err := q.Exec(ctx)
	return err
}
