package packs

import (
	"context"
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

func TestAssetsEnd(t *testing.T) {
	ctx, _ := turn(t, workspace, nil)
	if res, err := assetsEnd(ctx); res != nil || err != nil {
		t.Fatalf("unbegun turn = %v, %v", res, err)
	}
	ctx, _, err := assetsBegin(Deps{Assets: &stateAssets{}})(ctx, stateTurn(workspace, ""))
	if err != nil {
		t.Fatal(err)
	}
	if res, _ := assetsEnd(ctx); res != nil {
		t.Fatalf("no tool ran = %+v", res)
	}

	// Each tool's last output is kept; an answer's passages lose their text.
	recordAssets(ctx, func(r *AssetsResult) {
		r.Ask = askCard(&AskAssetsOutput{Scope: scopeOpenAsset, Excerpts: []Excerpt{{AssetID: "a1", Title: "Brochure", Pages: "p. 12", Text: "six rooms"}}})
	})
	d := analysisDeps()
	out, err := readAnalysis(ctx, d, ReadAnalysisInput{AssetID: "a1"})
	if err != nil {
		t.Fatal(err)
	}
	recordAssets(ctx, func(r *AssetsResult) { r.Analysis = out })
	res, err := assetsEnd(ctx)
	if err != nil || res == nil {
		t.Fatalf("end = %v, %v", res, err)
	}
	got := res.Value.(*AssetsResult)
	if got.Ask.Scope != scopeOpenAsset || got.Ask.Excerpts[0] != (AskCardCite{AssetID: "a1", Title: "Brochure", Pages: "p. 12"}) {
		t.Fatalf("ask = %+v", got.Ask)
	}
	if got.Analysis == nil || got.Analysis.AssetID != "a1" || got.Compare != nil || got.Campaign != nil {
		t.Fatalf("result = %+v", got)
	}
}

// turnAssets and lensRepo's State give Begin an empty bank to read.
type turnAssets struct{ memAssets }

func (turnAssets) Query(context.Context, models.AssetQuery) (*models.AssetQueryResult, error) {
	return &models.AssetQueryResult{}, nil
}

func (lensRepo) State(context.Context, *string, time.Time, int) (*repository.AnalysisState, error) {
	return &repository.AnalysisState{Lenses: map[models.AssetAnalysisStatus]int{}}, nil
}

type turnMessages struct {
	repository.AssistantMessageRepository
}

func (turnMessages) CreateBatch(context.Context, []*models.AssistantMessage) error { return nil }
func (turnMessages) ListRecent(context.Context, models.AssistantScopeKind, string, int) ([]models.AssistantMessage, error) {
	return nil, nil
}

// A turn that reads an analysis completes with it under results.assets, the
// tool as registered recording it.
func TestAssetsEnd_ThroughATurn(t *testing.T) {
	ctx := tenantctx.With(context.WithValue(t.Context(), logging.UserIDKey, "u1"), "tenant-1")
	g := genkit.Init(ctx)
	d := analysisDeps()
	d.Campaigns = &memCampaigns{}
	d.Assets = turnAssets{d.Assets.(memAssets)}
	a, err := assistant.Init(g, assistant.FlowConfig{Provider: llm.NewProvider(), MaxOutputTokens: 1024},
		assistant.Repos{Messages: turnMessages{}}, assetsPack(d))
	if err != nil {
		t.Fatal(err)
	}
	m := fakemodel.Define(g, "orchestrator",
		fakemodel.Tools(fakemodel.Call{Name: "readAnalysis", Input: map[string]any{"assetId": "a1"}}),
		fakemodel.Answer(`{"explanation":"It sells the reopening.","action":"answered"}`))
	fakemodel.Route(ctx, m, fakemodel.Slot{Flow: modelconfig.FlowAssistant, Slot: modelconfig.SlotOrchestrator})

	view := &viewresolve.Resolved{View: &viewcontext.ViewContext{Version: viewcontext.Version, Screen: "assets", Scope: workspace}, Summary: "On assets"}
	resp, err := a.Run(ctx, assistant.Request{Instruction: "What did the analysis find?", View: view}, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := resp.Results["assets"].(*AssetsResult)
	if !ok || got.Analysis == nil || got.Analysis.Title != "Villa Aurora brochure 2026" || got.Ask != nil {
		t.Fatalf("results = %#v", resp.Results)
	}
}
