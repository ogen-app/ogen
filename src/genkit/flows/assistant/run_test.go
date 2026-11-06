package assistant

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/firebase/genkit/go/ai"
	"github.com/firebase/genkit/go/genkit"

	"github.com/ogen-app/ogen/src/domain/modelconfig"
	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/domain/viewcontext"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/assistantkit"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/fakemodel"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/usecase/viewresolve"
)

// memMessages keeps every thread so later turns see earlier ones.
type memMessages struct {
	repository.AssistantMessageRepository
	mu   sync.Mutex
	msgs []models.AssistantMessage
}

func (m *memMessages) CreateBatch(_ context.Context, msgs []*models.AssistantMessage) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, msg := range msgs {
		m.msgs = append(m.msgs, *msg)
	}
	return nil
}

func (m *memMessages) ListRecent(_ context.Context, kind models.AssistantScopeKind, scopeID string, _ int) ([]models.AssistantMessage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []models.AssistantMessage
	for _, msg := range m.msgs {
		if msg.ScopeKind == kind && msg.ScopeID == scopeID {
			out = append(out, msg)
		}
	}
	return out, nil
}

// memViews reloads references from a fixed set of titles.
type memViews struct {
	byKind map[viewcontext.EntityKind]map[string]string
}

func (v memViews) Items(_ context.Context, kind viewcontext.EntityKind, ids []string) ([]viewresolve.Item, error) {
	var out []viewresolve.Item
	for _, id := range ids {
		if t, ok := v.byKind[kind][id]; ok {
			out = append(out, viewresolve.Item{Kind: kind, ID: id, Title: t})
		}
	}
	return out, nil
}

func (memViews) CheckFilter(_ viewcontext.EntityKind, filter string) error {
	if strings.Contains(filter, "nope") {
		return &viewcontext.InvalidError{Field: "filter", Reason: "unknown pile"}
	}
	return nil
}

type memSavedViews map[string]*models.SavedView

func (m memSavedViews) GetByID(_ context.Context, id string) (*models.SavedView, error) {
	if v, ok := m[id]; ok {
		return v, nil
	}
	return nil, sql.ErrNoRows
}

func testViews() memViews {
	return memViews{byKind: map[viewcontext.EntityKind]map[string]string{
		viewcontext.KindCampaign: {"c1": "Spring launch", "c2": "Autumn"},
		viewcontext.KindAsset:    {"a1": "Q3 deck"},
		viewcontext.KindIdea:     {"i1": "Pricing myths", "i2": "Founder story"},
	}}
}

type harness struct {
	t     *testing.T
	g     *genkit.Genkit
	tools *toolSet
	p     *prompts
	repos Repos
	msgs  *memMessages
	n     int
	// payloads are the last turn's event payloads, in order.
	payloads []any
}

func newHarness(t *testing.T, packs ...Pack) *harness {
	t.Helper()
	g := genkit.Init(t.Context())
	msgs := &memMessages{}
	user := "u1"
	repos := Repos{Messages: msgs, Views: testViews(), SavedViews: memSavedViews{
		"sv1":     {ID: "sv1", Surface: "ideas", Name: "Undecided", Shared: true},
		"private": {ID: "private", Surface: "assets", Name: "Mine", CreatedBy: &user},
		"theirs":  {ID: "theirs", Surface: "assets", Name: "Theirs", CreatedBy: new("u2")},
	}}
	tools, err := defineTools(g, repos, packs)
	if err != nil {
		t.Fatal(err)
	}
	p, err := loadPrompts(tools)
	if err != nil {
		t.Fatal(err)
	}
	return &harness{t: t, g: g, tools: tools, p: p, repos: repos, msgs: msgs}
}

// turn runs one message from view against a scripted model and returns the
// response, the event kinds and the model.
func (h *harness) turn(view *viewresolve.Resolved, instruction string, script ...fakemodel.Turn) (*Response, []SSEEventKind, *fakemodel.Model, error) {
	h.t.Helper()
	h.n++
	m := fakemodel.Define(h.g, fmt.Sprint("turn", h.n), script...)
	fakemodel.Route(h.t.Context(), m, fakemodel.Slot{Flow: modelconfig.FlowAssistant, Slot: modelconfig.SlotOrchestrator})
	var events []SSEEventKind
	h.payloads = nil
	ctx := context.WithValue(h.t.Context(), logging.UserIDKey, "u1")
	resp, err := run(ctx, h.g, Request{Instruction: instruction, View: view}, FlowConfig{}, h.repos, h.p, h.tools,
		func(kind SSEEventKind, data any) {
			events = append(events, kind)
			h.payloads = append(h.payloads, data)
		})
	return resp, events, m, err
}

// ideasView is the campaign's Ideas screen with n ideas selected, of total
// matching.
func ideasView(t *testing.T, n, total int) *viewresolve.Resolved {
	t.Helper()
	v, err := viewcontext.Parse([]byte(`{"version":1,"screen":"campaign.ideas","scope":{"kind":"campaign","id":"c1"},
		"selection":{"kind":"idea","allMatching":true},"filter":"origin=asset_analysis"}`))
	if err != nil {
		t.Fatal(err)
	}
	r := &viewresolve.Resolved{View: v, Scope: &viewresolve.Item{Kind: viewcontext.KindCampaign, ID: "c1", Title: "Spring launch"},
		SelectionTotal: total, Summary: fmt.Sprintf(`On campaign.ideas in campaign "Spring launch" · %d ideas selected`, total)}
	for i := range n {
		r.Selection = append(r.Selection, viewresolve.Item{Kind: viewcontext.KindIdea, ID: fmt.Sprint("i", i), Title: fmt.Sprint("Idea ", i)})
	}
	return r
}

// lastText is the text of the request's last user message.
func lastText(req *ai.ModelRequest) string {
	for _, m := range slices.Backward(req.Messages) {
		if m.Role == ai.RoleUser {
			return m.Text()
		}
	}
	return ""
}

func TestRun_AnswersAboutTheSelection(t *testing.T) {
	h := newHarness(t)
	view := ideasView(t, 30, 30)
	resp, events, m, err := h.turn(view, "what do these have in common?",
		fakemodel.Tools(fakemodel.Call{Name: "getSelection", Input: map[string]any{"offset": 20}}),
		fakemodel.Answer(`{"explanation":"They are all about pricing.","action":"answered"}`))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Explanation != "They are all about pricing." || resp.Action != actionAnswered {
		t.Fatalf("resp = %+v", resp)
	}
	for _, want := range []SSEEventKind{SSEEventToolCall, SSEEventToolResult, SSEEventExplanationDelta, SSEEventComplete} {
		if !slices.Contains(events, want) {
			t.Fatalf("events %v lack %s", events, want)
		}
	}

	// The <view> block opens the user's message, after the cache breakpoints;
	// it lists the first 20 and points at getSelection for the rest.
	first := m.Requests()[0]
	prompt := lastText(first)
	if !strings.HasPrefix(prompt, "<view>") || !strings.Contains(prompt, `idea "Idea 19" (id i19)`) ||
		strings.Contains(prompt, "Idea 20") || !strings.Contains(prompt, "call getSelection for the rest") ||
		!strings.HasSuffix(prompt, "what do these have in common?") {
		t.Fatalf("user message = %q", prompt)
	}
	if strings.Contains(first.Messages[0].Text(), "Spring launch") {
		t.Fatal("the system prompt carries per-turn state, so it can't be cached")
	}

	// The tool paged from offset 20.
	var page assistantkit.Outcome[SelectionPage]
	b, _ := json.Marshal(m.ToolResponses()[0].Output)
	if err := json.Unmarshal(b, &page); err != nil || len(page.Result.Items) != 10 || page.Result.Items[0].ID != "i20" {
		t.Fatalf("getSelection = %s (%v)", b, err)
	}

	// The turn is stored on the campaign's thread, the screen on the user's message.
	if len(h.msgs.msgs) != 2 {
		t.Fatalf("stored %d messages", len(h.msgs.msgs))
	}
	user, model := h.msgs.msgs[0], h.msgs.msgs[1]
	if user.ScopeKind != models.AssistantScopeCampaign || user.ScopeID != "c1" || user.ViewContext["screen"] != "campaign.ideas" ||
		user.CreatedBy != "u1" || model.ViewContext != nil || !model.CreatedAt.After(user.CreatedAt) {
		t.Fatalf("stored %+v / %+v", user, model)
	}
}

func TestRun_ThreadContinuesAcrossScreens(t *testing.T) {
	h := newHarness(t)
	if _, _, _, err := h.turn(ideasView(t, 2, 2), "summarise these", fakemodel.Answer(`{"explanation":"Two pricing ideas.","action":"answered"}`)); err != nil {
		t.Fatal(err)
	}
	assets, err := viewcontext.Parse([]byte(`{"version":1,"screen":"campaign.assets","scope":{"kind":"campaign","id":"c1"}}`))
	if err != nil {
		t.Fatal(err)
	}
	_, _, m, err := h.turn(&viewresolve.Resolved{View: assets, Summary: "On campaign.assets"}, "and which assets back them?",
		fakemodel.Answer(`{"explanation":"The Q3 deck.","action":"answered"}`))
	if err != nil {
		t.Fatal(err)
	}
	msgs := m.Requests()[0].Messages
	// system, the earlier user turn, the earlier model turn, this turn.
	if len(msgs) != 4 || !strings.Contains(msgs[2].Text(), "Two pricing ideas.") || !strings.HasPrefix(lastText(m.Requests()[0]), "<view>\nOn campaign.assets") {
		t.Fatalf("messages = %d, last = %q", len(msgs), lastText(m.Requests()[0]))
	}

	// Another scope is another thread.
	ws, _ := viewcontext.Parse([]byte(`{"version":1,"screen":"ideas","scope":{"kind":"workspace"}}`))
	_, _, m, err = h.turn(&viewresolve.Resolved{View: ws, Summary: "On ideas"}, "hi", fakemodel.Answer(`{"explanation":"Hi!","action":"answered"}`))
	if err != nil || len(m.Requests()[0].Messages) != 2 {
		t.Fatalf("workspace thread saw %d messages (err %v)", len(m.Requests()[0].Messages), err)
	}
}

func TestRun_Replies(t *testing.T) {
	cases := map[string]struct {
		reply      string
		action     string
		wantAIFail bool
	}{
		"prose":    {reply: "Sure, they share a theme.", action: actionAnswered},
		"declined": {reply: `{"explanation":"I can only help with your content.","action":"declined"}`, action: actionDeclined},
		"unknown":  {reply: `{"explanation":"Done.","action":"posts_generated"}`, action: actionAnswered},
		"broken":   {reply: `{"nope": 1}`, wantAIFail: true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			resp, _, _, err := h.turn(ideasView(t, 1, 1), "hello", fakemodel.Answer(tc.reply))
			if tc.wantAIFail {
				if _, ok := errors.AsType[*AIError](err); !ok {
					t.Fatalf("err = %v, want *AIError", err)
				}
				return
			}
			if err != nil || resp.Action != tc.action {
				t.Fatalf("resp = %+v, err = %v", resp, err)
			}
		})
	}
}

func TestRun_Validation(t *testing.T) {
	h := newHarness(t)
	if _, _, _, err := h.turn(ideasView(t, 1, 1), "  "); !isValidation(err) {
		t.Fatalf("blank instruction: %v", err)
	}
	if _, _, _, err := h.turn(nil, "hi"); !isValidation(err) {
		t.Fatalf("no view: %v", err)
	}
}

func isValidation(err error) bool {
	_, ok := errors.AsType[*ValidationError](err)
	return ok
}
