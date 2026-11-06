package assistantundo

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
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
