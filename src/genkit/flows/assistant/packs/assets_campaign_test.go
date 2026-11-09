package packs

import (
	"context"
	"database/sql"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/firebase/genkit/go/ai"

	"github.com/ogen-app/ogen/src/domain/entitlements"
	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/domain/viewcontext"
	"github.com/ogen-app/ogen/src/genkit/flows/assistant"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/assistantkit"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/usecase/campaign_actions/create"
)

type typeList struct {
	repository.CampaignTypeRepository
}

var testTypes = []models.CampaignType{
	{ID: "t-ever", Name: "evergreen", Label: "Evergreen", IsSystem: true},
	{ID: "t-launch", Name: "launch", Label: "Launch", IsSystem: true},
}

func (typeList) List(context.Context) ([]models.CampaignType, error) { return testTypes, nil }
func (typeList) GetByID(_ context.Context, id string) (*models.CampaignType, error) {
	for _, t := range testTypes {
		if t.ID == id {
			return &t, nil
		}
	}
	return nil, sql.ErrNoRows
}

type createdCampaigns struct {
	repository.CampaignRepository
	made []*models.Campaign
}

func (r *createdCampaigns) Create(_ context.Context, c *models.Campaign) error {
	r.made = append(r.made, c)
	return nil
}

type plan map[string]any

func (p plan) ResolveCurrent(context.Context, string) (*entitlements.Resolution, error) {
	r := &entitlements.Resolution{}
	for k, v := range p {
		r.Entitlements = append(r.Entitlements, entitlements.EntitlementValue{Feature: entitlements.Feature{Key: k}, Value: v})
	}
	return r, nil
}

func campaignDeps(p plan, active int64) (Deps, *createdCampaigns, *memActions) {
	camps, actions := &createdCampaigns{}, &memActions{}
	lim := entitlements.NewLimiter(p, nil, entitlements.ModeEnforce).
		Register("active_campaigns", entitlements.CounterFunc(func(context.Context, string) (int64, error) { return active, nil }))
	return Deps{
		Assets:         memAssets{all: []models.Asset{{ID: "a1", Title: "Brochure"}, {ID: "a2", Title: "Press kit"}}},
		CampaignTypes:  typeList{},
		CreateCampaign: &create.Service{Campaigns: camps, Types: typeList{}, Limiter: lim},
		Actions:        actions,
	}, camps, actions
}

func TestCampaignFromAssets(t *testing.T) {
	d, camps, actions := campaignDeps(plan{"active_campaigns": float64(5), "all_campaign_types": true}, 1)
	ctx, touched := turn(t, workspace, nil)

	out, err := campaignFromAssets(ctx, d, CampaignFromAssetsInput{
		AssetIDs: []string{"a1", "a2", "a9"}, Name: " Spring reopening ", Type: "launch",
		StartDate: "2026-03-02", EndDate: "2026-04-26", Description: "Around the 14 March opening.",
	})
	if err != nil {
		t.Fatal(err)
	}
	c := camps.made[0]
	if c.Name != "Spring reopening" || c.CampaignTypeID != "t-launch" || !c.UseAssets || !slices.Equal(c.AssetIDs, []string{"a1", "a2"}) {
		t.Fatalf("campaign = %+v; want the type resolved by name and only existing assets attached", c)
	}
	if c.PublishingTime == "" || len(c.PublishingDays) == 0 || c.GoalCadence == "" || c.CreatedBy != "u1" {
		t.Fatalf("defaults = %q %v %q by %q", c.PublishingTime, c.PublishingDays, c.GoalCadence, c.CreatedBy)
	}
	if out.Type != "Launch" || out.UndoID == "" || !out.StartDate.Equal(time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("out = %+v", out)
	}
	if a := actions.rows[0]; a.Kind != models.AssistantActionCampaignCreated || a.EntityID != c.ID || a.After["assetIds"] != "a1,a2" {
		t.Fatalf("undo = %+v", a)
	}
	if !slices.Equal(touched(), []assistantkit.Touched{{Kind: "campaign", ID: c.ID}}) {
		t.Fatalf("touched = %v", touched())
	}
}

func TestCampaignFromAssets_Refusals(t *testing.T) {
	d, camps, _ := campaignDeps(plan{"active_campaigns": float64(5), "all_campaign_types": false}, 1)
	ctx, _ := turn(t, workspace, nil)
	for name, in := range map[string]CampaignFromAssetsInput{
		"no assets":       {Name: "x"},
		"no name":         {AssetIDs: []string{"a1"}},
		"an unknown type": {AssetIDs: []string{"a1"}, Name: "x", Type: "webinar"},
		"a bad date":      {AssetIDs: []string{"a1"}, Name: "x", StartDate: "14 March"},
		"dates reversed":  {AssetIDs: []string{"a1"}, Name: "x", StartDate: "2026-04-01", EndDate: "2026-03-01"},
		"unknown assets":  {AssetIDs: []string{"a9"}, Name: "x"},
		"a gated type":    {AssetIDs: []string{"a1"}, Name: "x", Type: "Launch"},
	} {
		_, err := campaignFromAssets(ctx, d, in)
		if err == nil {
			t.Errorf("%s: created", name)
			continue
		}
		refused(t, err)
	}
	// The unknown type's refusal lists the types there are.
	_, err := planCampaign(ctx, d, CampaignFromAssetsInput{AssetIDs: []string{"a1"}, Name: "x", Type: "webinar"})
	if r, _ := err.(*assistantkit.Refusal); r == nil || !slices.Contains(r.Alternatives, "Launch") {
		t.Fatalf("refusal = %v", err)
	}

	full, _, _ := campaignDeps(plan{"active_campaigns": float64(1)}, 1)
	_, err = campaignFromAssets(ctx, full, CampaignFromAssetsInput{AssetIDs: []string{"a1"}, Name: "x"})
	refused(t, err)
	if len(camps.made) != 0 {
		t.Fatalf("made %d", len(camps.made))
	}
}

// The proposal stores the open asset as its source, so a yes given on
// another screen still attaches it; its card names the type and dates.
func TestCampaignFromAssets_Proposal(t *testing.T) {
	d, _, _ := campaignDeps(plan{"active_campaigns": float64(5), "all_campaign_types": true}, 1)
	var stored CampaignFromAssetsInput
	call := resolveFirst(func(ctx context.Context, in *CampaignFromAssetsInput) { in.AssetIDs = in.sources(ctx) },
		func(_ *ai.ToolContext, in CampaignFromAssetsInput) (struct{}, error) {
			stored = in
			return struct{}{}, nil
		})
	page, _ := assistant.NewViewTurnContext(t.Context(), viewcontext.ViewContext{
		Version: viewcontext.Version, Screen: "asset", Scope: workspace,
		Focus: &viewcontext.Ref{Kind: viewcontext.KindAsset, ID: "a2"},
	}, nil)
	if _, err := call(&ai.ToolContext{Context: page}, CampaignFromAssetsInput{Name: "x"}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(stored.AssetIDs, []string{"a2"}) {
		t.Fatalf("stored sources = %v; want the open asset", stored.AssetIDs)
	}

	line := campaignSummary(page, d, CampaignFromAssetsInput{Name: "Spring reopening", Type: "Launch", StartDate: "2026-03-02", EndDate: "2026-04-26"})
	if !strings.HasPrefix(line, `New campaign "Spring reopening" from 1 asset, Launch, 2 Mar – 26 Apr`) {
		t.Fatalf("summary = %q", line)
	}
}
