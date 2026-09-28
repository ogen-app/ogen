package repository

import (
	"context"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
)

// CampaignAssistantMessageRepository handles persistence of per-campaign
// conversation history between the user and the Campaign Assistant.
type CampaignAssistantMessageRepository interface {
	Create(ctx context.Context, msg *models.CampaignAssistantMessage) error
	// CreateBatch inserts several messages atomically (all-or-nothing) within a
	// single transaction, so a conversation turn never persists half-written.
	CreateBatch(ctx context.Context, msgs []*models.CampaignAssistantMessage) error
	ListRecentByCampaignID(ctx context.Context, campaignID string, limit int) ([]models.CampaignAssistantMessage, error)
}

type campaignAssistantMessageRepository struct {
	db *bun.DB
}

func NewCampaignAssistantMessageRepository(db *bun.DB) CampaignAssistantMessageRepository {
	return &campaignAssistantMessageRepository{db: db}
}

func (r *campaignAssistantMessageRepository) Create(ctx context.Context, msg *models.CampaignAssistantMessage) error {
	_, err := r.db.NewInsert().Model(msg).Exec(ctx)
	return err
}

func (r *campaignAssistantMessageRepository) CreateBatch(ctx context.Context, msgs []*models.CampaignAssistantMessage) error {
	if len(msgs) == 0 {
		return nil
	}
	return r.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		_, err := tx.NewInsert().Model(&msgs).Exec(ctx)
		return err
	})
}

func (r *campaignAssistantMessageRepository) ListRecentByCampaignID(ctx context.Context, campaignID string, limit int) ([]models.CampaignAssistantMessage, error) {
	var msgs []models.CampaignAssistantMessage
	err := r.db.NewSelect().
		Model(&msgs).
		Where("cam.campaign_id = ?", campaignID).
		// id is a random Sqid, not temporal — a deterministic tiebreak only;
		// Postgres timestamps are microsecond-resolution and each message is
		// inserted separately, so real ties don't occur.
		OrderExpr("cam.created_at DESC, cam.id DESC").
		Limit(limit).
		Scan(ctx)
	if err != nil {
		return nil, err
	}
	// Reverse to chronological order.
	for i, j := 0, len(msgs)-1; i < j; i, j = i+1, j-1 {
		msgs[i], msgs[j] = msgs[j], msgs[i]
	}
	return msgs, nil
}
