package embedding

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/firebase/genkit/go/ai"
	"github.com/firebase/genkit/go/core/api"
	"github.com/firebase/genkit/go/genkit"
	"github.com/firebase/genkit/go/plugins/googlegenai"

	"github.com/ogen-app/ogen/src/genkit/embedopts"
	"github.com/ogen-app/ogen/src/genkit/flows"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/infra/secrets"
	"github.com/ogen-app/ogen/src/infra/storage"
	"github.com/ogen-app/ogen/src/kernel/config"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/kernel/usage"
)

// Callbacks bundles the fire-and-forget callbacks registered by Init.
// OnMarkdownSave is always non-nil after Init; it becomes a no-op per call when
// no gemini_api_key is currently configured (see reloadableEmbedder).
//
// PDF ingestion has no callback here — it goes through the process_pdf River
// job, enqueued by the asset upload handler.
type Callbacks struct {
	OnMarkdownSave func(assetID, title, content, tenantID string)
}

// embeddingDimensions is the fixed embedding width baked into the
// assets_chunks.embedding halfvec(3072) column (CON-101 migration). The Gemini
// embedder must emit exactly this many dimensions; any other value produces
// vectors that fail to insert. Changing it requires a schema migration, so it
// is a compile-time constant rather than config-derived.
const embeddingDimensions = 3072

// ErrEmbeddingUnavailable is returned by the reloadable embedder's Embed when no
// gemini_api_key is currently configured. Callers treat it as a transient,
// recoverable state — the key can be set / rotated via the secrets API without a
// restart, so a later call can succeed.
var ErrEmbeddingUnavailable = errors.New("embedding: no gemini_api_key configured")

// reloadableEmbedder is a stable ai.Embedder whose backing implementation is
// swapped when gemini_api_key is set / rotated / cleared via the secrets API.
// Consumers — the flow runtime, the process_pdf worker, and the
// markdown embed scheduler — hold this one reference for the process lifetime
// and never need rewiring on a key change, mirroring the genkit-rebuild
// subscription Anthropic uses in server/genkit_runtime.go and the Zernio
// per-call resolver in server/zernio.go. inner is nil when no key is
// configured; Embed then returns ErrEmbeddingUnavailable and Available reports
// false.
type reloadableEmbedder struct {
	mu    sync.RWMutex
	inner ai.Embedder
	model string // stable name reported by Name() while inner is nil
}

// Available satisfies embedopts.Availabler so every call site can gate on
// availability without importing this package.
func (e *reloadableEmbedder) Available() bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.inner != nil
}

// Name returns the backing embedder's registry name (used as the chunk's Model
// column). It falls back to the configured model name while no key is set — Name
// is only meaningfully consulted during an embed, when inner is non-nil.
func (e *reloadableEmbedder) Name() string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	if e.inner != nil {
		return e.inner.Name()
	}
	return e.model
}

// Embed delegates to the current backing embedder, or reports
// ErrEmbeddingUnavailable when no key is configured.
func (e *reloadableEmbedder) Embed(ctx context.Context, req *ai.EmbedRequest) (*ai.EmbedResponse, error) {
	e.mu.RLock()
	inner := e.inner
	e.mu.RUnlock()
	if inner == nil {
		return nil, ErrEmbeddingUnavailable
	}
	return inner.Embed(ctx, req)
}

// Register satisfies ai.Embedder. The wrapper is never itself placed in a genkit
// registry — consumers call Embed directly and the real backing embedder is
// registered on its own genkit instance in rebuild — so this is a no-op.
func (e *reloadableEmbedder) Register(api.Registry) {}

// swap atomically replaces the backing embedder (nil = unavailable).
func (e *reloadableEmbedder) swap(inner ai.Embedder) {
	e.mu.Lock()
	e.inner = inner
	e.mu.Unlock()
}

// Init builds the embedding subsystem: a dedicated Genkit instance for the embed
// flows, a reloadable embedder whose Gemini key is read from the
// secrets store, and the fire-and-forget callbacks. The returned ai.Embedder is
// the stable reloadable wrapper — always non-nil — so callers can hold it across
// gemini_api_key rotations. It is deliberately separate from the
// Anthropic flow runtime so an Anthropic key rotation never disturbs the
// embedder.
//
// When gemini_api_key is not set the embedder is "unavailable": asset saves
// still succeed, but no vectors are written and semantic search returns nothing
// — mirroring the previous empty-GEMINI_API_KEY behaviour, except the key can
// now be added via the secrets API and takes effect without a restart. Because
// the embedder is a hosted API there is no readiness poll at boot.
func Init(
	ctx context.Context,
	cfg *config.Config,
	chunksRepo repository.AssetChunksRepository,
	assetRepo repository.AssetRepository,
	fileRepo repository.AssetFileRepository,
	store storage.Storage,
	secretStore secrets.Store,
	recorder *usage.Recorder,
) (Callbacks, ai.Embedder, error) {
	// Fail fast at boot if the configured dimension diverges from the fixed
	// halfvec column contract, rather than surfacing as opaque per-chunk insert
	// errors deep in the async embed flow. This is config validation independent
	// of the key, so it runs even when no key is configured yet.
	if cfg.EmbedDimensions != embeddingDimensions {
		return Callbacks{}, nil, fmt.Errorf(
			"EMBED_DIMENSIONS=%d does not match the assets_chunks.embedding halfvec(%d) column; changing the embedding dimension requires a schema migration",
			cfg.EmbedDimensions, embeddingDimensions)
	}

	// Output dimensionality drives every embed request and must match the
	// assets_chunks.embedding halfvec(N) column; set it before any flow fires.
	embedopts.Dimensions = int32(cfg.EmbedDimensions)

	embedder := &reloadableEmbedder{model: cfg.EmbedModel}

	// The embed flows are registered once, on their own Genkit instance, against
	// the stable wrapper — flow registration is key-independent because the flow
	// calls embedder.Embed directly rather than resolving the embedder from the
	// registry. Only the wrapper's backing embedder is rebuilt on a key change.
	g := genkit.Init(ctx)
	flows.Init(g, embedder, chunksRepo, assetRepo, recorder, cfg.EmbedModel)

	// rebuild resolves the current gemini_api_key and swaps in a fresh backing
	// embedder (or nil when the key is absent). Returned errors fail boot on the
	// initial call and are logged on subscription-triggered calls.
	rebuild := func(ctx context.Context) error {
		key, err := secretStore.Get(ctx, secrets.NameGeminiAPIKey)
		if errors.Is(err, secrets.ErrNotFound) {
			embedder.swap(nil)
			slog.WarnContext(ctx, "gemini_api_key not configured; embedding disabled", logging.AttrComponent, "embedding")
			return nil
		}
		if err != nil {
			// rebuild only fires on a gemini_api_key change, so a failure here is
			// a failed rotation: invalidate rather than keep serving with the
			// rotated-away (possibly revoked) key.
			embedder.swap(nil)
			return fmt.Errorf("embedding: read gemini_api_key: %w", err)
		}

		// A fresh plugin + Genkit instance per rebuild: the googlegenai plugin
		// captures its key at construction, and re-defining an embedder of the
		// same name on one instance conflicts — so each rotation gets its own
		// throwaway instance, mirroring the Anthropic genkit rebuild. The old
		// instance is left for the GC once the swap completes.
		plugin := &googlegenai.GoogleAI{APIKey: key}
		gg := genkit.Init(ctx, genkit.WithPlugins(plugin))
		newEmbedder, err := plugin.DefineEmbedder(gg, cfg.EmbedModel, &ai.EmbedderOptions{
			Label:      "Gemini Embedding 2",
			Dimensions: cfg.EmbedDimensions,
		})
		if err != nil {
			// Do not leave the previous embedder serving the old key after a
			// failed rotation — drop to unavailable until a later change rebuilds
			// successfully.
			embedder.swap(nil)
			return fmt.Errorf("embedding: init gemini embedder %q: %w", cfg.EmbedModel, err)
		}
		embedder.swap(newEmbedder)
		slog.InfoContext(ctx, "gemini embedder ready", logging.AttrComponent, "embedding", "model", cfg.EmbedModel)
		return nil
	}

	if err := rebuild(ctx); err != nil {
		return Callbacks{}, nil, err
	}

	// Hot-reload: setting / rotating / clearing gemini_api_key via the secrets
	// API rebuilds the backing embedder with no reboot.
	secretStore.Subscribe(secrets.NameGeminiAPIKey, func() {
		if err := rebuild(context.Background()); err != nil {
			slog.Error("rebuild after gemini_api_key change failed", logging.AttrComponent, "embedding", logging.AttrError, err)
		}
	})

	return Callbacks{
		OnMarkdownSave: flows.NewAssetOnSaveCallback(),
	}, embedder, nil
}
