package flows

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/firebase/genkit/go/ai"
	"github.com/firebase/genkit/go/core"
	"github.com/firebase/genkit/go/genkit"
	"github.com/pgvector/pgvector-go"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/genkit/embedopts"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/infra/vendors/llm"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
	"github.com/ogen-app/ogen/src/kernel/usage"
)

// EmbedAssetInput is the typed input for the embedAsset flow.
type EmbedAssetInput struct {
	AssetID string `json:"asset_id"`
	Title   string `json:"title"`
	Content string `json:"content"` // raw BlockNote JSON
	// TenantID carries the saver's tenant into the background embed goroutine,
	// which runs without a request context. The scheduler's status writes and
	// chunk upserts are tenant-scoped and would otherwise fail closed with
	// ErrNoTenant.
	TenantID string `json:"tenant_id"`
}

// EmbedAssetFlow is the singleton flow for embedding an asset.
// It is set by Init and ready for use after server startup.
var EmbedAssetFlow *core.Flow[EmbedAssetInput, struct{}, struct{}]

// embedScheduler serialises embed flows per asset and coalesces overlapping
// saves. If a new save arrives while an embed is running for the same asset,
// only the latest input is kept as "pending" — intermediate saves are
// discarded. When the running embed finishes, the pending one runs with the
// latest content. This prevents concurrent embeds of the same asset from
// flooding the embedder and contending for the same database rows.
type embedScheduler struct {
	mu        sync.Mutex
	pending   map[string]EmbedAssetInput
	running   map[string]bool
	assetRepo repository.AssetRepository // optional; when set, used to flip asset.status
	embedder  ai.Embedder                // set by Init; used to skip when no key
}

func newEmbedScheduler() *embedScheduler {
	return &embedScheduler{
		pending: make(map[string]EmbedAssetInput),
		running: make(map[string]bool),
	}
}

// Schedule queues an embed for the asset. Safe to call concurrently. Returns
// immediately — the embed runs in a background goroutine.
func (s *embedScheduler) Schedule(in EmbedAssetInput) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.pending[in.AssetID] = in
	if s.running[in.AssetID] {
		slog.Info("coalesced with in-flight embed", logging.AttrComponent, "genkit.embed_asset", "asset_id", in.AssetID)
		return
	}
	s.running[in.AssetID] = true
	go s.run(in.AssetID)
}

func (s *embedScheduler) run(assetID string) {
	for {
		s.mu.Lock()
		in, ok := s.pending[assetID]
		if !ok {
			s.running[assetID] = false
			s.mu.Unlock()
			return
		}
		delete(s.pending, assetID)
		s.mu.Unlock()

		// Skip when no gemini_api_key is configured: leave the asset's
		// status untouched (a later save re-triggers once a key is set via the
		// secrets API) rather than marking it failed. Unlike the process_pdf River
		// job, the markdown embed is fire-and-forget with no retry, so skip is the
		// only sane no-key behaviour.
		if !embedopts.Available(s.embedder) {
			slog.Warn("embed skipped, no gemini_api_key configured", logging.AttrComponent, "genkit.embed_asset", "asset_id", in.AssetID)
			continue
		}

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		// Rebuild the tenant context the request goroutine carried, so the
		// status writes and chunk upserts run against the right tenant.
		ctx = tenantctx.With(ctx, in.TenantID)
		s.setStatus(ctx, in.AssetID, models.AssetStatusProcessing)
		_, err := EmbedAssetFlow.Run(ctx, in)
		if err != nil {
			slog.ErrorContext(ctx, "embed failed", logging.AttrComponent, "genkit.embed_asset", "asset_id", in.AssetID, logging.AttrError, err)
			s.setStatus(ctx, in.AssetID, models.AssetStatusFailed)
		} else {
			s.setStatus(ctx, in.AssetID, models.AssetStatusReady)
		}
		cancel()
	}
}

func (s *embedScheduler) setStatus(ctx context.Context, assetID, status string) {
	if s.assetRepo == nil {
		return
	}
	if err := s.assetRepo.UpdateStatus(ctx, assetID, status); err != nil {
		slog.ErrorContext(ctx, "update status failed", logging.AttrComponent, "genkit.embed_asset", "asset_id", assetID, "status", status, logging.AttrError, err)
	}
}

var defaultEmbedScheduler = newEmbedScheduler()

// NewAssetOnSaveCallback returns a fire-and-forget callback that schedules an
// embed of the asset. Concurrent saves of the same asset are serialised — at
// most one embed per asset runs at a time, and intermediate saves are
// coalesced into the latest content.
func NewAssetOnSaveCallback() func(assetID, title, content, tenantID string) {
	return func(assetID, title, content, tenantID string) {
		defaultEmbedScheduler.Schedule(EmbedAssetInput{
			AssetID:  assetID,
			Title:    title,
			Content:  content,
			TenantID: tenantID,
		})
	}
}

// Init registers all Genkit flows. It must be called once during server
// startup, after the Genkit instance and embedder have been initialised.
// assetRepo is optional — when non-nil, the scheduler flips asset.status as
// embeds run.
func Init(g *genkit.Genkit, embedder ai.Embedder, repo repository.AssetChunksRepository, assetRepo repository.AssetRepository, recorder *usage.Recorder, embedModel string) {
	defaultEmbedScheduler.assetRepo = assetRepo
	defaultEmbedScheduler.embedder = embedder
	EmbedAssetFlow = genkit.DefineFlow(g, "embedAsset",
		func(ctx context.Context, in EmbedAssetInput) (struct{}, error) {
			return struct{}{}, embedAsset(ctx, embedder, repo, recorder, embedModel, in)
		},
	)
}

func embedAsset(ctx context.Context, embedder ai.Embedder, repo repository.AssetChunksRepository, recorder *usage.Recorder, embedModel string, in EmbedAssetInput) error {
	// Asset content is stored as Markdown — feed it directly to the
	// chunker/embedder. Markdown is readable enough for embeddings; stripping
	// syntax wasn't worth its complexity cost.

	// Prepend title so every chunk carries the asset's identity.
	fullText := in.Title
	if in.Content != "" {
		fullText += "\n" + in.Content
	}

	chunkTexts := ChunkText(fullText)
	if len(chunkTexts) == 0 {
		slog.WarnContext(ctx, "no text content to embed, skipping", logging.AttrComponent, "genkit.embed_asset", "asset_id", in.AssetID)
		return nil
	}
	slog.InfoContext(ctx, "chunked asset", logging.AttrComponent, "genkit.embed_asset", "asset_id", in.AssetID, "chunks", len(chunkTexts), "chars", len(fullText))

	// Embed each chunk individually. Gemini's EmbedContent can batch multiple
	// documents per request, but per-chunk calls keep the scheduler simple and
	// the chunk count per asset is small.
	chunks := make([]models.AssetChunk, 0, len(chunkTexts))
	var totalTokens int64
	for i, text := range chunkTexts {
		if !hasWords(text) {
			slog.WarnContext(ctx, "chunk skipped, no words", logging.AttrComponent, "genkit.embed_asset", "asset_id", in.AssetID, "chunk", i, "total", len(chunkTexts)-1, "len", len(text), "repr", truncate(text, 80))
			continue
		}
		slog.DebugContext(ctx, "embedding chunk", logging.AttrComponent, "genkit.embed_asset", "asset_id", in.AssetID, "chunk", i, "total", len(chunkTexts)-1, "len", len(text), "preview", truncate(text, 80))

		resp, err := embedder.Embed(ctx, &ai.EmbedRequest{
			Input:   []*ai.Document{ai.DocumentFromText(text, nil)},
			Options: embedopts.Document(),
		})
		if err != nil {
			return fmt.Errorf("embed asset %s chunk %d: %w", in.AssetID, i, err)
		}
		if len(resp.Embeddings) != 1 {
			return fmt.Errorf("embed asset %s chunk %d: expected 1 embedding, got %d", in.AssetID, i, len(resp.Embeddings))
		}

		tokens := EstimateTokens(text)
		totalTokens += int64(tokens)
		chunks = append(chunks, models.AssetChunk{
			ID:         fmt.Sprintf("%s:%d", in.AssetID, i),
			AssetID:    in.AssetID,
			ChunkIndex: i,
			Content:    text,
			TokenCount: tokens,
			Embedding:  pgvector.NewHalfVector(resp.Embeddings[0].Embedding),
			Model:      embedder.Name(),
		})
	}

	if err := repo.UpsertChunks(ctx, in.AssetID, chunks); err != nil {
		return fmt.Errorf("store chunks for asset %s: %w", in.AssetID, err)
	}

	// One usage event per asset embed. The Gemini embed response carries
	// no token usage, so we meter the chunker's estimate. Nil recorder = no-op.
	recorder.RecordResp(ctx, llm.VendorGemini, embedModel, "asset_embed", llm.EmbedUsage{Tokens: totalTokens})

	slog.InfoContext(ctx, "stored chunks", logging.AttrComponent, "genkit.embed_asset", "asset_id", in.AssetID, "chunks", len(chunks))
	return nil
}

// hasWords returns true when text contains at least one non-whitespace Unicode
// word character (letter or digit). This is stricter than TrimSpace != ""
// and catches zero-width spaces, non-breaking spaces, and other invisible
// Unicode characters that TrimSpace doesn't strip but that the embedding model
// cannot produce tokens from.
func hasWords(text string) bool {
	for _, r := range text {
		if r > 32 && !strings.ContainsRune(" \t\n\r\v\f\u00a0\u200b\u200c\u200d\ufeff", r) {
			return true
		}
	}
	return false
}

// truncate returns the first n runes of s, followed by "…" if truncated.
func truncate(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "…"
}
