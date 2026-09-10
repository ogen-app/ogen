package campaign_assistant

import (
	"context"
	"fmt"
	"testing"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/genkit/flows/draft_post"
	"github.com/ogen-app/ogen/src/infra/repository"
)

// fakeMessagesRepo is a minimal CampaignAssistantMessageRepository that returns
// a preset conversation history (oldest-first, as the real repo does).
type fakeMessagesRepo struct {
	repository.CampaignAssistantMessageRepository
	msgs []models.CampaignAssistantMessage
}

func (f *fakeMessagesRepo) ListRecentByCampaignID(context.Context, string, int) ([]models.CampaignAssistantMessage, error) {
	return f.msgs, nil
}

func modelMsg(content string) models.CampaignAssistantMessage {
	return models.CampaignAssistantMessage{Role: "model", Content: content}
}
func userMsg(content string) models.CampaignAssistantMessage {
	return models.CampaignAssistantMessage{Role: "user", Content: content}
}

// CON-207 FR2: the source material is the latest *answer* in the chat, pulled
// from the stored JSON envelope's "explanation" field — an override wins, an
// action-confirmation turn (e.g. post_drafted) is skipped, and legacy plain-text
// model messages are used as-is.
func TestResolveDraftSource(t *testing.T) {
	st := func(msgs []models.CampaignAssistantMessage) *requestState {
		return &requestState{campaignID: "c1", repos: CampaignAssistantRepos{Messages: &fakeMessagesRepo{msgs: msgs}}}
	}

	// Override always wins, no history consulted.
	if got, _ := resolveDraftSource(t.Context(), st(nil), "  pasted  "); got != "pasted" {
		t.Fatalf("override = %q, want pasted", got)
	}

	// Latest answered explanation is used.
	got, err := resolveDraftSource(t.Context(), st([]models.CampaignAssistantMessage{
		userMsg("what do the assets say?"),
		modelMsg(`{"action":"answered","explanation":"the research"}`),
	}), "")
	if err != nil || got != "the research" {
		t.Fatalf("answered = %q err=%v", got, err)
	}

	// A post_drafted confirmation is skipped in favour of the earlier answer.
	got, _ = resolveDraftSource(t.Context(), st([]models.CampaignAssistantMessage{
		modelMsg(`{"action":"answered","explanation":"research A"}`),
		userMsg("draft it"),
		modelMsg(`{"action":"post_drafted","explanation":"I drafted 1 post."}`),
	}), "")
	if got != "research A" {
		t.Fatalf("skip-confirmation = %q, want research A", got)
	}

	// No prior research → empty (the tool then declines).
	if got, _ := resolveDraftSource(t.Context(), st([]models.CampaignAssistantMessage{userMsg("hi")}), ""); got != "" {
		t.Fatalf("no-research = %q, want empty", got)
	}

	// Legacy plain-text model message (not JSON-wrapped) is used directly.
	if got, _ := resolveDraftSource(t.Context(), st([]models.CampaignAssistantMessage{modelMsg("plain answer")}), ""); got != "plain answer" {
		t.Fatalf("legacy = %q, want plain answer", got)
	}
}

// stubDraftPost records each request and returns req.Count synthetic posts.
func stubDraftPost(calls *[]draft_post.DraftPostRequest) func(context.Context, draft_post.DraftPostRequest, draft_post.OnEventFunc) (*draft_post.DraftPostResponse, error) {
	return func(_ context.Context, req draft_post.DraftPostRequest, _ draft_post.OnEventFunc) (*draft_post.DraftPostResponse, error) {
		*calls = append(*calls, req)
		posts := make([]draft_post.DraftedPost, req.Count)
		for i := range posts {
			posts[i] = draft_post.DraftedPost{PostID: fmt.Sprintf("%s-%d", req.PlatformID, i), PlatformID: req.PlatformID, PublishDate: req.WindowStart}
		}
		return &draft_post.DraftPostResponse{Posts: posts}, nil
	}
}

func newDraftState(msgs []models.CampaignAssistantMessage, calls *[]draft_post.DraftPostRequest) *requestState {
	return &requestState{
		campaignID:    "c1",
		campaign:      timelineCampaign(),
		instruction:   "make it punchy",
		repos:         CampaignAssistantRepos{Messages: &fakeMessagesRepo{msgs: msgs}},
		draftPost:     stubDraftPost(calls),
		maxDraftPosts: 5,
	}
}

// No research in the chat and no override → the tool declines without running
// the flow (CON-207 §9).
func TestToolDraftPost_DeclinesWithoutSource(t *testing.T) {
	var calls []draft_post.DraftPostRequest
	st := newDraftState([]models.CampaignAssistantMessage{userMsg("hi")}, &calls)
	ctx := withRequestState(t.Context(), st)

	out, err := toolDraftPost(ctx, DraftPostInput{Platforms: []string{"LinkedIn"}, Count: 1})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Note != noDraftSourceNote {
		t.Fatalf("note = %q, want the no-source note", out.Note)
	}
	if len(calls) != 0 {
		t.Fatalf("flow must not run without source, got %d calls", len(calls))
	}
	if st.draftPostResult != nil {
		t.Fatal("draftPostResult must stay nil on decline")
	}
	// The decline must NOT consume the turn's heavy-action slot (CON-213): a later
	// valid heavy tool call must still be able to reserve and run.
	if !st.reserveHeavyAction() {
		t.Fatal("a no-source decline must leave the heavy-action slot available")
	}
}

// The per-call cap is a TOTAL budget across platforms: count=3 over two
// platforms with max 5 yields 3 + 2 and reports clamped (CON-207 §10).
func TestToolDraftPost_BudgetAcrossPlatforms(t *testing.T) {
	var calls []draft_post.DraftPostRequest
	st := newDraftState([]models.CampaignAssistantMessage{
		modelMsg(`{"action":"answered","explanation":"the research"}`),
	}, &calls)
	ctx := withRequestState(t.Context(), st)

	out, err := toolDraftPost(ctx, DraftPostInput{Platforms: []string{"LinkedIn", "Threads"}, Count: 3})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.PostCount != 5 || !out.Clamped {
		t.Fatalf("out = {PostCount:%d Clamped:%v}, want {5 true}", out.PostCount, out.Clamped)
	}
	if len(calls) != 2 || calls[0].Count != 3 || calls[1].Count != 2 {
		t.Fatalf("flow calls counts = [%d %d], want [3 2]", calls[0].Count, calls[1].Count)
	}
	// Source + steering are threaded to the flow.
	if calls[0].SourceMaterial != "the research" || calls[0].Instruction != "make it punchy" {
		t.Fatalf("call[0] source=%q instruction=%q", calls[0].SourceMaterial, calls[0].Instruction)
	}
	if st.draftPostResult == nil || st.draftPostResult.PostCount != 5 {
		t.Fatalf("draftPostResult = %+v", st.draftPostResult)
	}
}

// CON-215 parity: a user-correctable input (past date, non-target platform)
// fails soft — zero posts + a warning, no flow call, draftPostResult unset, and
// the heavy-action slot left free — so the turn stays conversational instead of
// aborting with a raw "model call failed".
func TestToolDraftPost_SoftFailsUserInput(t *testing.T) {
	research := []models.CampaignAssistantMessage{
		modelMsg(`{"action":"answered","explanation":"the research"}`),
	}

	// A past publish date.
	var calls []draft_post.DraftPostRequest
	st := newDraftState(research, &calls)
	ctx := withRequestState(t.Context(), st)
	out, err := toolDraftPost(ctx, DraftPostInput{Platforms: []string{"LinkedIn"}, Count: 1, PublishDate: "2020-01-01"})
	if err != nil {
		t.Fatalf("past date must not error: %v", err)
	}
	if out.PostCount != 0 || len(out.Warnings) == 0 {
		t.Fatalf("past date: out = {PostCount:%d Warnings:%v}, want zero posts + a warning", out.PostCount, out.Warnings)
	}
	if len(calls) != 0 || st.draftPostResult != nil {
		t.Fatal("past date must not run the flow or set draftPostResult")
	}
	if !st.reserveHeavyAction() {
		t.Fatal("a soft failure must leave the heavy-action slot available")
	}

	// A non-target platform.
	calls = nil
	st = newDraftState(research, &calls)
	ctx = withRequestState(t.Context(), st)
	out, err = toolDraftPost(ctx, DraftPostInput{Platforms: []string{"TikTok"}, Count: 1})
	if err != nil {
		t.Fatalf("non-target platform must not error: %v", err)
	}
	if out.PostCount != 0 || len(out.Warnings) == 0 || len(calls) != 0 {
		t.Fatalf("non-target platform: out = %+v, calls = %d", out, len(calls))
	}
}

// An explicit sourceMaterial override bypasses history lookup.
func TestToolDraftPost_SourceOverride(t *testing.T) {
	var calls []draft_post.DraftPostRequest
	st := newDraftState(nil, &calls) // no history at all
	ctx := withRequestState(t.Context(), st)

	if _, err := toolDraftPost(ctx, DraftPostInput{Platforms: []string{"LinkedIn"}, Count: 1, SourceMaterial: "pasted material"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(calls) != 1 || calls[0].SourceMaterial != "pasted material" {
		t.Fatalf("override not used: %+v", calls)
	}
}

// CON-213: once a heavy action ran this turn, draftPost yields the skip note and
// never runs the flow.
func TestToolDraftPost_HeavyLatch(t *testing.T) {
	var calls []draft_post.DraftPostRequest
	st := newDraftState([]models.CampaignAssistantMessage{
		modelMsg(`{"action":"answered","explanation":"the research"}`),
	}, &calls)
	if !st.reserveHeavyAction() {
		t.Fatal("first reservation should win")
	}
	ctx := withRequestState(t.Context(), st)

	out, err := toolDraftPost(ctx, DraftPostInput{Platforms: []string{"LinkedIn"}, Count: 1})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Note != heavySkipNote {
		t.Fatalf("note = %q, want heavy skip note", out.Note)
	}
	if len(calls) != 0 {
		t.Fatal("flow must not run once a heavy action was reserved")
	}
}
