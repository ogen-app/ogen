package post_assistant

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/firebase/genkit/go/ai"
	"github.com/firebase/genkit/go/genkit"

	"github.com/ogen-app/ogen/src/domain/modelconfig"
	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/domain/viewcontext"
	"github.com/ogen-app/ogen/src/genkit/flows/assistant"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/fakemodel"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/usecase/post_actions/restore"
	"github.com/ogen-app/ogen/src/usecase/post_actions/schedule"
	"github.com/ogen-app/ogen/src/usecase/viewresolve"
)

type oneCampaign struct {
	repository.CampaignRepository
	c *models.Campaign
}

func (r oneCampaign) GetByID(context.Context, string) (*models.Campaign, error) { return r.c, nil }

type memVersions struct {
	repository.PostVersionRepository
	mu   sync.Mutex
	rows []models.PostVersion
}

func (m *memVersions) CountByPostID(context.Context, string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.rows), nil
}

func (m *memVersions) Create(_ context.Context, v *models.PostVersion) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rows = append(m.rows, *v)
	return nil
}

func (m *memVersions) CreateNext(_ context.Context, v *models.PostVersion) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	v.VersionNumber = len(m.rows) + 1
	m.rows = append(m.rows, *v)
	return nil
}

func (m *memVersions) ListByPostID(context.Context, string) ([]models.PostVersion, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.rows), nil
}

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

func (m *memMessages) ListRecent(context.Context, models.AssistantScopeKind, string, int) ([]models.AssistantMessage, error) {
	return nil, nil
}

type memProposals struct {
	repository.AssistantProposalRepository
	rows []*models.AssistantProposal
}

func (m *memProposals) Create(_ context.Context, p *models.AssistantProposal) error {
	m.rows = append(m.rows, p)
	return nil
}

func (m *memProposals) ListOpen(context.Context, models.AssistantScopeKind, string, time.Time) ([]models.AssistantProposal, error) {
	return nil, nil
}

// Only schedulePost of the post pack's offers makes no model call once
// confirmed, so it alone is confirmable past the spend cap.
func TestDefineTools_ScheduleIsFree(t *testing.T) {
	for _, s := range defineTools(genkit.Init(t.Context())) {
		if s.Free != (s.Name == "schedulePost") {
			t.Errorf("%s: Free = %v", s.Name, s.Free)
		}
	}
}

// A turn in a post's thread, end to end: the orchestrator forwards the
// change to editPost, the writer's copy streams and is saved as a version,
// and the post pack reports the edit; a schedule waits for the user's yes.
func TestPack_EditThenScheduleInThePostThread(t *testing.T) {
	g := genkit.Init(t.Context())
	posts := &savedPosts{post: &models.Post{ID: "p1", CampaignID: "c1", Status: models.PostStatusDraft, Content: "The old copy."}}
	versions := &memVersions{}
	repos := PostAssistantRepos{Posts: posts, Versions: versions, Campaigns: oneCampaign{c: &models.Campaign{ID: "c1", Name: "Spring"}}}
	pk, err := NewPack(g, PostAssistantFlowConfig{ScheduleService: &schedule.Service{}}, repos)
	if err != nil {
		t.Fatal(err)
	}
	proposals := &memProposals{}
	a, err := assistant.Init(g, assistant.FlowConfig{}, assistant.Repos{Messages: &memMessages{}, Proposals: proposals}, pk)
	if err != nil {
		t.Fatal(err)
	}
	v, err := viewcontext.Parse([]byte(`{"version":1,"screen":"post","scope":{"kind":"post","id":"p1"}}`))
	if err != nil {
		t.Fatal(err)
	}
	view := &viewresolve.Resolved{View: v}
	ctx := context.WithValue(t.Context(), logging.UserIDKey, "u1")

	orchestrator := fakemodel.Define(g, "orchestrator",
		fakemodel.Tools(fakemodel.Call{Name: "editPost", Input: map[string]any{"instruction": "make it punchier", "mode": "edit", "saveVersion": true, "versionNote": "Punchier"}}),
		fakemodel.Answer(`{"explanation":"Made it punchier.","action":"answered"}`))
	writer := fakemodel.Define(g, "writer", fakemodel.Answer("The punchy copy."))
	fakemodel.RouteEach(t.Context(), map[fakemodel.Slot]*fakemodel.Model{
		{Flow: modelconfig.FlowAssistant, Slot: modelconfig.SlotOrchestrator}: orchestrator,
		{Flow: modelconfig.FlowAssistant, Slot: modelconfig.SlotWriter}:       writer,
	})

	var events []assistant.SSEEventKind
	resp, err := a.Run(ctx, assistant.Request{Instruction: "make it punchier", View: view}, func(k assistant.SSEEventKind, _ any) { events = append(events, k) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(lastUserText(orchestrator), `<post id="p1">`) || !strings.Contains(lastUserText(orchestrator), "The old copy.") {
		t.Fatalf("the post block isn't in the message: %q", lastUserText(orchestrator))
	}
	r, _ := resp.Results[PackName].(*PostAssistantResponse)
	if r == nil || r.Action != actionEdited || r.UpdatedContent != "The punchy copy." || !r.SaveVersion {
		t.Fatalf("result = %+v", r)
	}
	if len(posts.saved) != 1 || posts.saved[0].Content != "The punchy copy." {
		t.Fatalf("saved = %+v", posts.saved)
	}
	// Version 1 is the user's copy, kept before the first change; 2 is the edit.
	if len(versions.rows) != 2 || versions.rows[0].Content != "The old copy." || versions.rows[1].Note != "Punchier" {
		t.Fatalf("versions = %+v", versions.rows)
	}
	if !slices.Contains(events, assistant.SSEEventKind(SSEEventContentDelta)) {
		t.Fatalf("events = %v; want the writer's copy streamed", events)
	}

	// A schedule is offered, not run.
	orchestrator = fakemodel.Define(g, "orchestrator2",
		fakemodel.Tools(fakemodel.Call{Name: "schedulePost", Input: map[string]any{"scheduledAt": "2030-06-15T09:00:00Z", "allowPromote": true}}),
		fakemodel.Answer(`{"explanation":"Ready to schedule for 15 June, 09:00.","action":"answered"}`))
	fakemodel.Route(t.Context(), orchestrator, fakemodel.Slot{Flow: modelconfig.FlowAssistant, Slot: modelconfig.SlotOrchestrator})
	resp, err = a.Run(ctx, assistant.Request{Instruction: "schedule it for June 15 at 9", View: view}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Proposals) != 1 || proposals.rows[0].Tool != "schedulePost" || !strings.Contains(proposals.rows[0].Summary, "Jun 15, 2030") {
		t.Fatalf("proposals = %+v", resp.Proposals)
	}
	// The offer names its post, so confirming it elsewhere schedules this one.
	if got := proposals.rows[0].Args["postId"]; got != "p1" {
		t.Fatalf("offer args = %v, want postId p1", proposals.rows[0].Args)
	}

	// A time that has passed is refused before it becomes an offer.
	orchestrator = fakemodel.Define(g, "orchestrator3",
		fakemodel.Tools(fakemodel.Call{Name: "schedulePost", Input: map[string]any{"scheduledAt": "2020-06-15T09:00:00Z"}}),
		fakemodel.Answer(`{"explanation":"That time has passed.","action":"answered"}`))
	fakemodel.Route(t.Context(), orchestrator, fakemodel.Slot{Flow: modelconfig.FlowAssistant, Slot: modelconfig.SlotOrchestrator})
	resp, err = a.Run(ctx, assistant.Request{Instruction: "schedule it for June 15, 2020", View: view}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Proposals) != 0 || len(proposals.rows) != 1 || !strings.Contains(toolOutput(t, orchestrator, "schedulePost"), `"status":"refused"`) {
		t.Fatalf("proposals = %+v, output = %s", resp.Proposals, toolOutput(t, orchestrator, "schedulePost"))
	}
}

// A second edit in a turn is written over the first: the writer is given
// the post as the first edit left it.
func TestPack_SecondEditSeesTheFirst(t *testing.T) {
	g := genkit.Init(t.Context())
	posts := &savedPosts{post: &models.Post{ID: "p2", CampaignID: "c1", Status: models.PostStatusDraft, Content: "The old copy."}}
	repos := PostAssistantRepos{Posts: posts, Versions: &memVersions{}, Campaigns: oneCampaign{c: &models.Campaign{ID: "c1", Name: "Spring"}}}
	pk, err := NewPack(g, PostAssistantFlowConfig{}, repos)
	if err != nil {
		t.Fatal(err)
	}
	a, err := assistant.Init(g, assistant.FlowConfig{}, assistant.Repos{Messages: &memMessages{}}, pk)
	if err != nil {
		t.Fatal(err)
	}
	v, err := viewcontext.Parse([]byte(`{"version":1,"screen":"post","scope":{"kind":"post","id":"p2"}}`))
	if err != nil {
		t.Fatal(err)
	}
	orchestrator := fakemodel.Define(g, "orchestrator",
		fakemodel.Tools(fakemodel.Call{Name: "editPost", Input: map[string]any{"instruction": "make it punchier"}}),
		fakemodel.Tools(fakemodel.Call{Name: "editPost", Input: map[string]any{"instruction": "now shorter"}}),
		fakemodel.Answer(`{"explanation":"Done.","action":"answered"}`))
	writer := fakemodel.Define(g, "writer", fakemodel.Answer("The first edit."), fakemodel.Answer("The second edit."))
	fakemodel.RouteEach(t.Context(), map[fakemodel.Slot]*fakemodel.Model{
		{Flow: modelconfig.FlowAssistant, Slot: modelconfig.SlotOrchestrator}: orchestrator,
		{Flow: modelconfig.FlowAssistant, Slot: modelconfig.SlotWriter}:       writer,
	})
	ctx := context.WithValue(t.Context(), logging.UserIDKey, "u1")
	resp, err := a.Run(ctx, assistant.Request{Instruction: "punchier, then shorter", View: &viewresolve.Resolved{View: v}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	reqs := writer.Requests()
	if len(reqs) != 2 || !strings.Contains(reqs[1].Messages[0].Text(), "The first edit.") || strings.Contains(reqs[1].Messages[0].Text(), "The old copy.") {
		t.Fatalf("second writer call doesn't see the first edit: %q", reqs[len(reqs)-1].Messages[0].Text())
	}
	if r, _ := resp.Results[PackName].(*PostAssistantResponse); r == nil || r.UpdatedContent != "The second edit." {
		t.Fatalf("result = %+v", r)
	}
}

// The result's content is what the turn's later write left: an edit after a
// restore reports the edit.
func TestResult_ReportsTheLatestContent(t *testing.T) {
	st := &requestState{
		editResult:    &editResult{Content: "The edit."},
		restoreResult: &restore.Result{Post: &models.Post{Content: "Version 2."}, RestoredFromVersion: 2, NewVersionNumber: 3},
		content:       "The edit.",
	}
	if r := st.result(); r.UpdatedContent != "The edit." {
		t.Fatalf("updatedContent = %q", r.UpdatedContent)
	}
}

// toolOutput is the JSON of the last result of tool the model was given.
func toolOutput(t *testing.T, m *fakemodel.Model, tool string) string {
	t.Helper()
	out := ""
	for _, tr := range m.ToolResponses() {
		if tr.Name == tool {
			b, err := json.Marshal(tr.Output)
			if err != nil {
				t.Fatal(err)
			}
			out = string(b)
		}
	}
	return out
}

func lastUserText(m *fakemodel.Model) string {
	req := m.Requests()[0]
	for _, msg := range slices.Backward(req.Messages) {
		if msg.Role == ai.RoleUser {
			return msg.Text()
		}
	}
	return ""
}
