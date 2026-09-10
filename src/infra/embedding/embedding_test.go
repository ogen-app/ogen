package embedding

import (
	"context"
	"errors"
	"testing"

	"github.com/firebase/genkit/go/ai"
	"github.com/firebase/genkit/go/core/api"

	"github.com/ogen-app/ogen/src/genkit/embedopts"
)

// fakeEmbedder is a minimal ai.Embedder stand-in for the reloadable wrapper's
// backing embedder — it records Embed calls so delegation can be asserted.
type fakeEmbedder struct {
	name  string
	resp  *ai.EmbedResponse
	calls int
}

func (f *fakeEmbedder) Name() string { return f.name }
func (f *fakeEmbedder) Embed(context.Context, *ai.EmbedRequest) (*ai.EmbedResponse, error) {
	f.calls++
	return f.resp, nil
}
func (f *fakeEmbedder) Register(api.Registry) {}

// Before any key is configured the wrapper is unavailable: Embed reports
// ErrEmbeddingUnavailable, Name falls back to the model, and embedopts.Available
// (the gate every call site uses) reflects it.
func TestReloadableEmbedderUnavailable(t *testing.T) {
	e := &reloadableEmbedder{model: "gemini-embedding-2"}

	if e.Available() {
		t.Error("Available() = true before any key; want false")
	}
	if embedopts.Available(e) {
		t.Error("embedopts.Available = true; want false when no key")
	}
	if got := e.Name(); got != "gemini-embedding-2" {
		t.Errorf("Name() = %q; want model fallback", got)
	}
	if _, err := e.Embed(t.Context(), &ai.EmbedRequest{}); !errors.Is(err, ErrEmbeddingUnavailable) {
		t.Errorf("Embed err = %v; want ErrEmbeddingUnavailable", err)
	}
}

// swap simulates a gemini_api_key set (then clear) via the secrets API: the
// wrapper picks up the new backing embedder — and drops it again — with no new
// reference handed to consumers (CON-104 hot-reload).
func TestReloadableEmbedderSwap(t *testing.T) {
	e := &reloadableEmbedder{model: "gemini-embedding-2"}
	fake := &fakeEmbedder{name: "googleai/gemini-embedding-2", resp: &ai.EmbedResponse{}}

	e.swap(fake) // key set

	if !e.Available() || !embedopts.Available(e) {
		t.Fatal("Available() = false after swap; want true")
	}
	if got := e.Name(); got != fake.name {
		t.Errorf("Name() = %q; want delegated %q", got, fake.name)
	}
	if _, err := e.Embed(t.Context(), &ai.EmbedRequest{}); err != nil {
		t.Errorf("Embed after swap: %v", err)
	}
	if fake.calls != 1 {
		t.Errorf("fake.calls = %d; want 1 (delegated)", fake.calls)
	}

	e.swap(nil) // key cleared

	if e.Available() {
		t.Error("Available() = true after swap(nil); want false")
	}
	if _, err := e.Embed(t.Context(), &ai.EmbedRequest{}); !errors.Is(err, ErrEmbeddingUnavailable) {
		t.Errorf("Embed after swap(nil) err = %v; want ErrEmbeddingUnavailable", err)
	}
}
