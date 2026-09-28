package queues

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"testing"

	"github.com/ogen-app/ogen/src/domain/models"
)

func TestEmbedStatsSettle(t *testing.T) {
	cases := []struct {
		name        string
		stats       embedStats
		lastAttempt bool
		want        string
		wantErr     bool
	}{
		{name: "nothing embeddable", stats: embedStats{}, want: models.AssetStatusReady},
		{name: "nothing embeddable last attempt", stats: embedStats{}, lastAttempt: true, want: models.AssetStatusReady},
		{name: "all failed retries", stats: embedStats{Attempts: 3, Failures: 3}, wantErr: true},
		{name: "all failed last attempt", stats: embedStats{Attempts: 3, Failures: 3}, lastAttempt: true, want: models.AssetStatusFailed},
		{name: "some failed", stats: embedStats{Attempts: 3, Embedded: 2, Failures: 1}, want: models.AssetStatusPartial},
		{name: "all embedded", stats: embedStats{Attempts: 2, Embedded: 2}, want: models.AssetStatusReady},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.stats.settle(tc.lastAttempt)
			if tc.wantErr {
				if _, ok := errors.AsType[*allChunksFailedError](err); !ok {
					t.Fatalf("err = %v, want *allChunksFailedError", err)
				}
				if err.Error() != "all 3 chunk(s) failed to embed" {
					t.Fatalf("err message = %q", err.Error())
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("settle = (%q, %v), want (%q, nil)", got, err, tc.want)
			}
		})
	}
}

func TestEmbedChunks(t *testing.T) {
	emb := &fakeEmbedder{failCalls: map[int]bool{2: true}}
	anchor := &models.SourceAnchor{Kind: "page", Page: 1}
	sources := []chunkSource{
		{Index: 0, Text: "first chunk text", Label: "Page 1", Anchor: anchor, PageStart: new(1)},
		{Index: 1, Text: " \u200b\n"}, // wordless: skipped, not an attempt
		{Index: 2, Text: "fails to embed"},
		{Index: 5, Text: "reported tokens", Tokens: 42},
	}

	chunks, stats := embedChunks(t.Context(), emb, "a1", slices.Values(sources))

	if want := (embedStats{Attempts: 3, Embedded: 2, Failures: 1, Tokens: int64(estimateTokens("first chunk text")) + 42}); stats != want {
		t.Fatalf("stats = %+v, want %+v", stats, want)
	}
	if len(chunks) != 2 {
		t.Fatalf("chunks = %d, want 2", len(chunks))
	}
	first, last := chunks[0], chunks[1]
	if first.ID != "a1:0" || first.AssetID != "a1" || first.Model != "fake-embedder" {
		t.Fatalf("first chunk identity = %q/%q/%q", first.ID, first.AssetID, first.Model)
	}
	if first.SourceLabel == nil || *first.SourceLabel != "Page 1" || first.SourceAnchor != anchor || *first.PageStart != 1 {
		t.Fatalf("first chunk metadata not carried: %+v", first)
	}
	if last.ID != "a1:5" || last.ChunkIndex != 5 || last.TokenCount != 42 {
		t.Fatalf("last chunk = id %q index %d tokens %d", last.ID, last.ChunkIndex, last.TokenCount)
	}
	if last.SourceLabel != nil {
		t.Fatalf("empty label must persist as nil, got %q", *last.SourceLabel)
	}
}

func TestStoreChunks(t *testing.T) {
	ctx := t.Context()
	store := &fakeChunks{}
	if err := storeChunks(ctx, store, "op", "a1", nil, false); err != nil || store.calls != 0 {
		t.Fatalf("empty set without upsertEmpty: err %v calls %d, want skipped", err, store.calls)
	}
	if err := storeChunks(ctx, store, "op", "a1", nil, true); err != nil || store.calls != 1 {
		t.Fatalf("empty set with upsertEmpty: err %v calls %d, want 1 call", err, store.calls)
	}
	if err := storeChunks(ctx, nil, "op", "a1", []models.AssetChunk{{ID: "a1:0"}}, true); err != nil {
		t.Fatalf("nil store: %v", err)
	}
	failing := &failingChunks{}
	err := storeChunks(ctx, failing, "process_x", "a1", []models.AssetChunk{{ID: "a1:0"}}, false)
	if err == nil || err.Error() != "process_x a1: store chunks: chunks: db down" {
		t.Fatalf("store error = %v", err)
	}
}

type failingChunks struct{}

func (failingChunks) UpsertChunks(context.Context, string, []models.AssetChunk) error {
	return errors.New("chunks: db down")
}

type unavailableEmbedder struct{ fakeEmbedder }

func (*unavailableEmbedder) Available() bool { return false }

func TestRequireEmbedder(t *testing.T) {
	ctx := t.Context()
	gaveUp := 0
	giveUp := func() error { gaveUp++; return nil }

	if ok, err := requireEmbedder(ctx, &fakeEmbedder{}, "op", "a1", false, giveUp); !ok || err != nil {
		t.Fatalf("available embedder: ok %v err %v", ok, err)
	}
	ok, err := requireEmbedder(ctx, &unavailableEmbedder{}, "process_x", "a1", false, giveUp)
	if ok || err == nil || err.Error() != "process_x a1: embedder unavailable" || gaveUp != 0 {
		t.Fatalf("unavailable, not last: ok %v err %v gaveUp %d", ok, err, gaveUp)
	}
	ok, err = requireEmbedder(ctx, nil, "op", "a1", true, giveUp)
	if ok || err != nil || gaveUp != 1 {
		t.Fatalf("nil embedder, last attempt: ok %v err %v gaveUp %d", ok, err, gaveUp)
	}
}

func TestAssetStatusWriter(t *testing.T) {
	ctx := t.Context()

	plain := &fakeStatus{}
	w := assetStatusWriter{op: "op", assets: plain}
	if err := w.set(ctx, "a1", models.AssetStatusProcessing); err != nil {
		t.Fatal(err)
	}
	if err := w.fail(ctx, "a1", models.UploadCodeInvalidFile, "bad"); err != nil {
		t.Fatal(err)
	}
	if plain.last() != models.AssetStatusFailed || plain.failCode != "" {
		t.Fatalf("without a marker fail must write the bare status: last %q code %q", plain.last(), plain.failCode)
	}

	marked := &fakeStatus{}
	w = assetStatusWriter{op: "op", assets: marked, marker: marked}
	if err := w.fail(ctx, "a1", models.UploadCodeInvalidFile, "bad"); err != nil {
		t.Fatal(err)
	}
	if marked.failCode != models.UploadCodeInvalidFile || marked.failReason != "bad" {
		t.Fatalf("with a marker fail must record code+reason: %q %q", marked.failCode, marked.failReason)
	}

	down := &fakeStatus{failOn: models.AssetStatusReady}
	err := assetStatusWriter{op: "process_x", assets: down}.set(ctx, "a1", models.AssetStatusReady)
	if err == nil || err.Error() != "process_x a1: set status ready: status: db down" {
		t.Fatalf("set error = %v", err)
	}

	if err := (assetStatusWriter{op: "op"}).fail(ctx, "a1", "c", "r"); err != nil {
		t.Fatalf("nil assets must be a no-op: %v", err)
	}
}

type fakeRunStore struct {
	existing  *models.AudioExtraction
	createErr error
	created   *models.AudioExtraction
	gets      int
}

func (f *fakeRunStore) GetByAssetAndRunKey(context.Context, string, string) (*models.AudioExtraction, error) {
	f.gets++
	if f.existing != nil {
		return f.existing, nil
	}
	return nil, sql.ErrNoRows
}

func (f *fakeRunStore) Create(_ context.Context, e *models.AudioExtraction) error {
	f.created = e
	return f.createErr
}

func TestEnsureExtractionRun(t *testing.T) {
	ctx := t.Context()
	newRun := func(id string) *models.AudioExtraction { return &models.AudioExtraction{ID: id, AssetID: "a1"} }

	existing := &models.AudioExtraction{ID: "x"}
	got, err := ensureExtractionRun(ctx, &fakeRunStore{existing: existing}, "op", "a1", "r", newRun)
	if err != nil || got != existing {
		t.Fatalf("existing run: got %v err %v", got, err)
	}

	store := &fakeRunStore{}
	got, err = ensureExtractionRun(ctx, store, "op", "a1", "r", newRun)
	if err != nil || got == nil || got.ID == "" || store.created != got {
		t.Fatalf("fresh run: got %+v err %v", got, err)
	}

	store = &fakeRunStore{createErr: errors.New("dup")}
	_, err = ensureExtractionRun(ctx, store, "process_x", "a1", "r", newRun)
	if err == nil || err.Error() != "process_x a1: create extraction: dup" || store.gets != 2 {
		t.Fatalf("create failure: err %v gets %d", err, store.gets)
	}
}
