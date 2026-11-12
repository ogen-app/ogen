package packs

import (
	"context"
	"database/sql"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/firebase/genkit/go/ai"
	"github.com/firebase/genkit/go/genkit"

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
		Assets:         memAssets{all: []models.Asset{{ID: "a1", Title: "Brochure", Type: new(models.AssetTypePDF)}, {ID: "a2", Title: "Press kit"}}},
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
	if out.Type != "Launch" || out.UndoID == "" || !out.StartDate.Equal(time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC)) || out.CreatedAt.IsZero() ||
		*out.Step != (assistantkit.Step{Subject: "Spring reopening", Sources: 2}) {
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
		"one day":         {AssetIDs: []string{"a1"}, Name: "x", StartDate: "2026-04-01", EndDate: "2026-04-01"},
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

	full, fullCamps, _ := campaignDeps(plan{"active_campaigns": float64(1)}, 1)
	_, err = campaignFromAssets(ctx, full, CampaignFromAssetsInput{AssetIDs: []string{"a1"}, Name: "x"})
	refused(t, err)
	if len(camps.made) != 0 || len(fullCamps.made) != 0 {
		t.Fatalf("made %d / %d; a refused campaign is never stored", len(camps.made), len(fullCamps.made))
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

	// The card names its button, sources and dates, with why those dates.
	card := campaignCard(page, d, CampaignFromAssetsInput{
		AssetIDs: []string{"a1", "a2"}, Name: "Spring reopening", Type: "Launch",
		StartDate: "2026-03-02", EndDate: "2026-04-26", DateReason: "Around the 14 March opening, from the brochure",
	})
	want := &assistantkit.Card{
		Verb: "Create campaign", Meta: "from 2 assets",
		Rows: []assistantkit.CardRow{
			{Kind: assistantkit.CardRowType, Value: "Launch"},
			{Kind: assistantkit.CardRowDates, Value: "2 Mar – 26 Apr 2026", Note: "Around the 14 March opening, from the brochure", Start: "2026-03-02", End: "2026-04-26"},
		},
		Sources:  []assistantkit.CardSource{{ID: "a1", Title: "Brochure", Type: "PDF"}, {ID: "a2", Title: "Press kit"}},
		Footnote: "The 2 become its sources. No posts yet; a content plan can follow.",
	}
	if !reflect.DeepEqual(card, want) {
		t.Fatalf("card = %+v\nwant %+v", card, want)
	}
	// One source is named; no dates leaves the row out; a bad call has none.
	card = campaignCard(page, d, CampaignFromAssetsInput{Name: "x"})
	if card.Meta != "from Press kit" || len(card.Rows) != 1 || !strings.HasPrefix(card.Footnote, "It becomes") {
		t.Fatalf("one-source card = %+v", card)
	}
	if card := campaignCard(page, d, CampaignFromAssetsInput{}); card != nil {
		t.Fatalf("card for a refused call = %+v", card)
	}
}

// Creating a campaign calls no model, so a yes past the spend cap still runs.
func TestCampaignFromAssets_IsFree(t *testing.T) {
	d, _, _ := campaignDeps(plan{}, 0)
	d.Campaigns = &memCampaigns{}
	specs, err := assetsPack(d).Define(genkit.Init(t.Context()))
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(specs, func(s assistantkit.Spec) bool { return s.Name == "createCampaignFromAssets" })
	if i < 0 || !specs[i].Free || specs[i].Tier != assistantkit.TierProposal {
		t.Fatalf("spec = %+v", specs[max(i, 0)])
	}
}

// The tagging offer's card names the tags and the assets they go on.
func TestTagCard(t *testing.T) {
	d, _, _ := campaignDeps(plan{}, 0)
	ctx, _ := turn(t, workspace, nil)
	card := tagCard(ctx, d, TagAssetsInput{AssetIDs: []string{"a1", "a2", "a1"}, Add: []string{"press", "spring"}, Remove: []string{"old"}})
	if card.Verb != "Tag 2 assets" || card.Meta != "" || len(card.Sources) != 2 || card.Sources[0].Type != "PDF" ||
		len(card.Rows) != 2 || card.Rows[0].Value != "Add press, spring" || card.Rows[1].Value != "Remove old" {
		t.Fatalf("card = %+v", card)
	}
	if card := tagCard(ctx, d, TagAssetsInput{AssetIDs: []string{"a1"}, Remove: []string{"old"}}); card.Verb != "Untag 1 asset" {
		t.Fatalf("untag card = %+v", card)
	}
	if card := tagCard(ctx, d, TagAssetsInput{AssetIDs: []string{"a9"}}); card != nil {
		t.Fatalf("card for missing assets = %+v", card)
	}
}
