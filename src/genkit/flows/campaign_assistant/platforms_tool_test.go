package campaign_assistant

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/assistantkit"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/fakemodel"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/usecase/assistantundo"
	"github.com/ogen-app/ogen/src/usecase/campaign_actions/platforms"
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

	// The same turn can then write for the platform it just added.
	resp, _ = h.turn("yes please",
		fakemodel.Tools(fakemodel.Call{Name: "confirmProposal", Input: map[string]any{"proposalId": offer.ID, "optionId": "add_platform"}}),
		fakemodel.Tools(fakemodel.Call{Name: "generatePosts", Input: map[string]any{"platforms": []string{"Threads"}, "count": 3}}),
		fakemodel.Answer(`{"explanation":"Threads is in, and 3 posts are drafted.","action":"posts_generated"}`))
	if resp.Action != actionPostsGenerated || resp.Platforms == nil || resp.Platforms.UndoID == "" {
		t.Fatalf("turn 2: action = %q, platforms = %+v", resp.Action, resp.Platforms)
	}
	if len(h.generated) != 1 || h.generated[0].Count != 3 || !slices.Equal(h.generated[0].PlatformIDs, []string{"pl2"}) {
		t.Fatalf("generated = %+v, want 3 Threads posts", h.generated)
	}
	tp := campaigns.c.TargetPlatforms
	if !slices.ContainsFunc(tp, func(p models.CampaignPlatform) bool { return p.ID == "pl2" }) {
		t.Fatalf("target platforms = %+v, want Threads added", tp)
	}
	if !slices.EqualFunc(campaigns.columns, [][]string{{"target_platforms"}}, slices.Equal) {
		t.Fatalf("columns written = %v, want only target_platforms", campaigns.columns)
	}

	// The undo service (tested against Postgres) restores Before while the
	// campaign matches After.
	a := actions.logged(t, resp.Platforms.UndoID)
	if len(a.Before["platforms"].([]any)) != 1 || len(a.After["platforms"].([]any)) != 2 {
		t.Fatalf("logged before = %v, after = %v", a.Before, a.After)
	}
	// After is what undo compares the campaign with: it must match the
	// stored one whole.
	if want := assistantundo.CampaignPlatforms(campaigns.c); !reflect.DeepEqual(a.After, want) {
		t.Fatalf("logged after = %v, want the stored campaign's %v", a.After, want)
	}
}

// A failed save leaves the turn's campaign as it was, so no later tool in the
// turn sees a platform that was never stored.
func TestUpdateCampaignPlatforms_FailedSaveChangesNothing(t *testing.T) {
	c := linkedInOnly()
	campaigns := &storedCampaign{c: linkedInOnly(), failUpdate: errors.New("db down")}
	st := &requestState{campaignID: "c1", campaign: c, repos: CampaignAssistantRepos{Campaigns: campaigns, Platforms: catalogRepo{}}}

	in := UpdateCampaignPlatformsInput{Platforms: []platforms.Request{{Platform: "Threads"}}}
	if _, err := toolUpdateCampaignPlatforms(withRequestState(t.Context(), st), in); err == nil {
		t.Fatal("update succeeded with a failing save")
	}
	if len(c.TargetPlatforms) != 1 || len(c.Platforms) != 1 {
		t.Fatalf("campaign = %+v / %+v, want LinkedIn only", c.TargetPlatforms, c.Platforms)
	}
}

// Asking for a post type a platform doesn't have returns the planner's
// refusal with the types it does have, not an offer to add the platform.
func TestGeneratePosts_MissingPostTypeIsRefused(t *testing.T) {
	st := &requestState{campaignID: "c1", campaign: linkedInOnly(),
		repos: CampaignAssistantRepos{Proposals: &memProposals{}, Platforms: catalogRepo{}}}

	out := runTool(t, st, "generatePosts", map[string]any{"platforms": []string{"Threads"}, "count": 3, "postType": "carousel"})
	if out.Status != assistantkit.StatusRefused || !slices.Equal(out.Alternatives, []string{"text-post"}) {
		t.Fatalf("outcome = %+v, want the post-type refusal", out)
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

// Asking for a platform the campaign narrowed to some post types, without
// naming types, opens it to all of them, and the offer says so.
func TestUpdateCampaignPlatforms_OfferNamesTheRestrictionLift(t *testing.T) {
	store := &memProposals{}
	c := linkedInOnly()
	c.TargetPlatforms = models.CampaignPlatforms{{ID: "pl1", PostTypes: []string{"article"}}}
	st := &requestState{campaignID: "c1", campaign: c,
		repos: CampaignAssistantRepos{Proposals: store, Platforms: catalogRepo{}}}

	out := runTool(t, st, "updateCampaignPlatforms", map[string]any{"platforms": []map[string]any{{"platform": "LinkedIn"}}})
	if out.Status != assistantkit.StatusProposed || !strings.Contains(out.Summary, "LinkedIn (every post type)") {
		t.Fatalf("outcome = %+v, want an offer naming every post type", out)
	}
}
