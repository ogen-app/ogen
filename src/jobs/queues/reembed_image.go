package queues

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/riverqueue/river"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/genkit/embedopts"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
)

// ReembedImageQueue re-embeds an IMG asset after a user edits its description
// (CON-312). It rebuilds the chunk set from the edited description plus the
// region blocks the latest settled extraction stored, so the anchored region
// chunks survive the edit. No vision call and no status change: the asset is
// already searchable; only its chunks are refreshed.
const ReembedImageQueue = "reembed_image"

const (
	// reembedSnooze is how long the job waits for an in-flight extraction to
	// settle before checking again.
	reembedSnooze = 30 * time.Second
	// reembedStaleAfter bounds that wait: an extraction not updated for this
	// long (well past the image job's attempts × timeout) is treated as stuck.
	reembedStaleAfter = 2 * time.Hour
)

// ReembedImageTask carries the edited asset. The description is re-read from
// the DB on each attempt, so a later edit is never overwritten by an older job.
type ReembedImageTask struct {
	AssetID  string `json:"asset_id"`
	TenantID string `json:"tenant_id"`
}

func (ReembedImageTask) Kind() string { return ReembedImageQueue }

// InsertOpts routes the job to the image queue beside the ingestion runs.
func (ReembedImageTask) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: ImageQueue, MaxAttempts: 5}
}

type ReembedImageProcessor struct {
	river.WorkerDefaults[ReembedImageTask]
	Deps ImageDeps
}

func init() {
	register(func(w *river.Workers, d Deps) {
		river.AddWorker(w, &ReembedImageProcessor{Deps: d.Image})
	})
}

func (p *ReembedImageProcessor) Work(ctx context.Context, job *river.Job[ReembedImageTask]) error {
	ctx = WithJobRequestID(ctx, job.JobRow)
	ctx = tenantctx.With(ctx, job.Args.TenantID)
	return p.process(ctx, job.Args)
}

func (p *ReembedImageProcessor) process(ctx context.Context, in ReembedImageTask) error {
	if p.Deps.Assets == nil || p.Deps.Extractions == nil || p.Deps.Blocks == nil || p.Deps.Chunks == nil {
		return nil
	}
	if !embedopts.Available(p.Deps.Embedder) {
		return fmt.Errorf("reembed_image %s: embedder unavailable", in.AssetID)
	}

	ext, err := p.Deps.Extractions.GetLatestByAsset(ctx, in.AssetID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil // never extracted — the first run embeds the description itself
	}
	if err != nil {
		return fmt.Errorf("reembed_image %s: load extraction: %w", in.AssetID, err)
	}
	switch ext.Status {
	case models.ImageExtractionStatusComplete, models.ImageExtractionStatusPartial:
	case models.ImageExtractionStatusFailed:
		return nil // a failed run left no chunks to refresh
	default:
		// A run is in flight and may settle on chunks from before the edit, so
		// wait for it rather than drop the edit (CON-312). A run idle this long
		// is stuck, not in flight — stop waiting.
		if time.Since(ext.UpdatedAt) > reembedStaleAfter {
			slog.WarnContext(ctx, "image re-embed skipped: extraction stuck", logging.AttrComponent, "jobs.reembed_image", "asset_id", in.AssetID, "extraction_status", ext.Status)
			return nil
		}
		return river.JobSnooze(reembedSnooze)
	}

	asset, err := p.Deps.Assets.GetByID(ctx, in.AssetID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil // deleted since the edit
	}
	if err != nil {
		return fmt.Errorf("reembed_image %s: load asset: %w", in.AssetID, err)
	}
	blocks, err := p.Deps.Blocks.ListByExtraction(ctx, ext.ID)
	if err != nil {
		return fmt.Errorf("reembed_image %s: load blocks: %w", in.AssetID, err)
	}

	inputs := embedInputsFromPersisted(asset.Content, blocks)
	if len(inputs) == 0 {
		// Description cleared and no text regions: drop the stale chunks.
		return p.Deps.Chunks.UpsertChunks(ctx, in.AssetID, nil)
	}
	ingest := &ProcessImageProcessor{Deps: p.Deps}
	_, err = ingest.embed(ctx, ProcessImageTask{AssetID: in.AssetID, TenantID: in.TenantID}, inputs)
	return err
}
