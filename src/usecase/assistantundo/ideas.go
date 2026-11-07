package assistantundo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
)

// IdeaVerdict is an idea's triage state, as an undo log stores it.
func IdeaVerdict(i *models.Idea) models.JSONMap {
	m := models.JSONMap{"verdict": nil, "remindAt": timeValue(i.RemindAt)}
	if i.Verdict != nil {
		m["verdict"] = string(*i.Verdict)
	}
	return m
}

// CampaignAssets is the assets a campaign draws on, as an undo log stores
// them.
func CampaignAssets(c *models.Campaign) models.JSONMap {
	ids := make([]any, len(c.AssetIDs))
	for i, id := range c.AssetIDs {
		ids[i] = id
	}
	return models.JSONMap{"assetIds": ids}
}

// WithIdeas lets s undo idea verdict changes.
func (s *Service) WithIdeas(ideas repository.IdeaRepository) *Service {
	s.undoers[models.AssistantActionIdeaVerdict] = ideaVerdict{ideas}
	return s
}

type ideaVerdict struct{ ideas repository.IdeaRepository }

// Undo restores the idea's verdict. The idea repository takes no
// transaction, so its write commits on its own, ahead of the undo record.
func (u ideaVerdict) Undo(ctx context.Context, _ bun.IDB, a *models.AssistantAction) error {
	i, err := u.ideas.GetByID(ctx, a.EntityID)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrChangedSince
	}
	if err != nil {
		return fmt.Errorf("load idea: %w", err)
	}
	if !same(IdeaVerdict(i), a.After) {
		return ErrChangedSince
	}
	i.Verdict, i.DecidedAt, i.DecidedBy = nil, nil, nil
	if v, ok := a.Before["verdict"].(string); ok && v != "" {
		verdict := models.IdeaVerdict(v)
		now := time.Now().UTC().Truncate(time.Microsecond)
		i.Verdict, i.DecidedAt, i.DecidedBy = &verdict, &now, &a.CreatedBy
	}
	if i.RemindAt, err = parseTime(a.Before["remindAt"]); err != nil {
		return fmt.Errorf("restore remind_at: %w", err)
	}
	i.UpdatedAt = time.Now().UTC()
	ok, err := u.ideas.Update(ctx, i, "verdict", "remind_at", "decided_at", "decided_by")
	if err != nil {
		return err
	}
	if !ok {
		return ErrChangedSince
	}
	return nil
}

type campaignAssets struct{ campaigns repository.CampaignRepository }

// Undo restores the assets the campaign draws on.
func (u campaignAssets) Undo(ctx context.Context, tx bun.IDB, a *models.AssistantAction) error {
	c, err := lockCampaign(ctx, u.campaigns, tx, a.EntityID)
	if err != nil {
		return err
	}
	if !same(CampaignAssets(c), a.After) {
		return ErrChangedSince
	}
	before, _ := a.Before["assetIds"].([]any)
	ids := models.StringSlice{}
	for _, v := range before {
		if s, ok := v.(string); ok && !slices.Contains(ids, s) {
			ids = append(ids, s)
		}
	}
	c.AssetIDs = ids
	return u.campaigns.UpdateColumns(ctx, tx, c, "asset_ids")
}
