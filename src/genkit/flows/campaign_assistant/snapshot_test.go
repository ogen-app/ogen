package campaign_assistant

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/fakemodel"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/usecase/campaign_actions/state"
)

type statusRows struct {
	repository.PostRepository
	statuses []models.PostStatus
}

func (r statusRows) ListRowsByCampaign(context.Context, string, ...string) ([]models.Post, error) {
	out := make([]models.Post, len(r.statuses))
	for i, s := range r.statuses {
		out[i] = models.Post{Status: s}
	}
	return out, nil
}

// The planner sees the goal, the room left under it and the allowed post types
// at the start of the user's message, not in the cached system prompt.
func TestRunner_StateOpensTheUserMessage(t *testing.T) {
	h := newRunnerHarness(t)
	perPeriod := 5
	c := timelineCampaign()
	c.ID = "c1"
	c.EstimatedPostCount, c.GoalCadence = &perPeriod, "month"
	c.Platforms = []models.Platform{{ID: "pl2", Name: "Threads", PostTypes: models.PostTypeMap{"text-post": "Text"}}}
	c.TargetPlatforms = models.CampaignPlatforms{{ID: "pl2"}}
	h.repos.Campaigns = oneCampaign{c: c}
	h.repos.Posts = statusRows{statuses: []models.PostStatus{models.PostStatusDraft, models.PostStatusDraft, models.PostStatusScheduled}}

	m := fakemodel.Define(h.g, "state", fakemodel.Answer(`{"explanation":"ok","action":"answered"}`))
	h.turnWith(m, "How much room is left?")

	req := m.Requests()[0]
	user := req.Messages[len(req.Messages)-1]
	text := user.Text()
	for _, want := range []string{
		"<campaign_state>",
		"- Goal: 5 posts (5 per month over 1). Already in the campaign: 3. Room left under the goal: 2.",
		"- Posts: 3 (2 draft, 1 scheduled)",
		"  - Threads: text-post",
		"</campaign_state>\n\nHow much room is left?",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("user message lacks %q:\n%s", want, text)
		}
	}
	for _, msg := range req.Messages {
		if msg.Role == "system" && strings.Contains(msg.Text(), "Room left under the goal") {
			t.Error("the per-turn state is in the cached system prompt")
		}
	}
}

type failingRows struct{ repository.PostRepository }

func (failingRows) ListRowsByCampaign(context.Context, string, ...string) ([]models.Post, error) {
	return nil, errors.New("db down")
}

// A failed post read keeps the turn going but says the room is unknown,
// instead of reporting an empty campaign with the whole goal free.
func TestRunner_StateUnreadPostsLeaveRoomUnknown(t *testing.T) {
	h := newRunnerHarness(t)
	perPeriod := 5
	c := timelineCampaign()
	c.ID = "c1"
	c.EstimatedPostCount, c.GoalCadence = &perPeriod, "month"
	h.repos.Campaigns = oneCampaign{c: c}
	h.repos.Posts = failingRows{}

	m := fakemodel.Define(h.g, "state-unread", fakemodel.Answer(`{"explanation":"ok","action":"answered"}`))
	h.turnWith(m, "Add five posts")

	req := m.Requests()[0]
	text := req.Messages[len(req.Messages)-1].Text()
	for _, want := range []string{"the room left under the goal is unknown", "- Posts: unknown"} {
		if !strings.Contains(text, want) {
			t.Errorf("user message lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "Room left under the goal:") {
		t.Errorf("unread posts reported as room:\n%s", text)
	}
}

// datedRows are a campaign's posts with their dates and platforms; columns
// records what was asked for.
type datedRows struct {
	repository.PostRepository
	posts   []models.Post
	columns *[]string
}

func (r datedRows) ListRowsByCampaign(_ context.Context, _ string, columns ...string) ([]models.Post, error) {
	if r.columns != nil {
		*r.columns = columns
	}
	return r.posts, nil
}

// lensDigests reports a finished analysis for the ids in ready, through the
// lens it was asked for.
type lensDigests struct {
	repository.AssetAnalysisRepository
	ready []string
	lens  **string
}

func (l lensDigests) LensDigests(_ context.Context, ids []string, campaignID *string) (map[string]repository.LensDigest, error) {
	*l.lens = campaignID
	out := map[string]repository.LensDigest{}
	for _, id := range ids {
		status := models.AssetAnalysisStatusPending
		if slices.Contains(l.ready, id) {
			status = models.AssetAnalysisStatusReady
		}
		out[id] = repository.LensDigest{ID: "an-" + id, Status: status}
	}
	return out, nil
}

// The state carries the current goal period's slots, the sources attached,
// ready and analysed through the campaign's lens, and the calendar's
// problems.
func TestSnapshot_PeriodSourcesCalendar(t *testing.T) {
	perPeriod := 3
	c := timelineCampaign()
	c.ID = "c1"
	c.EstimatedPostCount, c.GoalCadence = &perPeriod, "week"
	c.TargetPlatforms = models.CampaignPlatforms{{ID: "pl1"}}
	c.AssetIDs = models.StringSlice{"a1", "a2", "a3"}
	var lens *string
	var columns []string
	repos := CampaignAssistantRepos{
		Posts: datedRows{columns: &columns, posts: []models.Post{
			{ID: "p1", Status: models.PostStatusDraft, PlatformID: "pl1", ScheduledAt: new(day("2026-01-02"))},
			// Three on one day crowd it.
			{ID: "p2", Status: models.PostStatusDraft, PlatformID: "pl1", ScheduledAt: new(day("2026-01-09"))},
			{ID: "p3", Status: models.PostStatusDraft, PlatformID: "pl1", ScheduledAt: new(day("2026-01-09").Add(time.Hour))},
			{ID: "p4", Status: models.PostStatusDraft, PlatformID: "pl1", ScheduledAt: new(day("2026-01-09").Add(2 * time.Hour))},
		}},
		Assets:   readyAssets{all: []models.Asset{{ID: "a1", Status: models.AssetStatusReady}, {ID: "a2", Status: models.AssetStatusReady}, {ID: "a3", Status: models.AssetStatusFailed}}},
		Analyses: lensDigests{ready: []string{"a1"}, lens: &lens},
	}
	s := snapshot(t.Context(), repos, c, day("2026-01-02"))

	if p := s.Goal.Period; p == nil || p.Target != 3 || p.Planned != 1 || p.Left != 2 {
		t.Fatalf("period = %+v", s.Goal.Period)
	}
	if src := s.Sources; src == nil || src.WholeBank || src.Attached != 3 || src.Ready != 2 || src.Analysed != 1 || lens == nil || *lens != "c1" {
		t.Fatalf("sources = %+v, lens = %v", s.Sources, lens)
	}
	// The calendar check reads the posts' sources and accounts too.
	for _, col := range []string{"used_asset_ids", "social_account_id", "scheduled_at"} {
		if !slices.Contains(columns, col) {
			t.Fatalf("columns = %v, want %s", columns, col)
		}
	}
	if s.Calendar == nil || s.Calendar.Total == 0 || len(s.Calendar.Top) == 0 ||
		!slices.ContainsFunc(s.Calendar.Kinds, func(k state.KindCount) bool { return k.Kind == "crowded day" }) {
		t.Fatalf("calendar = %+v", s.Calendar)
	}
}
