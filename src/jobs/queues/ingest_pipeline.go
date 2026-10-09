package queues

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"iter"
	"log/slog"
	"strings"

	"github.com/pgvector/pgvector-go"

	"github.com/ogen-app/ogen/src/domain/modelconfig"
	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/genkit/embedbatch"
	"github.com/ogen-app/ogen/src/genkit/embedopts"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/usecase/notify"
)

// IngestQueue is the dedicated River queue PDF, document and URL ingestion run
// on: their parse, scrape and embed steps take minutes, and on the default
// queue a few bulk uploads would hold up post publishing and email.
const IngestQueue = "ingest"

// chunkSource is one text an asset processor hands to the shared embed step,
// with the position and citation metadata its persisted chunk carries.
type chunkSource struct {
	Index int
	Text  string
	// Tokens is the source-reported token count; zero falls back to
	// estimateTokens.
	Tokens int
	// Label is the citation label; empty persists as NULL.
	Label     string
	Anchor    *models.SourceAnchor
	PageStart *int
	PageEnd   *int
}

// embedStats tallies one embed pass: Attempts counts sources with words,
// Embedded those that produced a vector, Tokens the sum over embedded chunks.
type embedStats struct {
	Attempts int
	Embedded int
	Failures int
	Tokens   int64
}

// allChunksFailedError reports an embed pass where nothing landed although
// something was embeddable — almost always a transient embedder outage.
type allChunksFailedError struct{ attempts int }

func (e *allChunksFailedError) Error() string {
	return fmt.Sprintf("all %d chunk(s) failed to embed", e.attempts)
}

// settle turns an embed pass into the asset's final status. Nothing embeddable
// is ready with zero chunks; every embed failing is a retryable
// *allChunksFailedError until lastAttempt, when it settles to failed so the
// asset never stays stuck in "processing"; a partial embed is partial.
func (s embedStats) settle(lastAttempt bool) (string, error) {
	switch {
	case s.Attempts == 0:
		return models.AssetStatusReady, nil
	case s.Embedded == 0 && lastAttempt:
		return models.AssetStatusFailed, nil
	case s.Embedded == 0:
		return "", &allChunksFailedError{attempts: s.Attempts}
	case s.Failures > 0:
		return models.AssetStatusPartial, nil
	default:
		return models.AssetStatusReady, nil
	}
}

// embedChunks embeds every source that has words, in batched requests, and
// builds the chunk rows. A failed embed is counted, never fatal: the caller
// decides the outcome through embedStats.settle.
func embedChunks(ctx context.Context, embedder chunkEmbedder, assetID string, sources iter.Seq[chunkSource]) ([]models.AssetChunk, embedStats) {
	var srcs []chunkSource
	for src := range sources {
		if hasWords(src.Text) {
			srcs = append(srcs, src)
		}
	}
	texts := make([]string, len(srcs))
	for i := range srcs {
		texts[i] = srcs[i].Text
	}
	vecs, _ := embedbatch.Embed(ctx, embedder, texts, embedopts.Document())

	chunks := make([]models.AssetChunk, 0, len(srcs))
	stats := embedStats{Attempts: len(srcs)}
	for i, src := range srcs {
		if vecs[i] == nil {
			stats.Failures++
			continue
		}
		tokens := src.Tokens
		if tokens <= 0 {
			tokens = estimateTokens(src.Text)
		}
		stats.Embedded++
		stats.Tokens += int64(tokens)
		chunk := models.AssetChunk{
			ID:           fmt.Sprintf("%s:%d", assetID, src.Index),
			AssetID:      assetID,
			ChunkIndex:   src.Index,
			Content:      src.Text,
			TokenCount:   tokens,
			Embedding:    pgvector.NewHalfVector(vecs[i]),
			Model:        embedder.Name(),
			SourceAnchor: src.Anchor,
			PageStart:    src.PageStart,
			PageEnd:      src.PageEnd,
		}
		if src.Label != "" {
			chunk.SourceLabel = new(src.Label)
		}
		chunks = append(chunks, chunk)
	}
	return chunks, stats
}

// storeChunks replaces the asset's chunks with the embedded ones. An empty set
// is written only when clearStale and nothing was embeddable, clearing chunks
// left over from earlier content of a re-ingested asset. When every embed
// failed nothing is written, so the previous chunks stay searchable through a
// retry. A nil store is a no-op.
func storeChunks(ctx context.Context, store chunkUpserter, op, assetID string, chunks []models.AssetChunk, stats embedStats, clearStale bool) error {
	if store == nil || (len(chunks) == 0 && (!clearStale || stats.Attempts > 0)) {
		return nil
	}
	if err := store.UpsertChunks(ctx, assetID, chunks); err != nil {
		return fmt.Errorf("%s %s: store chunks: %w", op, assetID, err)
	}
	return nil
}

// requireEmbedder is the up-front guard every ingest runs before its paid
// source step (parse, scrape, transcribe, vision): without gemini_api_key every
// chunk embed would fail. It retries rather than fails, because a key set via
// the secrets API takes effect without a restart, and calls giveUp only on the
// last attempt so the asset never stays stuck in "processing". ok is false
// when the caller must return err.
func requireEmbedder(ctx context.Context, embedder chunkEmbedder, op, assetID string, lastAttempt bool, giveUp func() error) (ok bool, err error) {
	if embedopts.Available(embedder) {
		return true, nil
	}
	if lastAttempt {
		return false, giveUp()
	}
	slog.WarnContext(ctx, "embedder unavailable will retry", logging.AttrComponent, "jobs."+op, "asset_id", assetID)
	return false, fmt.Errorf("%s %s: embedder unavailable", op, assetID)
}

// assetFailureMarker records why an asset failed alongside the failed status.
type assetFailureMarker interface {
	MarkFailed(ctx context.Context, id, code, reason string) error
}

// assetStatusWriter persists an ingest's asset status and announces terminal
// outcomes to the asset's creator. A nil assets store makes every write a
// no-op. Status errors are returned so the job retries rather than reporting
// success with the asset stuck in "processing".
type assetStatusWriter struct {
	op       string
	assets   assetStatusUpdater
	notifier *notify.Service
	// label and kind word the notification (see notifyAssetStatus).
	label string
	kind  string
	// marker persists fail's code and reason. When unset, assets is used if it
	// implements assetFailureMarker; otherwise fail writes the bare status.
	marker assetFailureMarker
}

// unavailableReason is the tenant-visible reason when a dependency (embedder,
// processing service) stays down for every attempt.
func (w assetStatusWriter) unavailableReason() string {
	return w.label + " processing is temporarily unavailable — please try again"
}

// notConfiguredReason is the tenant-visible reason when the deployment lacks
// what the ingest needs; retrying cannot help.
func (w assetStatusWriter) notConfiguredReason() string {
	return w.label + " processing is not configured"
}

func (w assetStatusWriter) set(ctx context.Context, assetID, status string) error {
	if w.assets == nil {
		return nil
	}
	if err := w.assets.UpdateStatus(ctx, assetID, status); err != nil {
		return fmt.Errorf("%s %s: set status %s: %w", w.op, assetID, status, err)
	}
	notifyAssetStatus(ctx, w.notifier, w.assets, assetID, status, w.label, w.kind)
	return nil
}

// fail marks the asset failed with a machine-readable code and a tenant-visible
// reason. Without a marker the code and reason are dropped.
func (w assetStatusWriter) fail(ctx context.Context, assetID, code, reason string) error {
	if w.assets == nil {
		return nil
	}
	marker := w.marker
	if marker == nil {
		m, ok := w.assets.(assetFailureMarker)
		if !ok {
			return w.set(ctx, assetID, models.AssetStatusFailed)
		}
		marker = m
	}
	if err := marker.MarkFailed(ctx, assetID, code, reason); err != nil {
		return fmt.Errorf("%s %s: mark failed: %w", w.op, assetID, err)
	}
	notifyAssetStatus(ctx, w.notifier, w.assets, assetID, models.AssetStatusFailed, w.label, w.kind)
	return nil
}

// modelResolver picks the model for a (flow, slot) under ctx's tenant tier. A
// nil resolver uses the process-wide modelconfig resolver; tests inject one.
type modelResolver func(ctx context.Context, flowKey, slotKey string) string

func (r modelResolver) model(ctx context.Context, flowKey, slotKey string) string {
	if r == nil {
		return modelconfig.Model(ctx, flowKey, slotKey)
	}
	return r(ctx, flowKey, slotKey)
}

// extractionRunStore loads and creates the per-(asset, run_key) extraction row
// of a resumable ingest (audio, image).
type extractionRunStore[T any] interface {
	GetByAssetAndRunKey(ctx context.Context, assetID, runKey string) (*T, error)
	Create(ctx context.Context, e *T) error
}

// ensureExtractionRun loads the (asset, run_key) extraction or creates the
// fresh one built by newRun. A concurrent create loses to the unique index and
// reloads the winner.
func ensureExtractionRun[T any](ctx context.Context, store extractionRunStore[T], op, assetID, runKey string, newRun func(id string) *T) (*T, error) {
	ext, err := store.GetByAssetAndRunKey(ctx, assetID, runKey)
	if err == nil {
		return ext, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%s %s: load extraction: %w", op, assetID, err)
	}
	id, err := models.NewID()
	if err != nil {
		return nil, fmt.Errorf("%s %s: new extraction id: %w", op, assetID, err)
	}
	ext = newRun(id)
	if err := store.Create(ctx, ext); err != nil {
		if again, gErr := store.GetByAssetAndRunKey(ctx, assetID, runKey); gErr == nil {
			return again, nil
		}
		return nil, fmt.Errorf("%s %s: create extraction: %w", op, assetID, err)
	}
	return ext, nil
}

// openOriginal opens the original the upload handler stored before enqueue,
// to stream it to the parsing service rather than hold the whole file in
// memory. The caller closes it; N reports the bytes read so far, which is the
// file's size once the parser has consumed it. Every failure — opening, or a
// read error surfacing through the parser — is transient.
func openOriginal(ctx context.Context, store blobStore, op, assetID, key string) (*originalReader, error) {
	rc, err := store.Download(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("%s %s: download %s: %w", op, assetID, key, err)
	}
	return &originalReader{rc: rc}, nil
}

// originalReader is an io.ReadCloser that counts what it reads.
type originalReader struct {
	rc io.ReadCloser
	n  int
}

func (r *originalReader) Read(p []byte) (int, error) {
	n, err := r.rc.Read(p)
	r.n += n
	return n, err
}

func (r *originalReader) Close() error { return r.rc.Close() }

// N is the number of bytes read so far.
func (r *originalReader) N() int { return r.n }

// positiveIntPtr returns &n for n > 0 and nil otherwise, for optional 1-based
// positions where zero means unknown.
func positiveIntPtr(n int) *int {
	if n <= 0 {
		return nil
	}
	return new(n)
}

// estimateTokens mirrors flows.EstimateTokens (≈4 chars/token).
func estimateTokens(text string) int {
	if len(text) == 0 {
		return 0
	}
	if t := len(text) / 4; t > 0 {
		return t
	}
	return 1
}

// hasWords mirrors the flows helper: true when text has a non-whitespace word
// character (catches zero-width / non-breaking spaces the embedder can't use).
func hasWords(text string) bool {
	for _, r := range text {
		if r > 32 && !strings.ContainsRune(" \t\n\r\v\f\u00a0\u200b\u200c\u200d\ufeff", r) {
			return true
		}
	}
	return false
}
