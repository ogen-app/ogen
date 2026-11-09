package packs

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/firebase/genkit/go/genkit"

	"github.com/ogen-app/ogen/src/domain/modelconfig"
	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/domain/viewcontext"
	"github.com/ogen-app/ogen/src/genkit/flows/assistant"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/fakemodel"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/infra/vendors/llm"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
	"github.com/ogen-app/ogen/src/usecase/viewresolve"
)

// storedProposals keeps the turn's offers.
type storedProposals struct {
	repository.AssistantProposalRepository
	n int
}

func (s *storedProposals) Create(_ context.Context, p *models.AssistantProposal) error {
	s.n++
	p.ID = fmt.Sprint("prop-", s.n)
	return nil
}

func (s *storedProposals) ListOpen(context.Context, models.AssistantScopeKind, string, time.Time) ([]models.AssistantProposal, error) {
	return nil, nil
}

// A turn that checks and optimises the calendar completes with both outputs
// under results.repair, the tools as registered recording them, so a
// reloaded thread can draw the findings, their keys and the locked posts.
func TestRepairEnd_ThroughATurn(t *testing.T) {
	ctx := tenantctx.With(context.WithValue(t.Context(), logging.UserIDKey, "u1"), "tenant-1")
	g := genkit.Init(ctx)
	d, _, _ := repairDeps()
	a, err := assistant.Init(g, assistant.FlowConfig{Provider: llm.NewProvider(), MaxOutputTokens: 1024},
		assistant.Repos{Messages: turnMessages{}, Proposals: &storedProposals{}}, repairPack(d))
	if err != nil {
		t.Fatal(err)
	}
	m := fakemodel.Define(g, "orchestrator",
		fakemodel.Tools(fakemodel.Call{Name: "checkCalendar", Input: map[string]any{}}, fakemodel.Call{Name: "optimiseCalendar", Input: map[string]any{}}),
		fakemodel.Answer(`{"explanation":"Two posts are off; here are the moves.","action":"answered"}`))
	fakemodel.Route(ctx, m, fakemodel.Slot{Flow: modelconfig.FlowAssistant, Slot: modelconfig.SlotOrchestrator})

	view := &viewresolve.Resolved{View: &viewcontext.ViewContext{Version: viewcontext.Version, Screen: "campaign.calendar", Scope: campaign1}, Summary: "On the calendar"}
	resp, err := a.Run(ctx, assistant.Request{Instruction: "What's wrong with the calendar? Even it out.", View: view}, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := resp.Results["repair"].(map[string]any)
	if !ok {
		t.Fatalf("results = %#v", resp.Results)
	}
	check, ok := got["checkCalendar"].(*CheckCalendarOutput)
	if !ok || len(check.Findings) == 0 || check.Findings[0].Key == "" {
		t.Fatalf("checkCalendar = %#v", got["checkCalendar"])
	}
	opt, ok := got["optimiseCalendar"].(*OptimiseOutput)
	if !ok || len(opt.Locked) != 1 || opt.Locked[0].PostID != "p3" {
		t.Fatalf("optimiseCalendar = %#v", got["optimiseCalendar"])
	}
}
