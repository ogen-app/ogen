package campaign_assistant

import (
	"context"
	"database/sql"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/genkit/flows/content_plan"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/assistantkit"
	"github.com/ogen-app/ogen/src/infra/repository"
)

// memProposals is an in-memory AssistantProposalRepository.
type memProposals struct {
	mu   sync.Mutex
	rows []*models.AssistantProposal
}

func (m *memProposals) Create(_ context.Context, p *models.AssistantProposal) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if p.Status == "" {
		p.Status = models.ProposalPending
	}
	m.rows = append(m.rows, p)
	return nil
}

func (m *memProposals) GetByID(_ context.Context, id string) (*models.AssistantProposal, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, p := range m.rows {
		if p.ID == id {
			return p, nil
		}
	}
	return nil, sql.ErrNoRows
}

func (m *memProposals) ListOpen(_ context.Context, kind models.AssistantScopeKind, scopeID string, now time.Time) ([]models.AssistantProposal, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []models.AssistantProposal
	for _, p := range m.rows {
		if p.ScopeKind == kind && p.ScopeID == scopeID && p.Open(now) {
			out = append(out, *p)
		}
	}
	return out, nil
}

func (m *memProposals) Resolve(_ context.Context, id string, status models.AssistantProposalStatus, chosen, by string, now time.Time) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, p := range m.rows {
		if p.ID == id && p.Open(now) {
			p.Status, p.Chosen, p.ResolvedBy, p.ResolvedAt = status, chosen, by, &now
			return true, nil
		}
	}
	return false, nil
}

var _ repository.AssistantProposalRepository = (*memProposals)(nil)

// proposalState is a campaign turn whose generation engine counts its calls.
func proposalState(proposals *memProposals, generated *int) *requestState {
	return &requestState{
		campaignID:       "c1",
		campaign:         timelineCampaign(),
		repos:            CampaignAssistantRepos{Proposals: proposals},
		maxGeneratePosts: 10,
		generatePosts: func(_ context.Context, req content_plan.GeneratePostsRequest, _ content_plan.OnEventFunc) (*content_plan.ContentPlanResponse, error) {
			*generated++
			posts := make([]content_plan.DraftPost, req.Count)
			return &content_plan.ContentPlanResponse{Posts: posts}, nil
		},
		contentPlan: func(context.Context, string, content_plan.OnEventFunc) (*content_plan.ContentPlanResponse, error) {
			*generated++
			return &content_plan.ContentPlanResponse{}, nil
		},
	}
}

func TestGeneratePosts_SmallBatchRunsStraightAway(t *testing.T) {
	store := &memProposals{}
	var generated int
	st := proposalState(store, &generated)
	out := runTool(t, st, "generatePosts", map[string]any{"platforms": []string{"Threads"}, "count": 3})
	if out.Status != assistantkit.StatusOK || generated != 1 || len(store.rows) != 0 {
		t.Fatalf("outcome = %+v, generated = %d, proposals = %d", out, generated, len(store.rows))
	}
}

func TestGeneratePosts_LargeBatchIsProposed(t *testing.T) {
	store := &memProposals{}
	var generated int
	st := proposalState(store, &generated)
	out := runTool(t, st, "generatePosts", map[string]any{"platforms": []string{"Threads"}, "count": 10})
	if out.Status != assistantkit.StatusProposed || out.ProposalID == "" || generated != 0 {
		t.Fatalf("outcome = %+v, generated = %d", out, generated)
	}
	if len(store.rows) != 1 {
		t.Fatalf("stored %d proposals, want 1", len(store.rows))
	}
	p := store.rows[0]
	if p.Tool != "generatePosts" || p.ScopeID != "c1" || p.Args["count"] != float64(10) || !strings.Contains(p.Summary, "10") {
		t.Fatalf("stored proposal = %+v", p)
	}
	if !p.ExpiresAt.After(time.Now()) {
		t.Fatalf("proposal expires at %v, in the past", p.ExpiresAt)
	}
	// The turn is labelled "proposed" and the card event carries the proposal.
	outs := st.outcomes()
	if got := turnAction(outs, st.writes); got != actionProposed {
		t.Fatalf("turn action = %q, want %q", got, actionProposed)
	}
	if outs[0].event != SSEEventProposalCreated || outs[0].payload.(ProposalCreatedEventPayload).ID != p.ID {
		t.Fatalf("outcome = %+v", outs[0])
	}

	// Confirming runs the stored call unchanged, past the gate.
	reg := newTestTools(t).reg
	ctx := assistantkit.WithProposalSink(withRequestState(t.Context(), st), campaignSink{st: st})
	raw, err := reg.RunConfirmed(ctx, p.Tool, map[string]any(p.Args))
	if err != nil {
		t.Fatal(err)
	}
	if res, _ := raw.(map[string]any); res["status"] != string(assistantkit.StatusOK) || generated != 1 {
		t.Fatalf("confirmed run = %#v, generated = %d", raw, generated)
	}
}

// A request that could never run is refused, not proposed.
func TestGeneratePosts_InvalidLargeBatchIsRefusedNotProposed(t *testing.T) {
	store := &memProposals{}
	var generated int
	st := proposalState(store, &generated)
	out := runTool(t, st, "generatePosts", map[string]any{"platforms": []string{"TikTok"}, "count": 10})
	if out.Status != assistantkit.StatusRefused || len(store.rows) != 0 || generated != 0 {
		t.Fatalf("outcome = %+v, proposals = %d, generated = %d", out, len(store.rows), generated)
	}
}

func TestRunContentPlan_AlwaysProposed(t *testing.T) {
	store := &memProposals{}
	var generated int
	out := runTool(t, proposalState(store, &generated), "runContentPlan", map[string]any{})
	if out.Status != assistantkit.StatusProposed || generated != 0 || len(store.rows) != 1 || store.rows[0].Tool != "runContentPlan" {
		t.Fatalf("outcome = %+v, generated = %d, proposals = %d", out, generated, len(store.rows))
	}
}

// Without a proposal store a proposal-tier call fails closed: it neither runs
// nor pretends to have been proposed.
func TestRunContentPlan_NoStoreFailsClosed(t *testing.T) {
	var generated int
	st := proposalState(nil, &generated)
	st.repos.Proposals = nil
	spec, _ := newTestTools(t).reg.Lookup("runContentPlan")
	tool := spec.Tool.(interface {
		RunRaw(context.Context, any) (any, error)
	})
	ctx := assistantkit.WithProposalSink(withRequestState(t.Context(), st), campaignSink{st: st})
	if _, err := tool.RunRaw(ctx, map[string]any{}); err == nil || generated != 0 {
		t.Fatalf("err = %v, generated = %d, want a failure and no run", err, generated)
	}
}

func TestContext_ListsOpenProposals(t *testing.T) {
	p, err := loadPrompts(newTestTools(t).reg)
	if err != nil {
		t.Fatal(err)
	}
	actx, err := assembleContext(timelineCampaign(), "", []openProposal{{ID: "prop-1", Summary: "Add 10 draft posts for Threads"}}, time.Now(), p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(actx.ContextBlock, "## Waiting for the user's answer\n- prop-1: Add 10 draft posts for Threads") {
		t.Fatalf("context block:\n%s", actx.ContextBlock)
	}
	actx, err = assembleContext(timelineCampaign(), "", nil, time.Now(), p)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(actx.ContextBlock, "Waiting for the user's answer") {
		t.Fatal("the section renders with no open proposals")
	}
}
