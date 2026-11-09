package campaign_assistant

import (
	"context"
	"slices"
	"testing"

	"github.com/firebase/genkit/go/ai"
	"github.com/pgvector/pgvector-go"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/domain/viewcontext"
	"github.com/ogen-app/ogen/src/genkit/flows/assistant"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/assistantkit"
	"github.com/ogen-app/ogen/src/infra/repository"
)

// onAssetPage is a turn on the campaign's page of asset open, with the
// campaign's tools ready.
func onAssetPage(t *testing.T, open string) context.Context {
	t.Helper()
	ctx, _ := assistant.NewViewTurnContext(t.Context(), viewcontext.ViewContext{
		Version: viewcontext.Version, Screen: "campaign.asset",
		Scope: viewcontext.Scope{Kind: models.AssistantScopeCampaign, ID: "c1"},
		Focus: &viewcontext.Ref{Kind: viewcontext.KindAsset, ID: open},
	}, nil)
	return withRequestState(ctx, &requestState{
		campaign: &models.Campaign{ID: "c1", AssetIDs: models.StringSlice{"a1", "a2", "a3"}},
		repos: CampaignAssistantRepos{Assets: readyAssets{all: []models.Asset{
			{ID: "a1", Status: "ready"}, {ID: "a2", Status: "ready"}, {ID: "a3", Status: "failed"}, {ID: "x9", Status: "ready"},
		}}},
	})
}

func TestOnOpenAsset(t *testing.T) {
	var got []string
	ran := false
	call := onOpenAsset(func(in *GeneratePostsInput) (*[]string, bool) { return &in.AssetIDs, in.AllAssets || in.Exploratory },
		func(_ *ai.ToolContext, in GeneratePostsInput) (assistantkit.Outcome[struct{}], error) {
			got, ran = in.AssetIDs, true
			return assistantkit.Outcome[struct{}]{}, nil
		})
	run := func(ctx context.Context, in GeneratePostsInput) ([]string, assistantkit.Outcome[struct{}]) {
		t.Helper()
		got, ran = nil, false
		out, err := call(&ai.ToolContext{Context: ctx}, in)
		if err != nil {
			t.Fatal(err)
		}
		return got, out
	}

	page := onAssetPage(t, "a2")
	for name, c := range map[string]struct {
		ctx  context.Context
		in   GeneratePostsInput
		want []string
	}{
		"the open asset is the default":    {page, GeneratePostsInput{}, []string{"a2"}},
		"named assets win":                 {page, GeneratePostsInput{AssetIDs: []string{"a1"}}, []string{"a1"}},
		"all of them widens it":            {page, GeneratePostsInput{AllAssets: true}, nil},
		"an exploratory call uses none":    {page, GeneratePostsInput{Exploratory: true}, nil},
		"a confirmed offer runs as stored": {assistantkit.WithConfirmed(page), GeneratePostsInput{}, nil},
		"another campaign's asset isn't":   {onAssetPage(t, "x9"), GeneratePostsInput{}, nil},
		"off an asset page, all of them":   {withRequestState(t.Context(), getRequestState(page)), GeneratePostsInput{}, nil},
	} {
		if got, _ := run(c.ctx, c.in); !ran || !slices.Equal(got, c.want) {
			t.Errorf("%s: ran %t, sources = %v, want %v", name, ran, got, c.want)
		}
	}

	// One of the campaign's assets that isn't ready is refused, not
	// silently widened to the others; asking for all of them runs.
	if _, out := run(onAssetPage(t, "a3"), GeneratePostsInput{}); ran || out.Status != assistantkit.StatusRefused {
		t.Fatalf("unready open asset: ran %t, outcome %+v", ran, out)
	}
	if got, _ := run(onAssetPage(t, "a3"), GeneratePostsInput{AllAssets: true}); !ran || got != nil {
		t.Fatalf("unready, all: ran %t, sources %v", ran, got)
	}
}

type vecEmbedder struct{ ai.Embedder }

func (vecEmbedder) Embed(context.Context, *ai.EmbedRequest) (*ai.EmbedResponse, error) {
	return &ai.EmbedResponse{Embeddings: []*ai.Embedding{{Embedding: []float32{1}}}}, nil
}

// searchedChunks records the assets a search covered.
type searchedChunks struct {
	repository.AssetChunksRepository
	ids *[]string
}

func (r searchedChunks) SearchSimilar(_ context.Context, _ pgvector.HalfVector, ids []string, _ float64, _ int) ([]models.AssetChunk, error) {
	*r.ids = ids
	return []models.AssetChunk{{AssetID: ids[0], Content: "six treatment rooms"}}, nil
}

func TestAskCampaignAssets_OpenAssetOnly(t *testing.T) {
	var searched []string
	ask := func(ctx context.Context, in AskCampaignAssetsInput) *AskCampaignAssetsOutput {
		t.Helper()
		st := getRequestState(ctx)
		st.embedder = vecEmbedder{}
		st.repos.Chunks = searchedChunks{ids: &searched}
		in.Query = "spa?"
		out, err := toolAskCampaignAssets(ctx, in)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	if out := ask(onAssetPage(t, "a2"), AskCampaignAssetsInput{}); !out.OpenAssetOnly || !slices.Equal(searched, []string{"a2"}) {
		t.Fatalf("default = %+v over %v; want the open asset only", out, searched)
	}
	if out := ask(onAssetPage(t, "a2"), AskCampaignAssetsInput{AllAssets: true}); out.OpenAssetOnly || !slices.Equal(searched, []string{"a1", "a2"}) {
		t.Fatalf("all = %+v over %v; want the campaign's ready assets", out, searched)
	}
	if out := ask(onAssetPage(t, "a2"), AskCampaignAssetsInput{AssetIDs: []string{"a1"}}); out.OpenAssetOnly || !slices.Equal(searched, []string{"a1"}) {
		t.Fatalf("named = %+v over %v", out, searched)
	}
	if out := ask(onAssetPage(t, "x9"), AskCampaignAssetsInput{}); out.OpenAssetOnly || !slices.Equal(searched, []string{"a1", "a2"}) {
		t.Fatalf("another campaign's asset open = %+v over %v", out, searched)
	}
	searched = nil
	if out := ask(onAssetPage(t, "a3"), AskCampaignAssetsInput{}); out.Note != openAssetUnready || searched != nil {
		t.Fatalf("unready open asset = %+v over %v; want a note, no search", out, searched)
	}
}
