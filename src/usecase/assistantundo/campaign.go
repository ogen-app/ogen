package assistantundo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

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

func timeValue(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UTC().Format(time.RFC3339Nano)
}

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

func loadCampaign(ctx context.Context, campaigns repository.CampaignRepository, id string) (*models.Campaign, error) {
	c, err := campaigns.GetByID(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return c, err
}

type campaignDates struct{ campaigns repository.CampaignRepository }

func (u campaignDates) Undo(ctx context.Context, a *models.AssistantAction) error {
	c, err := loadCampaign(ctx, u.campaigns, a.EntityID)
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
	c.StartDate, c.EndDate = start, end
	c.UpdatedAt = time.Now().UTC()
	return u.campaigns.Update(ctx, c)
}

type campaignBrief struct{ campaigns repository.CampaignRepository }

func (u campaignBrief) Undo(ctx context.Context, a *models.AssistantAction) error {
	c, err := loadCampaign(ctx, u.campaigns, a.EntityID)
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
	c.UpdatedAt = time.Now().UTC()
	return u.campaigns.Update(ctx, c)
}
