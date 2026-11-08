package campaign_assistant

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/genkit/flows/content_plan"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/usecase/notify"
)

func storeOffer(t *testing.T, h *runnerHarness, id string) {
	t.Helper()
	if err := h.proposals.Create(t.Context(), &models.AssistantProposal{
		ID: id, ScopeKind: models.AssistantScopeCampaign, ScopeID: "c1", Tool: "generatePosts",
		Args:    models.JSONMap{"platforms": []any{"Threads"}, "count": float64(10)},
		Summary: "Add 10 draft posts for Threads", ExpiresAt: farFuture,
	}); err != nil {
		t.Fatal(err)
	}
}

// The card's confirm button runs the offer without a model call, records the
// answer in the conversation, and cannot run it twice.
func TestAnswerProposal_ConfirmFromCard(t *testing.T) {
	h := newRunnerHarness(t)
	storeOffer(t, h, "prop-1")

	var events []SSEEventKind
	resp, err := answerProposal(t.Context(), h.cfg, h.repos, h.tools, AnswerProposalRequest{ProposalID: "prop-1", Confirm: true},
		func(kind SSEEventKind, _ any) { events = append(events, kind) })
	if err != nil {
		t.Fatal(err)
	}
	if resp.Action != actionPostsGenerated || len(h.generated) != 1 || h.generated[0].Count != 10 {
		t.Fatalf("action = %q, generated = %+v", resp.Action, h.generated)
	}
	if !has(events, SSEEventProposalResolved) || events[len(events)-1] != SSEEventComplete {
		t.Fatalf("events = %v", events)
	}
	msgs, _ := h.repos.Messages.ListRecentByCampaignID(t.Context(), "c1", 10)
	if len(msgs) != 2 || msgs[0].Content != "Confirmed: Add 10 draft posts for Threads" {
		t.Fatalf("conversation = %+v", msgs)
	}

	_, err = answerProposal(t.Context(), h.cfg, h.repos, h.tools, AnswerProposalRequest{ProposalID: "prop-1", Confirm: true}, nil)
	if _, ok := errors.AsType[*ProposalConflictError](err); !ok || len(h.generated) != 1 {
		t.Fatalf("second confirm: err = %v, generated = %d", err, len(h.generated))
	}
}

func TestAnswerProposal_DismissFromCard(t *testing.T) {
	h := newRunnerHarness(t)
	storeOffer(t, h, "prop-1")
	resp, err := answerProposal(t.Context(), h.cfg, h.repos, h.tools, AnswerProposalRequest{ProposalID: "prop-1"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Action != actionAnswered || len(h.generated) != 0 || h.proposals.rows[0].Status != models.ProposalDismissed {
		t.Fatalf("action = %q, generated = %d, proposal = %+v", resp.Action, len(h.generated), h.proposals.rows[0])
	}
	msgs, _ := h.repos.Messages.ListRecentByCampaignID(t.Context(), "c1", 10)
	if len(msgs) != 2 || msgs[0].Content != "Dismissed: Add 10 draft posts for Threads" {
		t.Fatalf("conversation = %+v", msgs)
	}
	var stored CampaignAssistantResponse
	if err := json.Unmarshal([]byte(msgs[1].Content), &stored); err != nil || stored.Action != resp.Action || stored.Explanation != resp.Explanation {
		t.Fatalf("stored reply = %s (%v), want %+v", msgs[1].Content, err, resp)
	}
}

// Work that fails after the offer was answered still leaves the answer in the
// conversation and tells the client the offer is resolved.
func TestAnswerProposal_ConfirmedWorkFails(t *testing.T) {
	h := newRunnerHarness(t)
	h.cfg.GeneratePosts = func(context.Context, content_plan.GeneratePostsRequest, content_plan.OnEventFunc) (*content_plan.ContentPlanResponse, error) {
		return nil, errors.New("engine down")
	}
	storeOffer(t, h, "prop-1")

	var events []SSEEventKind
	_, err := answerProposal(t.Context(), h.cfg, h.repos, h.tools, AnswerProposalRequest{ProposalID: "prop-1", Confirm: true},
		func(kind SSEEventKind, _ any) { events = append(events, kind) })
	if err == nil {
		t.Fatal("want the work's error")
	}
	if !has(events, SSEEventProposalResolved) || has(events, SSEEventComplete) {
		t.Fatalf("events = %v", events)
	}
	msgs, _ := h.repos.Messages.ListRecentByCampaignID(t.Context(), "c1", 10)
	if len(msgs) != 2 || msgs[0].Content != "Confirmed: Add 10 draft posts for Threads" {
		t.Fatalf("conversation = %+v", msgs)
	}
}

func TestAnswerProposal_Unknown(t *testing.T) {
	h := newRunnerHarness(t)
	_, err := answerProposal(t.Context(), h.cfg, h.repos, h.tools, AnswerProposalRequest{ProposalID: "nope", Confirm: true}, nil)
	if !errors.Is(err, ErrProposalNotFound) {
		t.Fatalf("err = %v, want ErrProposalNotFound", err)
	}
}

// The offered work can still be refused when it runs, e.g. its date has
// passed since it was proposed. That is a conflict, not a success.
func TestAnswerProposal_WorkRefused(t *testing.T) {
	h := newRunnerHarness(t)
	if err := h.proposals.Create(t.Context(), &models.AssistantProposal{
		ID: "stale", ScopeKind: models.AssistantScopeCampaign, ScopeID: "c1", Tool: "generatePosts",
		Args:    models.JSONMap{"platforms": []any{"Threads"}, "count": float64(10), "windowStart": "2020-01-01", "windowEnd": "2020-01-01"},
		Summary: "Add 10 draft posts for Threads", ExpiresAt: farFuture,
	}); err != nil {
		t.Fatal(err)
	}
	_, err := answerProposal(t.Context(), h.cfg, h.repos, h.tools, AnswerProposalRequest{ProposalID: "stale", Confirm: true}, nil)
	if _, ok := errors.AsType[*ProposalConflictError](err); !ok || len(h.generated) != 0 {
		t.Fatalf("err = %v, generated = %d", err, len(h.generated))
	}
}

// notifRows keeps the notifications the flow drops.
type notifRows struct {
	repository.NotificationRepository
	rows []*models.Notification
}

func (r *notifRows) Insert(_ context.Context, n *models.Notification) (bool, error) {
	r.rows = append(r.rows, n)
	return true, nil
}

// A content plan confirmed from its card tells the owner it is ready, as one
// run from the chat does.
func TestAnswerProposal_ContentPlanNotifiesOwner(t *testing.T) {
	h := newRunnerHarness(t)
	c := timelineCampaign()
	c.ID, c.TenantID, c.CreatedBy = "c1", "t1", "owner-1"
	h.repos.Campaigns = oneCampaign{c: c}
	notes := &notifRows{}
	h.cfg.Notifier = notify.New(notes, nil)
	h.cfg.ContentPlan = func(context.Context, content_plan.ContentPlanRequest, content_plan.OnEventFunc) (*content_plan.ContentPlanResponse, error) {
		return &content_plan.ContentPlanResponse{Ideas: make([]content_plan.DraftPost, 3)}, nil
	}
	if err := h.proposals.Create(t.Context(), &models.AssistantProposal{
		ID: "plan", ScopeKind: models.AssistantScopeCampaign, ScopeID: "c1", Tool: "runContentPlan",
		Args: models.JSONMap{}, Summary: "Rebuild the content plan", ExpiresAt: farFuture,
	}); err != nil {
		t.Fatal(err)
	}

	resp, err := answerProposal(t.Context(), h.cfg, h.repos, h.tools, AnswerProposalRequest{ProposalID: "plan", Confirm: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Action != actionContentPlan || len(notes.rows) != 1 || notes.rows[0].Type != "campaign.content_plan_ready" || notes.rows[0].UserID != "owner-1" {
		t.Fatalf("action = %q, notifications = %+v", resp.Action, notes.rows)
	}
}
