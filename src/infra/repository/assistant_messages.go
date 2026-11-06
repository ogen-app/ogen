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

func NewAssistantMessageRepository(db *bun.DB) AssistantMessageRepository {
	return &assistantMessageRepository{db: db}
}

func (r *assistantMessageRepository) CreateBatch(ctx context.Context, msgs []*models.AssistantMessage) error {
	if len(msgs) == 0 {
		return nil
	}
	return r.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		_, err := tx.NewInsert().Model(&msgs).Exec(ctx)
		return err
	})
}

func (r *assistantMessageRepository) ListRecent(ctx context.Context, kind models.AssistantScopeKind, scopeID string, limit int) ([]models.AssistantMessage, error) {
	var msgs []models.AssistantMessage
	err := r.db.NewSelect().Model(&msgs).
		Where("asm.scope_kind = ? AND asm.scope_id = ?", kind, scopeID).
		// id is random, not temporal: a deterministic tiebreak only.
		OrderExpr("asm.created_at DESC, asm.id DESC").
		Limit(limit).
		Scan(ctx)
	if err != nil {
		return nil, err
	}
	slices.Reverse(msgs)
	return msgs, nil
}
