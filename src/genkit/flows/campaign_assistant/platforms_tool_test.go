package campaign_assistant

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

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
		fakemodel.Answer(`{"explanation":"Threads isn't in this campaign yet. Want me to add it?","action":"answered"}`))
	if len(resp.Proposals) != 1 || len(h.generated) != 0 || len(h.proposals.rows) != 1 {
		t.Fatalf("turn 1: proposals = %+v, generated = %d, stored = %d", resp.Proposals, len(h.generated), len(h.proposals.rows))
	}
	offer := h.proposals.rows[0]
	if o, ok := offer.Option("add_platform"); !ok || o.Tool != "updateCampaignPlatforms" {
		t.Fatalf("offer options = %+v", offer.Options)
	}

	// The same turn can then write for the platform it just added.
	resp, _ = h.turn("yes please",
		fakemodel.Tools(fakemodel.Call{Name: "confirmOffer", Input: map[string]any{"proposalId": offer.ID, "optionId": "add_platform"}}),
		fakemodel.Tools(fakemodel.Call{Name: "generatePosts", Input: map[string]any{"platforms": []string{"Threads"}, "count": 3}}),
		fakemodel.Answer(`{"explanation":"Threads is in, and 3 posts are drafted.","action":"answered"}`))
	r := campaignResult(resp)
	if r == nil || r.Action != actionPostsGenerated || r.Platforms == nil || r.Platforms.UndoID == "" {
		t.Fatalf("turn 2: result = %+v", r)
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
	a := actions.logged(t, r.Platforms.UndoID)
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

// platformPosts are a campaign's posts, by platform, type and status.
type platformPosts struct {
	repository.PostRepository
	posts []models.Post
}

func (p platformPosts) ListRowsByCampaign(context.Context, string, ...string) ([]models.Post, error) {
	return p.posts, nil
}

// twoPlatforms is linkedInOnly with Threads targeted too.
func twoPlatforms() *models.Campaign {
	c := linkedInOnly()
	c.TargetPlatforms = append(c.TargetPlatforms, models.CampaignPlatform{ID: "pl2"})
	c.Platforms = append(c.Platforms, models.Platform{ID: "pl2", Name: "Threads"})
	return c
}

// Removing a platform with only drafts on it leaves them: the card names the
// button and the drafts that stay, the result lists them, and the
// campaign's last platform can't go.
func TestUpdateCampaignPlatforms_RemovesAndKeepsDrafts(t *testing.T) {
	c := twoPlatforms()
	posts := platformPosts{posts: []models.Post{
		{ID: "t1", Title: "A pool in the rock", PlatformID: "pl2", Status: models.PostStatusDraft},
		{ID: "t2", PlatformID: "pl2", Status: models.PostStatusScheduledForManualPublish},
		{ID: "t3", PlatformID: "pl2", Status: models.PostStatusPublished},
		{ID: "l1", PlatformID: "pl1", Status: models.PostStatusDraft},
	}}
	campaigns := &storedCampaign{c: c}
	st := &requestState{campaignID: "c1", campaign: c,
		repos: CampaignAssistantRepos{Proposals: &memProposals{}, Platforms: catalogRepo{}, Posts: posts, Campaigns: campaigns}}
	in := UpdateCampaignPlatformsInput{Remove: []platforms.Request{{Platform: "Threads"}}}

	out := runTool(t, st, "updateCampaignPlatforms", map[string]any{"remove": []map[string]any{{"platform": "Threads"}}})
	if out.Status != assistantkit.StatusProposed || out.Summary != "Remove Threads. 2 unpublished posts stay on it, 1 scheduled and still publishing until taken off the calendar" {
		t.Fatalf("outcome = %+v", out)
	}
	card := updatePlatformsGate.Card(withRequestState(t.Context(), st), in)
	if card == nil || card.Verb != "Remove Threads" || len(card.Rows) != 1 || card.Rows[0].Kind != assistantkit.CardRowPosts ||
		card.Rows[0].Note != "they stay, flagged “not a platform of this campaign”" {
		t.Fatalf("card = %+v", card)
	}
	got, err := toolUpdateCampaignPlatforms(withRequestState(t.Context(), st), in)
	if err != nil || got.Removed != "Threads" || got.PostsLeft == nil || got.PostsLeft.Posts != 2 || !slices.Equal(got.Platforms, []string{"LinkedIn"}) {
		t.Fatalf("out = %+v, err = %v", got, err)
	}
	r := st.platformsResult
	if len(c.TargetPlatforms) != 1 || len(c.Platforms) != 1 || r.Removed != "Threads" || r.PostsLeft.Posts != 2 {
		t.Fatalf("campaign = %+v / %+v, result = %+v", c.TargetPlatforms, c.Platforms, r)
	}
	if len(r.Kept) != 2 || r.Kept[0] != (KeptPost{ID: "t1", Title: "A pool in the rock", Platform: "Threads", Status: models.PostStatusDraft}) || r.Kept[1].Title != "Untitled post" {
		t.Fatalf("kept = %+v", r.Kept)
	}
	out = runTool(t, st, "updateCampaignPlatforms", map[string]any{"remove": []map[string]any{{"platform": "LinkedIn"}}})
	if out.Status != assistantkit.StatusRefused {
		t.Fatalf("last platform = %+v", out)
	}
}

// A removal with nothing on it says so: zero counts, not a missing field.
func TestUpdateCampaignPlatforms_NothingOnIt(t *testing.T) {
	c := twoPlatforms()
	st := &requestState{campaignID: "c1", campaign: c,
		repos: CampaignAssistantRepos{Platforms: catalogRepo{}, Posts: platformPosts{}, Campaigns: &storedCampaign{c: c}}}
	if _, err := toolUpdateCampaignPlatforms(withRequestState(t.Context(), st), UpdateCampaignPlatformsInput{Remove: []platforms.Request{{Platform: "Threads"}}}); err != nil {
		t.Fatal(err)
	}
	if r := st.platformsResult; r.PostsLeft == nil || *r.PostsLeft != (PostsLeft{}) || r.Kept != nil {
		t.Fatalf("result = %+v", r)
	}
}

// scheduledOnThreads is a campaign with a post scheduled to publish on
// Threads and a draft beside it.
func scheduledOnThreads() (*models.Campaign, platformPosts) {
	at := time.Date(2026, 4, 22, 9, 0, 0, 0, time.UTC)
	return twoPlatforms(), platformPosts{posts: []models.Post{
		{ID: "t1", Title: "Three nights, one treatment", PlatformID: "pl2", Status: models.PostStatusScheduled, ScheduledAt: &at},
		{ID: "t2", Title: "Spring menu", PlatformID: "pl2", Status: models.PostStatusDraft},
	}}
}

// A post scheduled on the platform holds the removal back: the choice is to
// unschedule it first, recommended, or remove later, with a footnote on why.
func TestUpdateCampaignPlatforms_ScheduledPostBlocks(t *testing.T) {
	c, posts := scheduledOnThreads()
	store := &memProposals{}
	st := &requestState{campaignID: "c1", campaign: c, cancelPost: func(context.Context, string, string) error { return nil },
		repos: CampaignAssistantRepos{Proposals: store, Platforms: catalogRepo{}, Posts: posts, Campaigns: &storedCampaign{c: c}}}

	out := runTool(t, st, "updateCampaignPlatforms", map[string]any{"remove": []map[string]any{{"platform": "Threads"}}})
	if out.Status != assistantkit.StatusConflict || out.Reason != "Threads has 2 posts in this campaign" || len(out.Options) != 2 {
		t.Fatalf("outcome = %+v", out)
	}
	unschedule, later := out.Options[0], out.Options[1]
	if unschedule.ID != "unschedule" || !unschedule.Recommended || unschedule.Title != "Unschedule 1, then remove" ||
		unschedule.Description != "“Three nights, one treatment” is scheduled for Wed Apr 22, 09:00 and goes back to ready to publish. The 1 other post stays, flagged in Quality." {
		t.Fatalf("unschedule = %+v", unschedule)
	}
	if later.ID != "later" || later.Recommended || later.Description != "Keep Threads until the scheduled post is out; ask again after Wed Apr 22." {
		t.Fatalf("later = %+v", later)
	}
	if o, _ := store.rows[0].Option("unschedule"); o.Args["scheduled"] != scheduledUnschedule {
		t.Fatalf("stored unschedule args = %+v", o.Args)
	}
	err := updatePlatformsGate.Precheck(withRequestState(t.Context(), st), UpdateCampaignPlatformsInput{Remove: []platforms.Request{{Platform: "Threads"}}})
	ce, ok := errors.AsType[*assistantkit.ConflictError](err)
	if !ok || ce.Card == nil || ce.Card.Footnote != "A post already scheduled to Threads would publish where the campaign no longer is, so removal waits on it." {
		t.Fatalf("conflict = %+v", err)
	}
	if len(c.TargetPlatforms) != 2 {
		t.Fatalf("removed before the choice: %+v", c.TargetPlatforms)
	}
}

// Without a way to withdraw posts, waiting is the only way offered.
func TestUpdateCampaignPlatforms_NoWithdrawOffersLaterOnly(t *testing.T) {
	c, posts := scheduledOnThreads()
	st := &requestState{campaignID: "c1", campaign: c,
		repos: CampaignAssistantRepos{Proposals: &memProposals{}, Platforms: catalogRepo{}, Posts: posts, Campaigns: &storedCampaign{c: c}}}
	out := runTool(t, st, "updateCampaignPlatforms", map[string]any{"remove": []map[string]any{{"platform": "Threads"}}})
	if out.Status != assistantkit.StatusConflict || len(out.Options) != 1 || out.Options[0].ID != "later" {
		t.Fatalf("outcome = %+v", out)
	}
}

// Picking "unschedule" withdraws the scheduled post, then removes the
// platform; "later" writes nothing; an offer confirmed after a post was
// scheduled there removes nothing either.
func TestUpdateCampaignPlatforms_ScheduledChoices(t *testing.T) {
	remove := []platforms.Request{{Platform: "Threads"}}
	t.Run("unschedule", func(t *testing.T) {
		c, posts := scheduledOnThreads()
		var cancelled []string
		st := &requestState{campaignID: "c1", campaign: c,
			cancelPost: func(_ context.Context, id, _ string) error { cancelled = append(cancelled, id); return nil },
			repos:      CampaignAssistantRepos{Platforms: catalogRepo{}, Posts: posts, Campaigns: &storedCampaign{c: c}}}
		got, err := toolUpdateCampaignPlatforms(withRequestState(t.Context(), st), UpdateCampaignPlatformsInput{Remove: remove, Scheduled: scheduledUnschedule})
		if err != nil || got.Unscheduled != 1 || !slices.Equal(cancelled, []string{"t1"}) || len(c.TargetPlatforms) != 1 {
			t.Fatalf("out = %+v, err = %v, cancelled = %v", got, err, cancelled)
		}
		if r := st.platformsResult; r.Unscheduled != 1 || len(r.Kept) != 2 || r.Kept[0].Status != models.PostStatusReadyForPublish {
			t.Fatalf("result = %+v", r)
		}
	})
	t.Run("a failed withdrawal removes nothing", func(t *testing.T) {
		c, posts := scheduledOnThreads()
		st := &requestState{campaignID: "c1", campaign: c,
			cancelPost: func(context.Context, string, string) error { return errors.New("queue down") },
			repos:      CampaignAssistantRepos{Platforms: catalogRepo{}, Posts: posts, Campaigns: &storedCampaign{c: c}}}
		_, err := toolUpdateCampaignPlatforms(withRequestState(t.Context(), st), UpdateCampaignPlatformsInput{Remove: remove, Scheduled: scheduledUnschedule})
		if _, ok := errors.AsType[*assistantkit.Refusal](err); !ok || len(c.TargetPlatforms) != 2 || st.platformsResult != nil {
			t.Fatalf("err = %v, platforms = %+v", err, c.TargetPlatforms)
		}
	})
	t.Run("later", func(t *testing.T) {
		c, posts := scheduledOnThreads()
		st := &requestState{campaignID: "c1", campaign: c,
			repos: CampaignAssistantRepos{Platforms: catalogRepo{}, Posts: posts, Campaigns: &storedCampaign{c: c}}}
		got, err := toolUpdateCampaignPlatforms(withRequestState(t.Context(), st), UpdateCampaignPlatformsInput{Remove: remove, Scheduled: scheduledLater})
		if err != nil || got.Later == nil || got.Later.Removed != "Threads" || got.Later.Until != "2026-04-22T09:00:00Z" ||
			len(c.TargetPlatforms) != 2 || st.platformsResult != nil {
			t.Fatalf("out = %+v, err = %v", got, err)
		}
	})
	t.Run("scheduled since the offer", func(t *testing.T) {
		c, posts := scheduledOnThreads()
		st := &requestState{campaignID: "c1", campaign: c,
			repos: CampaignAssistantRepos{Platforms: catalogRepo{}, Posts: posts, Campaigns: &storedCampaign{c: c}}}
		_, err := toolUpdateCampaignPlatforms(withRequestState(t.Context(), st), UpdateCampaignPlatformsInput{Remove: remove})
		if r, ok := errors.AsType[*assistantkit.Refusal](err); !ok || !strings.HasPrefix(r.Reason, assistantkit.ChangedSince) || len(c.TargetPlatforms) != 2 {
			t.Fatalf("err = %v", err)
		}
	})
}
