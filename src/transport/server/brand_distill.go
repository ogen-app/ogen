package server

import (
	"context"
	"fmt"
	"time"

	"github.com/firebase/genkit/go/genkit"

	"github.com/ogen-app/ogen/src/genkit/flows/brand_distill"
	"github.com/ogen-app/ogen/src/infra/vendors/llm"
	"github.com/ogen-app/ogen/src/kernel/usage"
	"github.com/ogen-app/ogen/src/usecase/branddistill"
	"github.com/ogen-app/ogen/src/usecase/brandsource"
	"github.com/ogen-app/ogen/src/usecase/ingest"
	"github.com/ogen-app/ogen/src/usecase/urlimport"
)

// brandDistillExpireEvery is how often proposals nobody committed within
// branddistill.ExpireAfter are expired.
const brandDistillExpireEvery = 6 * time.Hour

// newBrandDistill builds the brand distillation service. Its website read
// imports pages through the same use case as POST /assets/url; the model
// stages are bound once the Genkit runtime is up (initGenkit).
func newBrandDistill(d *deps) *branddistill.Service {
	r := d.r
	importer := &urlimport.Service{
		DB:      d.db,
		Assets:  r.pieceRepo,
		Ingest:  &ingest.Service{DB: d.db, Assets: r.pieceRepo, Files: r.assetFileRepo, Images: r.assetImageRepo},
		Jobs:    d.enqueuer,
		Gate:    d.ingest.firecrawl,
		Limiter: d.entitlements.limiter,
	}
	return branddistill.New(d.db, branddistill.Repos{
		Runs:   r.brandDistillRepo,
		Assets: r.pieceRepo,
		Chunks: r.chunksRepo,
		Brands: r.brandRepo,
	}, d.entitlements.limiter, d.hub, d.enqueuer, branddistill.Deps{
		Mapper:   d.ingest.firecrawl,
		Importer: importer,
		Marker:   d.brandSources(),
		Recorder: d.usage.recorder,
		Notifier: d.notifier,
	})
}

// initBrandDistill registers the brand_distill flows and returns their
// callbacks.
func initBrandDistill(g *genkit.Genkit, provider *llm.Provider, recorder *usage.Recorder, checker *usage.Checker) (brand_distill.Callbacks, error) {
	cb, err := brand_distill.Init(g, brand_distill.FlowConfig{Provider: provider, Recorder: recorder, Checker: checker})
	if err != nil {
		return brand_distill.Callbacks{}, fmt.Errorf("init brand distill flow: %w", err)
	}
	return cb, nil
}

// brandDistillFlows binds the service to the runtime's current callbacks.
func (r *genkitRuntime) brandDistillFlows() *branddistill.Flows {
	return &branddistill.Flows{
		Extract: r.BrandDistillExtract,
		Compose: r.BrandDistillCompose,
		Ready:   r.IsAnthropicAvailable,
	}
}

// BrandDistillExtract runs the extract stage. The job treats
// ErrAnthropicUnavailable as transient and retries.
func (r *genkitRuntime) BrandDistillExtract(ctx context.Context, req brand_distill.ExtractRequest) (*brand_distill.ExtractResponse, error) {
	r.mu.RLock()
	fn := r.brandDistill.Extract
	r.mu.RUnlock()
	if fn == nil {
		return nil, ErrAnthropicUnavailable
	}
	return fn(ctx, req)
}

// BrandDistillCompose runs the compose stage.
func (r *genkitRuntime) BrandDistillCompose(ctx context.Context, req brand_distill.ComposeRequest) (*brand_distill.ComposeResponse, error) {
	r.mu.RLock()
	fn := r.brandDistill.Compose
	r.mu.RUnlock()
	if fn == nil {
		return nil, ErrAnthropicUnavailable
	}
	return fn(ctx, req)
}

// brandSources tags the assets brought in to read a brand from.
func (d *deps) brandSources() *brandsource.Marker {
	return &brandsource.Marker{Tags: d.r.tagRepo}
}
