package campaign_assistant

import (
	"context"
	"slices"
	"testing"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/assistantkit"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/fakemodel"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/usecase/assistantundo"
)

type catalogRepo struct{ repository.PlatformRepository }

func (catalogRepo) ListEnabled(context.Context) ([]models.Platform, error) {
	return []models.Platform{
		{ID: "pl1", Name: "LinkedIn", PostTypes: models.PostTypeMap{"article": "Article", "carousel": "Carousel"}},
		{ID: "pl2", Name: "Threads", PostTypes: models.PostTypeMap{"text-post": "Text"}},
	}, nil
}

// linkedInOnly is a campaign that targets LinkedIn and not Threads.
func linkedInOnly() *models.Campaign {
	c := timelineCampaign()
	c.ID = "c1"
	c.Platforms = []models.Platform{{ID: "pl1", Name: "LinkedIn", PostTypes: models.PostTypeMap{"article": "Article"}}}
	c.TargetPlatforms = models.CampaignPlatforms{{ID: "pl1"}}
	return c
}

// The beta-test loop (CON-257): "add Threads posts" on a campaign without
// Threads offers to add it, and "yes please" adds it the first time.
func TestRunner_YesPleaseAddsThePlatform(t *testing.T) {
	h := newRunnerHarness(t)
	campaigns := &storedCampaign{c: linkedInOnly()}
	actions := &memActions{}
	h.repos.Campaigns, h.repos.Platforms, h.repos.Actions = campaigns, catalogRepo{}, actions

	resp, _ := h.turn("Add a few Threads posts",
		fakemodel.Tools(fakemodel.Call{Name: "generatePosts", Input: map[string]any{"platforms": []string{"Threads"}, "count": 3}}),
		fakemodel.Answer(`{"explanation":"Threads isn't in this campaign yet. Want me to add it?","action":"proposed"}`))
	if resp.Action != actionProposed || len(h.generated) != 0 || len(h.proposals.rows) != 1 {
		t.Fatalf("turn 1: action = %q, generated = %d, proposals = %d", resp.Action, len(h.generated), len(h.proposals.rows))
	}
	offer := h.proposals.rows[0]
	if o, ok := offer.Option("add_platform"); !ok || o.Tool != "updateCampaignPlatforms" {
		t.Fatalf("offer options = %+v", offer.Options)
	}

	resp, _ = h.turn("yes please",
		fakemodel.Tools(fakemodel.Call{Name: "confirmProposal", Input: map[string]any{"proposalId": offer.ID, "optionId": "add_platform"}}),
		fakemodel.Answer(`{"explanation":"Threads is in.","action":"platforms_updated"}`))
	if resp.Action != actionPlatformsUpdated || resp.Platforms == nil || resp.Platforms.UndoID == "" {
		t.Fatalf("turn 2: action = %q, platforms = %+v", resp.Action, resp.Platforms)
	}
	tp := campaigns.c.TargetPlatforms
	if !slices.ContainsFunc(tp, func(p models.CampaignPlatform) bool { return p.ID == "pl2" }) {
		t.Fatalf("target platforms = %+v, want Threads added", tp)
	}

	// And it can be undone.
	if _, err := assistantundo.New(actions, campaigns).Undo(t.Context(), resp.Platforms.UndoID, "user-1"); err != nil {
		t.Fatal(err)
	}
	if len(campaigns.c.TargetPlatforms) != 1 {
		t.Fatalf("after undo = %+v", campaigns.c.TargetPlatforms)
	}
}

// Asking for a platform directly is a proposal; an unknown platform or type is
// refused with what would work.
func TestUpdateCampaignPlatforms_ProposedAndChecked(t *testing.T) {
	store := &memProposals{}
	st := &requestState{campaignID: "c1", campaign: linkedInOnly(),
		repos: CampaignAssistantRepos{Proposals: store, Platforms: catalogRepo{}}}

	out := runTool(t, st, "updateCampaignPlatforms", map[string]any{"platforms": []map[string]any{{"platform": "Threads"}}})
	if out.Status != assistantkit.StatusProposed || len(store.rows) != 1 {
		t.Fatalf("outcome = %+v", out)
	}
	out = runTool(t, st, "updateCampaignPlatforms", map[string]any{"platforms": []map[string]any{{"platform": "MySpace"}}})
	if out.Status != assistantkit.StatusRefused || !slices.Equal(out.Alternatives, []string{"LinkedIn", "Threads"}) {
		t.Fatalf("unknown platform = %+v", out)
	}
	out = runTool(t, st, "updateCampaignPlatforms", map[string]any{"platforms": []map[string]any{{"platform": "LinkedIn"}}})
	if out.Status != assistantkit.StatusRefused {
		t.Fatalf("already targeted = %+v", out)
	}
}
