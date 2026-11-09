package create

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"testing"

	"github.com/ogen-app/ogen/src/domain/entitlements"
	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
)

type memCampaigns struct {
	repository.CampaignRepository
	created []*models.Campaign
}

func (r *memCampaigns) Create(_ context.Context, c *models.Campaign) error {
	r.created = append(r.created, c)
	return nil
}

type memTypes struct {
	repository.CampaignTypeRepository
}

func (memTypes) GetByID(_ context.Context, id string) (*models.CampaignType, error) {
	switch id {
	case "evergreen":
		return &models.CampaignType{ID: id, Name: "evergreen", IsSystem: true}, nil
	case "launch":
		return &models.CampaignType{ID: id, Name: "launch", IsSystem: true}, nil
	case "custom":
		return &models.CampaignType{ID: id, Name: "Ours"}, nil
	}
	return nil, sql.ErrNoRows
}

type fixedPlan map[string]any

func (p fixedPlan) ResolveCurrent(context.Context, string) (*entitlements.Resolution, error) {
	r := &entitlements.Resolution{}
	for k, v := range p {
		r.Entitlements = append(r.Entitlements, entitlements.EntitlementValue{Feature: entitlements.Feature{Key: k}, Value: v})
	}
	return r, nil
}

type attached struct{ ids map[string][]string }

func (a *attached) ScheduleAttached(_ context.Context, _, campaignID string, ids []string) {
	a.ids[campaignID] = ids
}

func service(plan fixedPlan, active int64) (*Service, *memCampaigns, *attached) {
	camps, an := &memCampaigns{}, &attached{ids: map[string][]string{}}
	lim := entitlements.NewLimiter(plan, nil, entitlements.ModeEnforce).
		Register("active_campaigns", entitlements.CounterFunc(func(context.Context, string) (int64, error) { return active, nil }))
	return &Service{Campaigns: camps, Types: memTypes{}, Limiter: lim, Analysis: an}, camps, an
}

func TestCreate(t *testing.T) {
	ctx := tenantctx.With(t.Context(), "tn-1")
	s, camps, an := service(fixedPlan{"active_campaigns": float64(3), "all_campaign_types": true, "custom_campaign_types": false}, 1)

	c := &models.Campaign{ID: "c1", Name: "Spring", CampaignTypeID: "launch", AssetIDs: models.StringSlice{"a1", "a2"}}
	if err := s.Create(ctx, c, "assistant"); err != nil {
		t.Fatal(err)
	}
	if len(camps.created) != 1 || !slices.Equal(an.ids["c1"], []string{"a1", "a2"}) {
		t.Fatalf("created %d, analyses %v; want it stored and its sources queued", len(camps.created), an.ids)
	}

	for name, c := range map[string]struct {
		typeID string
		want   func(error) bool
	}{
		"an unknown type":    {"nope", func(err error) bool { return errors.Is(err, ErrInvalidType) }},
		"a gated-off custom": {"custom", func(err error) bool { _, ok := errors.AsType[*entitlements.FeatureNotAvailableError](err); return ok }},
	} {
		if err := s.Create(ctx, &models.Campaign{ID: "x", CampaignTypeID: c.typeID}, "api"); !c.want(err) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	if len(camps.created) != 1 {
		t.Fatalf("created %d; a refused campaign is never stored", len(camps.created))
	}
}

func TestCreate_Quota(t *testing.T) {
	ctx := tenantctx.With(t.Context(), "tn-1")
	s, camps, _ := service(fixedPlan{"active_campaigns": float64(3)}, 3)
	err := s.Create(ctx, &models.Campaign{ID: "c1", CampaignTypeID: "evergreen"}, "api")
	if err == nil || len(camps.created) != 0 {
		t.Fatalf("at the cap: err = %v, created %d", err, len(camps.created))
	}
	// The baseline type is never gated.
	s, camps, _ = service(fixedPlan{"active_campaigns": float64(3), "all_campaign_types": false}, 0)
	if err := s.Create(ctx, &models.Campaign{ID: "c1", CampaignTypeID: "evergreen"}, "api"); err != nil || len(camps.created) != 1 {
		t.Fatalf("evergreen = %v", err)
	}
	if err := s.Create(ctx, &models.Campaign{ID: "c2", CampaignTypeID: "launch"}, "api"); err == nil {
		t.Fatal("a gated system type was created")
	}
}
