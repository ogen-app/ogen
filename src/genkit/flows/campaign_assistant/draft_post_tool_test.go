package campaign_assistant

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/genkit/flows/draft_post"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/assistantkit"
	"github.com/ogen-app/ogen/src/infra/repository"
)

// fakeMessagesRepo is a minimal AssistantMessageRepository that returns
// a preset conversation history (oldest-first, as the real repo does).
type fakeMessagesRepo struct {
	repository.AssistantMessageRepository
	msgs []models.AssistantMessage
}

func (f *fakeMessagesRepo) ListRecent(context.Context, models.AssistantScopeKind, string, int) ([]models.AssistantMessage, error) {
	return f.msgs, nil
}

func modelMsg(content string) models.AssistantMessage {
	return models.AssistantMessage{Role: "model", Content: content}
}
func userMsg(content string) models.AssistantMessage {
	return models.AssistantMessage{Role: "user", Content: content}
}

// The source material is the latest *answer* in the chat, pulled
// from the stored JSON envelope's "explanation" field — an override wins, an
// action-confirmation turn (e.g. post_drafted) is skipped, and legacy plain-text
// model messages are used as-is.
func TestResolveDraftSource(t *testing.T) {
	st := func(msgs []models.AssistantMessage) *requestState {
		return &requestState{campaignID: "c1", repos: CampaignAssistantRepos{Messages: &fakeMessagesRepo{msgs: msgs}}}
	}

	// Override always wins, no history consulted.
	if got, _ := resolveDraftSource(t.Context(), st(nil), "  pasted  "); got != "pasted" {
		t.Fatalf("override = %q, want pasted", got)
	}

	// Latest answered explanation is used.
	got, err := resolveDraftSource(t.Context(), st([]models.AssistantMessage{
		userMsg("what do the assets say?"),
		modelMsg(`{"action":"answered","explanation":"the research"}`),
	}), "")
	if err != nil || got != "the research" {
		t.Fatalf("answered = %q err=%v", got, err)
	}

	// A post_drafted confirmation is skipped in favour of the earlier answer.
	got, _ = resolveDraftSource(t.Context(), st([]models.AssistantMessage{
		modelMsg(`{"action":"answered","explanation":"research A"}`),
		userMsg("draft it"),
		modelMsg(`{"action":"post_drafted","explanation":"I drafted 1 post."}`),
	}), "")
	if got != "research A" {
		t.Fatalf("skip-confirmation = %q, want research A", got)
	}

	// The assistant stores the pack's action under results: an "answered"
	// turn that drafted is still a confirmation.
	got, err = resolveDraftSource(t.Context(), st([]models.AssistantMessage{
		modelMsg(`{"action":"answered","explanation":"research A"}`),
		userMsg("draft it"),
		modelMsg(`{"action":"answered","explanation":"I drafted 1 post.","results":{"campaign":{"action":"post_drafted"}}}`),
	}), "")
	if err != nil || got != "research A" {
		t.Fatalf("skip-pack-confirmation = %q err=%v, want research A", got, err)
	}

	// No prior research → empty (the tool then declines).
	if got, _ := resolveDraftSource(t.Context(), st([]models.AssistantMessage{userMsg("hi")}), ""); got != "" {
		t.Fatalf("no-research = %q, want empty", got)
	}

	// Legacy plain-text model message (not JSON-wrapped) is used directly.
	if got, _ := resolveDraftSource(t.Context(), st([]models.AssistantMessage{modelMsg("plain answer")}), ""); got != "plain answer" {
		t.Fatalf("legacy = %q, want plain answer", got)
	}
}

// stubDraftPost records each request and returns req.Count synthetic posts.
// Platforms are drafted concurrently, so recording is locked.
func stubDraftPost(calls *[]draft_post.DraftPostRequest) func(context.Context, draft_post.DraftPostRequest, draft_post.OnEventFunc) (*draft_post.DraftPostResponse, error) {
	var mu sync.Mutex
	return func(_ context.Context, req draft_post.DraftPostRequest, _ draft_post.OnEventFunc) (*draft_post.DraftPostResponse, error) {
		mu.Lock()
		*calls = append(*calls, req)
		mu.Unlock()
		posts := make([]draft_post.DraftedPost, req.Count)
		for i := range posts {
			posts[i] = draft_post.DraftedPost{PostID: fmt.Sprintf("%s-%d", req.PlatformID, i), PlatformID: req.PlatformID, PublishDate: req.WindowStart}
		}
		return &draft_post.DraftPostResponse{Posts: posts}, nil
	}
}

func newDraftState(msgs []models.AssistantMessage, calls *[]draft_post.DraftPostRequest) *requestState {
	return &requestState{
		campaignID:    "c1",
		campaign:      timelineCampaign(),
		instruction:   "make it punchy",
		repos:         CampaignAssistantRepos{Messages: &fakeMessagesRepo{msgs: msgs}},
		draftPost:     stubDraftPost(calls),
		maxDraftPosts: 5,
	}
}

// fix 2: a held-back draft stores the research and the steering it was made
// from, so confirming it after newer research, from a "yes", drafts what was
// offered.
func TestToolDraftPost_OfferKeepsItsResearch(t *testing.T) {
	var calls []draft_post.DraftPostRequest
	store := &memProposals{}
	st := newDraftState([]models.AssistantMessage{
		modelMsg(`{"action":"answered","explanation":"research A"}`),
	}, &calls)
	st.repos.Proposals = store
	out := runTool(t, st, "draftPost", map[string]any{"platforms": []string{"LinkedIn"}, "count": 5})
	if out.Status != assistantkit.StatusProposed || len(calls) != 0 {
		t.Fatalf("outcome = %+v, calls = %d", out, len(calls))
	}
	if args := store.rows[0].Args; args["sourceMaterial"] != "research A" || args["instruction"] != "make it punchy" {
		t.Fatalf("stored args = %v", args)
	}

	// Newer research, and the confirming turn's own message.
	later := newDraftState([]models.AssistantMessage{
		modelMsg(`{"action":"answered","explanation":"research A"}`),
		modelMsg(`{"action":"answered","explanation":"research B"}`),
	}, &calls)
	later.instruction = "yes"
	if _, err := newTestTools(t).reg.RunConfirmed(later.bind(t.Context()), "draftPost", map[string]any(store.rows[0].Args)); err != nil {
		t.Fatal(err)
	}
	if len(calls) == 0 || calls[0].SourceMaterial != "research A" || calls[0].Instruction != "make it punchy" {
		t.Fatalf("drafted from %+v", calls)
	}
}

// No research in the chat and no override → the tool declines without running
// the flow.
func TestToolDraftPost_DeclinesWithoutSource(t *testing.T) {
	var calls []draft_post.DraftPostRequest
	st := newDraftState([]models.AssistantMessage{userMsg("hi")}, &calls)
	ctx := withRequestState(t.Context(), st)

	out, err := toolDraftPost(ctx, DraftPostInput{Platforms: []string{"LinkedIn"}, Count: 1})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Status != assistantkit.StatusRefused || out.Reason != noDraftSourceNote {
		t.Fatalf("out = %+v, want a refusal with the no-source reason", out)
	}
	if len(calls) != 0 {
		t.Fatalf("flow must not run without source, got %d calls", len(calls))
	}
	if st.draftPostResult != nil {
		t.Fatal("draftPostResult must stay nil on decline")
	}
	// The decline must NOT consume the turn's heavy-action slot: a later
	// valid heavy tool call must still be able to reserve and run.
	if !st.reserveHeavyAction() {
		t.Fatal("a no-source decline must leave the heavy-action slot available")
	}
}

// The per-call cap is a TOTAL budget across platforms: count=3 over two
// platforms with max 5 yields 3 + 2 and reports clamped.
func TestToolDraftPost_BudgetAcrossPlatforms(t *testing.T) {
	var calls []draft_post.DraftPostRequest
	st := newDraftState([]models.AssistantMessage{
		modelMsg(`{"action":"answered","explanation":"the research"}`),
	}, &calls)
	ctx := withRequestState(t.Context(), st)

	out, err := toolDraftPost(ctx, DraftPostInput{Platforms: []string{"LinkedIn", "Threads"}, Count: 3})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !out.Succeeded() || out.Result.PostCount != 5 || !out.Result.Clamped {
		t.Fatalf("out = %+v, want 5 posts, clamped", out)
	}
	if len(calls) != 2 {
		t.Fatalf("flow calls = %d, want 2", len(calls))
	}
	// The platforms are drafted concurrently; the budget follows platform order.
	countByPlatform := map[string]int{calls[0].PlatformID: calls[0].Count, calls[1].PlatformID: calls[1].Count}
	if got := []int{countByPlatform[out.Result.PlatformIDs[0]], countByPlatform[out.Result.PlatformIDs[1]]}; got[0] != 3 || got[1] != 2 {
		t.Fatalf("flow call counts in platform order = %v, want [3 2]", got)
	}
	// Source + steering are threaded to the flow.
	if calls[0].SourceMaterial != "the research" || calls[0].Instruction != "make it punchy" {
		t.Fatalf("call[0] source=%q instruction=%q", calls[0].SourceMaterial, calls[0].Instruction)
	}
	if st.draftPostResult == nil || st.draftPostResult.PostCount != 5 {
		t.Fatalf("draftPostResult = %+v", st.draftPostResult)
	}
}

// One platform the engine refuses doesn't hide the drafts another saved: the
// call succeeds with them, the refusal a warning. Only when every platform
// is refused is the call.
func TestToolDraftPost_PartialRefusalKeepsTheDrafts(t *testing.T) {
	var calls []draft_post.DraftPostRequest
	st := newDraftState([]models.AssistantMessage{
		modelMsg(`{"action":"answered","explanation":"the research"}`),
	}, &calls)
	saved := st.draftPost
	refuse := ""
	var mu sync.Mutex
	st.draftPost = func(ctx context.Context, req draft_post.DraftPostRequest, onEvent draft_post.OnEventFunc) (*draft_post.DraftPostResponse, error) {
		mu.Lock()
		defer mu.Unlock()
		if refuse == "*" || req.PlatformID == refuse {
			return nil, &draft_post.ValidationError{Msg: "the research is too thin for this platform"}
		}
		return saved(ctx, req, onEvent)
	}
	threads := ""
	for _, p := range st.campaign.Platforms {
		if p.Name == "Threads" {
			threads = p.ID
		}
	}
	refuse = threads

	out, err := toolDraftPost(withRequestState(t.Context(), st), DraftPostInput{Platforms: []string{"LinkedIn", "Threads"}, Count: 1})
	if err != nil || !out.Succeeded() || out.Result.PostCount != 1 || st.draftPostResult == nil {
		t.Fatalf("out = %+v, err = %v, result = %+v", out, err, st.draftPostResult)
	}
	if !slices.ContainsFunc(out.Result.Warnings, func(w string) bool { return strings.HasPrefix(w, "Threads: ") }) {
		t.Fatalf("warnings = %v, want the Threads refusal", out.Result.Warnings)
	}

	refuse = "*"
	st = newDraftState([]models.AssistantMessage{modelMsg(`{"action":"answered","explanation":"the research"}`)}, &calls)
	st.draftPost = func(context.Context, draft_post.DraftPostRequest, draft_post.OnEventFunc) (*draft_post.DraftPostResponse, error) {
		return nil, &draft_post.ValidationError{Msg: "the research is too thin for this platform"}
	}
	out, err = toolDraftPost(withRequestState(t.Context(), st), DraftPostInput{Platforms: []string{"LinkedIn"}, Count: 1})
	if err != nil || out.Status != assistantkit.StatusRefused || st.draftPostResult != nil {
		t.Fatalf("all refused: out = %+v, err = %v", out, err)
	}
}

// A user-correctable input (past date, non-target platform) fails soft — a
// refusal with a reason, no flow call, draftPostResult unset, and
// the heavy-action slot left free — so the turn stays conversational instead of
// aborting with a raw "model call failed".
func TestToolDraftPost_SoftFailsUserInput(t *testing.T) {
	research := []models.AssistantMessage{
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
	if out.Status != assistantkit.StatusRefused || out.Reason == "" {
		t.Fatalf("past date: out = %+v, want a refusal with a reason", out)
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
	if out.Status != assistantkit.StatusRefused || out.Reason == "" || len(calls) != 0 {
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

// Once a heavy action ran this turn, draftPost yields the skip note and
// never runs the flow.
func TestToolDraftPost_HeavyLatch(t *testing.T) {
	var calls []draft_post.DraftPostRequest
	st := newDraftState([]models.AssistantMessage{
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
	if !out.Succeeded() || out.Result.Note != heavySkipNote {
		t.Fatalf("out = %+v, want the heavy skip note", out)
	}
	if len(calls) != 0 {
		t.Fatal("flow must not run once a heavy action was reserved")
	}
}
