package assistantundo

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/campaignphase"
	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
)

// CampaignDates is the snapshot a dates change is logged and compared with.
func CampaignDates(c *models.Campaign) models.JSONMap {
	return models.JSONMap{"startDate": timeValue(c.StartDate), "endDate": timeValue(c.EndDate)}
}

// CampaignBrief is the snapshot a brief change is logged and compared with.
func CampaignBrief(c *models.Campaign) models.JSONMap {
	return models.JSONMap{
		"description":    c.Description,
		"targetPersona":  c.TargetPersona,
		"keyMessages":    c.KeyMessages,
		"toneGuidelines": c.ToneGuidelines,
	}
}

// CampaignPlatforms is the snapshot a platforms change is logged and compared
// with: the target platforms as they read back from JSON, so a logged snapshot
// and a fresh one compare equal.
func CampaignPlatforms(c *models.Campaign) models.JSONMap {
	var v any
	if b, err := json.Marshal(c.TargetPlatforms); err == nil {
		_ = json.Unmarshal(b, &v)
	}
	return models.JSONMap{"platforms": v}
}

// CampaignGoal is the snapshot a goal change is logged and compared with:
// the posts per period, nil without a goal, and the period.
func CampaignGoal(c *models.Campaign) models.JSONMap {
	var perPeriod any
	if c.EstimatedPostCount != nil {
		perPeriod = *c.EstimatedPostCount
	}
	return models.JSONMap{"perPeriod": perPeriod, "cadence": c.GoalCadence}
}

type campaignGoal struct{ campaigns repository.CampaignRepository }

// Undo puts the campaign's goal back while it is still as the change left it.
func (u campaignGoal) Undo(ctx context.Context, tx bun.IDB, a *models.AssistantAction) error {
	c, err := lockCampaign(ctx, u.campaigns, tx, a.EntityID)
	if err != nil {
		return err
	}
	if !same(CampaignGoal(c), a.After) {
		return ErrChangedSince
	}
	c.EstimatedPostCount = nil
	if n, ok := a.Before["perPeriod"].(float64); ok {
		c.EstimatedPostCount = new(int(n))
	}
	if n, ok := a.Before["perPeriod"].(int); ok {
		c.EstimatedPostCount = new(n)
	}
	c.GoalCadence, _ = a.Before["cadence"].(string)
	return u.campaigns.UpdateColumns(ctx, tx, c, "estimated_post_count", "goal_cadence")
}

// timeValue renders a date for a snapshot.
func timeValue(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UTC().Format(time.RFC3339Nano)
}

// parseTime reads a date back from a snapshot.
func parseTime(v any) (*time.Time, error) {
	if v == nil {
		return nil, nil
	}
	s, ok := v.(string)
	if !ok {
		return nil, fmt.Errorf("date snapshot is %T", v)
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// same reports whether two snapshots hold the same values. Both sides are
// built by this package, so their values are nil or strings.
func same(a, b models.JSONMap) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if fmt.Sprint(v) != fmt.Sprint(b[k]) {
			return false
		}
	}
	return true
}

// lockCampaign reads the campaign within tx and holds its row until tx ends,
// so the snapshot comparison and the restore see no write in between.
func lockCampaign(ctx context.Context, campaigns repository.CampaignRepository, tx bun.IDB, id string) (*models.Campaign, error) {
	c, err := campaigns.GetForUpdate(ctx, tx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return c, err
}

type campaignDates struct{ campaigns repository.CampaignRepository }

// Undo restores the campaign's dates. A manual phase plan that fits the
// current dates is re-anchored to the restored ones, as a date edit does; one
// that doesn't is left alone, since it may fit the restored dates again.
func (u campaignDates) Undo(ctx context.Context, tx bun.IDB, a *models.AssistantAction) error {
	c, err := lockCampaign(ctx, u.campaigns, tx, a.EntityID)
	if err != nil {
		return err
	}
	if !same(CampaignDates(c), a.After) {
		return ErrChangedSince
	}
	start, err := parseTime(a.Before["startDate"])
	if err != nil {
		return fmt.Errorf("restore start date: %w", err)
	}
	end, err := parseTime(a.Before["endDate"])
	if err != nil {
		return fmt.Errorf("restore end date: %w", err)
	}
	_, source := campaignphase.Resolve(c)
	c.StartDate, c.EndDate = start, end
	if err := u.campaigns.UpdateColumns(ctx, tx, c, "start_date", "end_date"); err != nil {
		return err
	}
	if source != campaignphase.SourceManual {
		return nil
	}
	entries, ok := campaignphase.Rebound(c, c.PhaseWindows)
	if !ok {
		return nil
	}
	rows := make([]models.CampaignPhaseWindow, len(entries))
	for i, e := range entries {
		rows[i] = models.CampaignPhaseWindow{PhaseID: e.PhaseID, StartDate: e.Start, EndDate: e.End}
	}
	return u.campaigns.ReplacePhaseWindowsTx(ctx, tx, c.ID, rows)
}

type campaignPlatforms struct{ campaigns repository.CampaignRepository }

// Undo restores the platforms the campaign targets and their post types.
func (u campaignPlatforms) Undo(ctx context.Context, tx bun.IDB, a *models.AssistantAction) error {
	c, err := lockCampaign(ctx, u.campaigns, tx, a.EntityID)
	if err != nil {
		return err
	}
	if !same(CampaignPlatforms(c), a.After) {
		return ErrChangedSince
	}
	b, err := json.Marshal(a.Before["platforms"])
	if err != nil {
		return fmt.Errorf("restore platforms: %w", err)
	}
	var restored models.CampaignPlatforms
	if err := json.Unmarshal(b, &restored); err != nil {
		return fmt.Errorf("restore platforms: %w", err)
	}
	if restored == nil {
		restored = models.CampaignPlatforms{}
	}
	c.TargetPlatforms = restored
	return u.campaigns.UpdateColumns(ctx, tx, c, "target_platforms")
}

type campaignBrief struct{ campaigns repository.CampaignRepository }

// Undo restores the campaign's brief fields.
func (u campaignBrief) Undo(ctx context.Context, tx bun.IDB, a *models.AssistantAction) error {
	c, err := lockCampaign(ctx, u.campaigns, tx, a.EntityID)
	if err != nil {
		return err
	}
	if !same(CampaignBrief(c), a.After) {
		return ErrChangedSince
	}
	text := func(k string) string { s, _ := a.Before[k].(string); return s }
	c.Description = text("description")
	c.TargetPersona = text("targetPersona")
	c.KeyMessages = text("keyMessages")
	c.ToneGuidelines = text("toneGuidelines")
	return u.campaigns.UpdateColumns(ctx, tx, c, "description", "target_persona", "key_messages", "tone_guidelines")
}

// CampaignCreated is the snapshot a new campaign is logged and compared
// with: what its proposal set, and the brief and platforms an edit since
// would change.
func CampaignCreated(c *models.Campaign) models.JSONMap {
	snap := models.JSONMap{
		"name":      c.Name,
		"startDate": timeValue(c.StartDate),
		"endDate":   timeValue(c.EndDate),
		"assetIds":  strings.Join(slices.Sorted(slices.Values(c.AssetIDs)), ","),
	}
	maps.Copy(snap, CampaignBrief(c))
	maps.Copy(snap, CampaignPlatforms(c))
	return snap
}

type campaignCreated struct{ campaigns repository.CampaignRepository }

// Undo deletes a campaign the assistant created while it is still as
// created: unchanged, with no posts or series, and no ideas anyone wrote,
// planned, asked for, merged or decided — only the ones its sources'
// analyses distilled on their own. The campaign's row and its ideas are
// locked first, so no post, series or idea is added to it, and no idea
// decided, between the check and the delete.
func (u campaignCreated) Undo(ctx context.Context, tx bun.IDB, a *models.AssistantAction) error {
	c, err := lockCampaign(ctx, u.campaigns, tx, a.EntityID)
	if err != nil {
		return err
	}
	if !same(CampaignCreated(c), a.After) {
		return ErrChangedSince
	}
	var ideaIDs []string
	if err := tx.NewSelect().Model((*models.Idea)(nil)).Column("i.id").
		Where("i.campaign_id = ?", c.ID).For("UPDATE OF i").Scan(ctx, &ideaIDs); err != nil {
		return err
	}
	used, err := campaignUsed(ctx, tx, c.ID)
	if err != nil {
		return err
	}
	if used {
		return ErrChangedSince
	}
	deleted, err := u.campaigns.DeleteTx(ctx, tx, c.ID)
	if err != nil {
		return err
	}
	if !deleted {
		return ErrNotFound
	}
	return nil
}

// campaignUsed reports whether anything was made in the campaign: a post, a
// series or its run, or an idea that isn't one its sources' analyses
// distilled on their own and nobody touched.
func campaignUsed(ctx context.Context, tx bun.IDB, campaignID string) (bool, error) {
	checks := []*bun.SelectQuery{
		tx.NewSelect().Model((*models.Post)(nil)).Where("po.campaign_id = ?", campaignID),
		tx.NewSelect().Model((*models.CampaignSeriesRun)(nil)).Where("cs.campaign_id = ?", campaignID),
		tx.NewSelect().Model((*models.Series)(nil)).Where("bs.campaign_id = ?", campaignID).Where("bs.deleted_at IS NULL"),
		tx.NewSelect().Model((*models.Idea)(nil)).
			Where("i.campaign_id = ?", campaignID).
			Where(`i.origin <> ? OR i.verdict IS NOT NULL OR i.distill_run_id IS NOT NULL OR i.merged_at IS NOT NULL
				OR i.plan_platform_id IS NOT NULL OR i.plan_post_type IS NOT NULL OR i.plan_phase_id IS NOT NULL
				OR i.plan_target_date IS NOT NULL`, models.IdeaOriginAssetAnalysis),
	}
	for _, q := range checks {
		ok, err := q.Exists(ctx)
		if err != nil || ok {
			return ok, err
		}
	}
	return false, nil
}
