package campaign_assistant

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/fakemodel"
	"github.com/ogen-app/ogen/src/infra/repository"
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
