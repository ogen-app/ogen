package packs

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/firebase/genkit/go/ai"

	"github.com/ogen-app/ogen/src/domain/entitlements"
	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/genkit/flows/assistant"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/assistantkit"
	"github.com/ogen-app/ogen/src/kernel/activity"
	"github.com/ogen-app/ogen/src/usecase/assistantundo"
	"github.com/ogen-app/ogen/src/usecase/campaign_actions/create"
	"github.com/ogen-app/ogen/src/usecase/campaigngoal"
	"github.com/ogen-app/ogen/src/usecase/scheduling"
)

// campaignNameMax bounds a proposed campaign's name.
const campaignNameMax = 120

// CampaignFromAssetsInput is the campaign to set up and the assets it draws
// on.
type CampaignFromAssetsInput struct {
	AssetIDs    []string `json:"assetIds,omitempty"    jsonschema:"description=The assets to use as its sources, e.g. the selected ones from getSelection; defaults to the asset open on screen"`
	Name        string   `json:"name"                  jsonschema:"description=The campaign's name, from what the sources are about"`
	Type        string   `json:"type,omitempty"        jsonschema:"description=The campaign type's name or label, e.g. Launch; omit for Evergreen"`
	StartDate   string   `json:"startDate,omitempty"   jsonschema:"description=First day (YYYY-MM-DD), from what the sources say, e.g. an opening date; omit when they don't say"`
	EndDate     string   `json:"endDate,omitempty"     jsonschema:"description=Last day (YYYY-MM-DD); omit when they don't say"`
	DateReason  string   `json:"dateReason,omitempty"  jsonschema:"description=With dates: why those, in a few words from the sources, e.g. Around the 14 March opening, from the brochure"`
	Description string   `json:"description,omitempty" jsonschema:"description=One or two sentences on what the campaign is for, from the sources"`
}

// CampaignFromAssetsOutput is the campaign made.
type CampaignFromAssetsOutput struct {
	CampaignID string     `json:"campaignId"`
	Name       string     `json:"name"`
	Type       string     `json:"type"`
	StartDate  *time.Time `json:"startDate,omitempty"`
	EndDate    *time.Time `json:"endDate,omitempty"`
	AssetIDs   []string   `json:"assetIds"`
	UndoID     string     `json:"undoId,omitempty"`
	// CreatedAt is when the yes made it.
	CreatedAt time.Time `json:"createdAt"`
	// Step is the campaign made and how many sources it got.
	Step *assistantkit.Step `json:"step,omitempty"`
}

// campaignPlan is a proposal checked and resolved: its type, dates and
// sources.
type campaignPlan struct {
	ids        []string
	sources    []models.Asset
	ct         *models.CampaignType
	start, end *time.Time
}

func (in *CampaignFromAssetsInput) sources(ctx context.Context) []string {
	ids := dedupe(in.AssetIDs)
	if len(ids) == 0 {
		if open := assistant.FocusedAsset(ctx); open != "" {
			ids = []string{open}
		}
	}
	return ids
}

// planCampaign checks a proposal before it is offered, and again when it
// runs: the name, the type, the dates and that the sources exist.
func planCampaign(ctx context.Context, d Deps, in CampaignFromAssetsInput) (*campaignPlan, error) {
	ids := in.sources(ctx)
	if err := checkAssetIDs(ids); err != nil {
		return nil, err
	}
	name := strings.TrimSpace(in.Name)
	if name == "" || len([]rune(name)) > campaignNameMax {
		return nil, assistantkit.Refused(fmt.Sprintf("name the campaign, in at most %d characters", campaignNameMax))
	}
	types, err := d.CampaignTypes.List(ctx)
	if err != nil {
		return nil, err
	}
	want := cmp.Or(strings.TrimSpace(in.Type), "evergreen")
	i := slices.IndexFunc(types, func(t models.CampaignType) bool {
		return strings.EqualFold(t.Name, want) || strings.EqualFold(t.Label, want) || t.ID == want
	})
	if i < 0 {
		names := make([]string, len(types))
		for j, t := range types {
			names[j] = cmp.Or(t.Label, t.Name)
		}
		return nil, assistantkit.Refused(fmt.Sprintf("there is no campaign type %q", want), names...)
	}
	p := &campaignPlan{ct: &types[i]}
	if p.start, err = isoDay(in.StartDate); err != nil {
		return nil, err
	}
	if p.end, err = isoDay(in.EndDate); err != nil {
		return nil, err
	}
	// A content plan needs the end after the start, so the campaign does too.
	if p.start != nil && p.end != nil && !p.start.Before(*p.end) {
		return nil, assistantkit.Refused("the end date must be after the start date")
	}
	metas, err := d.Assets.ListMeta(ctx, ids)
	if err != nil {
		return nil, err
	}
	if len(metas) == 0 {
		return nil, assistantkit.Refused("none of those assets exist")
	}
	for _, id := range ids {
		if i := slices.IndexFunc(metas, func(a models.Asset) bool { return a.ID == id }); i >= 0 {
			p.ids = append(p.ids, id)
			p.sources = append(p.sources, metas[i])
		}
	}
	return p, nil
}

func isoDay(s string) (*time.Time, error) {
	if s = strings.TrimSpace(s); s == "" {
		return nil, nil
	}
	t, err := time.Parse(time.DateOnly, s)
	if err != nil {
		return nil, assistantkit.Refused(fmt.Sprintf("%q isn't a date; use YYYY-MM-DD", s))
	}
	return &t, nil
}

// campaignFromAssets creates the campaign through the same service as the
// Campaigns screen, with the assets attached as its only sources.
func campaignFromAssets(ctx context.Context, d Deps, in CampaignFromAssetsInput) (*CampaignFromAssetsOutput, error) {
	p, err := planCampaign(ctx, d, in)
	if err != nil {
		return nil, err
	}
	id, err := models.NewID()
	if err != nil {
		return nil, err
	}
	cadence, _ := campaigngoal.Normalize("")
	now := time.Now().UTC()
	c := &models.Campaign{
		ID: id, Name: strings.TrimSpace(in.Name), Description: strings.TrimSpace(in.Description),
		CampaignTypeID: p.ct.ID, Status: models.StatusActive,
		AssetIDs: models.StringSlice(p.ids), UseAssets: true,
		TargetPlatforms: models.CampaignPlatforms{}, TagIDs: models.StringSlice{}, Tags: []models.Tag{},
		StartDate: p.start, EndDate: p.end,
		PublishingTime: scheduling.DefaultPublishingTime, PublishingDays: scheduling.DefaultPublishingDays(),
		SpreadMinutes: scheduling.DefaultSpreadMinutes, GoalCadence: cadence,
		CreatedBy: actor(ctx), CreatedAt: now, UpdatedAt: now,
	}
	err = d.CreateCampaign.Create(ctx, c, activity.SourceAssistant)
	if refusal := createRefusal(err); refusal != nil {
		return nil, refusal
	}
	if err != nil {
		return nil, err
	}
	assistantkit.Touch(ctx, "campaign", c.ID)
	undo := recordUndo(ctx, d, models.AssistantActionCampaignCreated, c.ID, models.JSONMap{}, assistantundo.CampaignCreated(c))
	return &CampaignFromAssetsOutput{
		CampaignID: c.ID, Name: c.Name, Type: cmp.Or(p.ct.Label, p.ct.Name),
		StartDate: c.StartDate, EndDate: c.EndDate, AssetIDs: p.ids, UndoID: undo, CreatedAt: c.CreatedAt,
		Step: &assistantkit.Step{Subject: c.Name, Sources: len(p.ids)},
	}, nil
}

// createRefusal turns the workspace's limits into a refusal the model can
// pass on; nil for any other error.
func createRefusal(err error) error {
	if _, ok := errors.AsType[*entitlements.QuotaExceededError](err); ok {
		return assistantkit.Refused("the workspace has as many active campaigns as its plan allows; archive one or upgrade first")
	}
	if _, ok := errors.AsType[*entitlements.FeatureNotAvailableError](err); ok {
		return assistantkit.Refused("the workspace's plan doesn't include that campaign type; use Evergreen")
	}
	if errors.Is(err, create.ErrInvalidType) {
		return assistantkit.Refused("that campaign type is gone; pick another")
	}
	return nil
}

// resolveFirst settles a call's defaults before the gate, so a proposal
// stores what it will use and a confirmed one runs as stored.
func resolveFirst[In, Out any](resolve func(context.Context, *In), next ai.ToolFunc[In, Out]) ai.ToolFunc[In, Out] {
	return func(ctx *ai.ToolContext, in In) (Out, error) {
		if !assistantkit.Confirmed(ctx) {
			resolve(ctx, &in)
		}
		return next(ctx, in)
	}
}

// campaignSummary is the proposal card's line.
func campaignSummary(ctx context.Context, d Deps, in CampaignFromAssetsInput) string {
	n := len(in.sources(ctx))
	s := fmt.Sprintf("New campaign %q from %s", strings.TrimSpace(in.Name), plural(n, "asset", "assets"))
	if p, err := planCampaign(ctx, d, in); err == nil {
		s += ", " + cmp.Or(p.ct.Label, p.ct.Name)
		if p.start != nil && p.end != nil {
			s += fmt.Sprintf(", %s – %s", p.start.Format("2 Jan"), p.end.Format("2 Jan"))
		}
	}
	return s
}

// campaignCard is the proposal card's details: the campaign's type and
// dates, the assets it will draw on, and that no posts come with it.
func campaignCard(ctx context.Context, d Deps, in CampaignFromAssetsInput) *assistantkit.Card {
	p, err := planCampaign(ctx, d, in)
	if err != nil {
		return nil
	}
	c := &assistantkit.Card{
		Verb:    "Create campaign",
		Action:  assistantkit.CardActionCreateCampaign,
		Meta:    sourcesMeta(p.sources),
		DrawsOn: assistantkit.CardDrawsOnSources,
		Rows:    []assistantkit.CardRow{{Kind: assistantkit.CardRowType, Value: cmp.Or(p.ct.Label, p.ct.Name)}},
		Sources: cardSources(p.sources),
	}
	if p.start != nil || p.end != nil {
		c.Rows = append(c.Rows, datesRow(p.start, p.end, strings.TrimSpace(in.DateReason)))
	}
	c.FootnoteKey, c.FootnoteCount = assistantkit.CardFootnoteCampaignSources, len(p.sources)
	if len(p.sources) == 1 {
		c.Footnote = "It becomes the campaign's source. No posts yet; a content plan can follow."
	} else {
		c.Footnote = fmt.Sprintf("The %d become its sources. No posts yet; a content plan can follow.", len(p.sources))
	}
	return c
}

// sourcesMeta is a card's headline meta: the one asset by its title, or how
// many.
func sourcesMeta(as []models.Asset) string {
	if len(as) == 1 {
		return "from " + as[0].Title
	}
	return "from " + plural(len(as), "asset", "assets")
}

func cardSources(as []models.Asset) []assistantkit.CardSource {
	out := make([]assistantkit.CardSource, len(as))
	for i, a := range as {
		out[i] = assistantkit.CardSource{ID: a.ID, Title: a.Title, Type: deref(a.Type)}
	}
	return out
}

// datesRow is a card's dates, either of them open, and why those.
func datesRow(start, end *time.Time, reason string) assistantkit.CardRow {
	r := assistantkit.CardRow{Kind: assistantkit.CardRowDates, Note: reason}
	var parts []string
	if start != nil {
		r.Start = start.Format(time.DateOnly)
		parts = append(parts, "from "+start.Format("2 Jan 2006"))
	}
	if end != nil {
		r.End = end.Format(time.DateOnly)
		parts = append(parts, "to "+end.Format("2 Jan 2006"))
	}
	if start != nil && end != nil {
		parts = []string{start.Format("2 Jan") + " – " + end.Format("2 Jan 2006")}
	}
	r.Value = strings.Join(parts, " ")
	return r
}
