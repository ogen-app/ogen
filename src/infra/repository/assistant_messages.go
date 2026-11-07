package repository

import (
	"context"
	"slices"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
)

// AssistantMessageRepository stores universal-assistant threads, one per
// scope.
type AssistantMessageRepository interface {
	// CreateBatch inserts a turn's messages all-or-nothing, so a turn never
	// persists half-written.
	CreateBatch(ctx context.Context, msgs []*models.AssistantMessage) error
	// ListRecent returns a thread's last limit messages, oldest first.
	ListRecent(ctx context.Context, kind models.AssistantScopeKind, scopeID string, limit int) ([]models.AssistantMessage, error)
}

type assistantMessageRepository struct {
	db *bun.DB
}

// NewAssistantMessageRepository returns the bun-backed AssistantMessageRepository.
func NewAssistantMessageRepository(db *bun.DB) AssistantMessageRepository {
	return &assistantMessageRepository{db: db}
}

// CreateBatch inserts msgs in one statement inside a transaction, in slice
// order, so seq follows the batch order.
func (r *assistantMessageRepository) CreateBatch(ctx context.Context, msgs []*models.AssistantMessage) error {
	if len(msgs) == 0 {
		return nil
	}
	return r.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		_, err := tx.NewInsert().Model(&msgs).Exec(ctx)
		return err
	})
}

// ListRecent returns the thread's last limit messages, oldest first, and none
// for a limit below one. Messages that share created_at keep insertion order.
func (r *assistantMessageRepository) ListRecent(ctx context.Context, kind models.AssistantScopeKind, scopeID string, limit int) ([]models.AssistantMessage, error) {
	if limit <= 0 {
		// bun drops a zero LIMIT, which would load the whole thread.
		return nil, nil
	}
	var msgs []models.AssistantMessage
	err := r.db.NewSelect().Model(&msgs).
		Where("asm.scope_kind = ? AND asm.scope_id = ?", kind, scopeID).
		// seq breaks created_at ties in insertion order.
		OrderExpr("asm.created_at DESC, asm.seq DESC").
		Limit(limit).
		Scan(ctx)
	if err != nil {
		return nil, err
	}
	slices.Reverse(msgs)
	return msgs, nil
}
