// Package embedbatch embeds many texts with as few embedder round trips as
// possible.
package embedbatch

import (
	"context"
	"fmt"

	"github.com/firebase/genkit/go/ai"
)

// Size is the most texts sent in one embed request. Gemini's
// batchEmbedContents accepts up to 100; at the chunker's ~6,000-char ceiling
// 50 keeps a request around 300 KB.
const Size = 50

// Embedder is the slice of ai.Embedder this package needs.
type Embedder interface {
	Embed(ctx context.Context, req *ai.EmbedRequest) (*ai.EmbedResponse, error)
}

// Embed returns one vector per text, index-aligned with texts, sending Size
// texts per request. A batch that fails is retried one text at a time so a
// single bad text cannot fail its neighbours; a text that still fails gets a
// nil vector, and err is the first such failure.
func Embed(ctx context.Context, e Embedder, texts []string, options any) (vecs [][]float32, err error) {
	vecs = make([][]float32, len(texts))
	for start := 0; start < len(texts); start += Size {
		end := min(start+Size, len(texts))
		batch := texts[start:end]
		if got, berr := embed(ctx, e, batch, options); berr == nil {
			copy(vecs[start:end], got)
			continue
		}
		for i, text := range batch {
			got, terr := embed(ctx, e, []string{text}, options)
			if terr != nil {
				if err == nil {
					err = terr
				}
				continue
			}
			vecs[start+i] = got[0]
		}
	}
	return vecs, err
}

func embed(ctx context.Context, e Embedder, texts []string, options any) ([][]float32, error) {
	docs := make([]*ai.Document, len(texts))
	for i, t := range texts {
		docs[i] = ai.DocumentFromText(t, nil)
	}
	resp, err := e.Embed(ctx, &ai.EmbedRequest{Input: docs, Options: options})
	if err != nil {
		return nil, err
	}
	if len(resp.Embeddings) != len(texts) {
		return nil, fmt.Errorf("embed: got %d embeddings for %d texts", len(resp.Embeddings), len(texts))
	}
	out := make([][]float32, len(texts))
	for i, emb := range resp.Embeddings {
		out[i] = emb.Embedding
	}
	return out, nil
}
