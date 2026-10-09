package flows

import (
	"context"
	"slices"
	"testing"

	"github.com/firebase/genkit/go/ai"
	"github.com/pgvector/pgvector-go"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
)

// storedEmbeddings serves EmbeddingsByContent from a fixed map; the rest of the
// repository is unused here.
type storedEmbeddings struct {
	repository.AssetChunksRepository
	byContent map[string]pgvector.HalfVector
}

func (s storedEmbeddings) EmbeddingsByContent(context.Context, string, string, []string) (map[string]pgvector.HalfVector, error) {
	return s.byContent, nil
}

func TestFillEmbeddingsReusesUnchangedChunks(t *testing.T) {
	var embedded []string
	embedder := ai.NewEmbedder("test/embedder", nil, func(_ context.Context, req *ai.EmbedRequest) (*ai.EmbedResponse, error) {
		var resp ai.EmbedResponse
		for _, d := range req.Input {
			embedded = append(embedded, d.Content[0].Text)
			resp.Embeddings = append(resp.Embeddings, &ai.Embedding{Embedding: []float32{2}})
		}
		return &resp, nil
	})
	repo := storedEmbeddings{byContent: map[string]pgvector.HalfVector{
		"unchanged": pgvector.NewHalfVector([]float32{1}),
	}}
	chunks := []models.AssetChunk{
		{Content: "unchanged", TokenCount: 10},
		{Content: "edited", TokenCount: 7},
	}

	tokens, err := fillEmbeddings(t.Context(), embedder, repo, "a1", chunks)
	if err != nil {
		t.Fatalf("fill embeddings: %v", err)
	}
	if !slices.Equal(embedded, []string{"edited"}) {
		t.Errorf("embedded %v, want only the edited chunk", embedded)
	}
	if tokens != 7 {
		t.Errorf("metered tokens = %d, want 7 (reused chunks are free)", tokens)
	}
	if got := chunks[0].Embedding.Slice(); len(got) != 1 || got[0] != 1 {
		t.Errorf("unchanged chunk embedding = %v, want the stored [1]", got)
	}
	if got := chunks[1].Embedding.Slice(); len(got) != 1 || got[0] != 2 {
		t.Errorf("edited chunk embedding = %v, want a fresh [2]", got)
	}
}
