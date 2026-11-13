package packs

import (
	"context"
	"sync"

	"github.com/ogen-app/ogen/src/genkit/flows/assistant"
	"github.com/ogen-app/ogen/src/usecase/assets"
)

// AssetsResult is what the assets tools found or made in a turn, for the
// client's cards: each tool's last output. It reaches the client as
// complete.results.assets, which the thread's history stores.
type AssetsResult struct {
	State    *assets.State             `json:"state,omitempty"`
	Ask      *AskCard                  `json:"ask,omitempty"`
	Analysis *ReadAnalysisOutput       `json:"analysis,omitempty"`
	Compare  *CompareAssetsOutput      `json:"compare,omitempty"`
	Campaign *CampaignFromAssetsOutput `json:"campaign,omitempty"`
	Distill  *DistillMoreOutput        `json:"distill,omitempty"`
}

// AskCard is an askAssets answer's sources: what was searched and the
// passages' assets and pages, without their text.
type AskCard struct {
	Scope    string        `json:"scope"`
	Excerpts []AskCardCite `json:"excerpts"`
}

// AskCardCite is one passage an answer drew on.
type AskCardCite struct {
	AssetID string `json:"assetId"`
	Title   string `json:"title"`
	Type    string `json:"type,omitempty"`
	Pages   string `json:"pages,omitempty"`
}

type assetsTurnKey struct{}

// assetsTurn collects a turn's AssetsResult; its tools run concurrently.
type assetsTurn struct {
	mu  sync.Mutex
	res AssetsResult
	any bool
}

func withAssetsTurn(ctx context.Context) context.Context {
	return context.WithValue(ctx, assetsTurnKey{}, &assetsTurn{})
}

// recordAssets notes a tool's output on the turn; outside a begun turn it
// does nothing.
func recordAssets(ctx context.Context, set func(*AssetsResult)) {
	t, ok := ctx.Value(assetsTurnKey{}).(*assetsTurn)
	if !ok {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	set(&t.res)
	t.any = true
}

func askCard(out *AskAssetsOutput) *AskCard {
	c := &AskCard{Scope: out.Scope, Excerpts: make([]AskCardCite, len(out.Excerpts))}
	for i, e := range out.Excerpts {
		c.Excerpts[i] = AskCardCite{AssetID: e.AssetID, Title: e.Title, Type: e.Type, Pages: e.Pages}
	}
	return c
}

// assetsEnd reports the turn's AssetsResult; nil when no tool gave one.
func assetsEnd(ctx context.Context) (*assistant.PackResult, error) {
	t, ok := ctx.Value(assetsTurnKey{}).(*assetsTurn)
	if !ok {
		return nil, nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.any {
		return nil, nil
	}
	res := t.res
	return &assistant.PackResult{Value: &res}, nil
}
