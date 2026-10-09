package embedbatch

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"testing"

	"github.com/firebase/genkit/go/ai"
)

// fakeEmbedder embeds a text as [len(text)] and fails any request containing
// a text in bad.
type fakeEmbedder struct {
	bad      map[string]bool
	requests []int
}

func (f *fakeEmbedder) Embed(_ context.Context, req *ai.EmbedRequest) (*ai.EmbedResponse, error) {
	f.requests = append(f.requests, len(req.Input))
	var resp ai.EmbedResponse
	for _, d := range req.Input {
		text := d.Content[0].Text
		if f.bad[text] {
			return nil, errors.New("bad text")
		}
		resp.Embeddings = append(resp.Embeddings, &ai.Embedding{Embedding: []float32{float32(len(text))}})
	}
	return &resp, nil
}

func TestEmbedBatches(t *testing.T) {
	texts := make([]string, Size+3)
	for i := range texts {
		texts[i] = strconv.Itoa(i * 1000)
	}
	f := &fakeEmbedder{}
	vecs, err := Embed(t.Context(), f, texts, nil)
	if err != nil {
		t.Fatalf("embed: %v", err)
	}
	if !slices.Equal(f.requests, []int{Size, 3}) {
		t.Errorf("request sizes = %v, want [%d 3]", f.requests, Size)
	}
	for i, v := range vecs {
		if len(v) != 1 || v[0] != float32(len(texts[i])) {
			t.Fatalf("vecs[%d] = %v, want [%d]", i, v, len(texts[i]))
		}
	}
}

func TestEmbedIsolatesAFailingText(t *testing.T) {
	texts := []string{"a", "bb", "ccc"}
	f := &fakeEmbedder{bad: map[string]bool{"bb": true}}
	vecs, err := Embed(t.Context(), f, texts, nil)
	if err == nil {
		t.Fatal("want the failing text's error")
	}
	got := fmt.Sprint(vecs)
	if want := "[[1] [] [3]]"; got != want {
		t.Errorf("vecs = %s, want %s", got, want)
	}
	if !slices.Equal(f.requests, []int{3, 1, 1, 1}) {
		t.Errorf("request sizes = %v, want one batch then three singles", f.requests)
	}
}
