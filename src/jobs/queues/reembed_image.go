package queues

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/genkit/embedopts"
	"github.com/ogen-app/ogen/src/kernel/logging"
)

// ReembedImageQueue re-embeds an IMG asset after a user edits its description.
// It rebuilds the chunk set from the edited description plus the
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

type ReembedImageProcessor struct {
	Deps ImageDeps

	memOnce sync.Once
	mem     *memChunkStage
}

// process runs one attempt in-process.
func (p *ReembedImageProcessor) process(ctx context.Context, in ReembedImageTask) error {
	return runLocal(ctx, p.step, ReembedImageQueue, in, 0, 0, localActions{})
}

func (p *ReembedImageProcessor) stages() chunkStage {
	if p.Deps.Stages != nil {
		return p.Deps.Stages
	}
	p.memOnce.Do(func() { p.mem = newMemChunkStage(p.Deps.Chunks) })
	return p.mem
}

func (p *ReembedImageProcessor) embedLoop() embedLoop {
	return embedLoop{stage: p.stages(), embedder: p.Deps.Embedder}
}

type reembedState struct {
	Embed embedProgress `json:"embed"`
}

// step stages the edited description and the latest settled extraction's
// blocks, then embeds them a batch at a time.
func (p *ReembedImageProcessor) step(ctx context.Context, in StepInput) (StepOutput, error) {
	var task ReembedImageTask
	if err := json.Unmarshal(in.Args, &task); err != nil {
		return done("skipped_bad_args")
	}
	ctx = withTaskTenant(ctx, task.TenantID)
	var st reembedState
	if err := decodeState(in.State, &st); err != nil {
		return done("skipped_bad_state")
	}
	if st.Embed.RunID == "" {
		return p.start(ctx, task)
	}
	if err := p.embedLoop().settle(ctx, &st.Embed, in.Result); err != nil {
		return retry("reembed_image "+task.AssetID, err)
	}
	return p.embedNext(ctx, task, &st)
}

func (p *ReembedImageProcessor) start(ctx context.Context, in ReembedImageTask) (StepOutput, error) {
	if p.Deps.Assets == nil || p.Deps.Extractions == nil || p.Deps.Blocks == nil || p.Deps.Chunks == nil {
		return done("skipped_unwired")
	}
	if !embedopts.Available(p.Deps.Embedder) {
		return retry("reembed_image "+in.AssetID, errors.New("embedder unavailable"))
	}

	ext, err := p.Deps.Extractions.GetLatestByAsset(ctx, in.AssetID)
	if errors.Is(err, sql.ErrNoRows) {
		return done("skipped_never_extracted") // the first run embeds the description itself
	}
	if err != nil {
		return retry("reembed_image "+in.AssetID+": load extraction", err)
	}
	switch ext.Status {
	case models.ImageExtractionStatusComplete, models.ImageExtractionStatusPartial:
	case models.ImageExtractionStatusFailed:
		return done("skipped_failed_run") // a failed run left no chunks to refresh
	default:
		// A run is in flight and may settle on chunks from before the edit, so
		// wait for it rather than drop the edit. A run idle this long
		// is stuck, not in flight — stop waiting.
		if time.Since(ext.UpdatedAt) > reembedStaleAfter {
			slog.WarnContext(ctx, "image re-embed skipped: extraction stuck", logging.AttrComponent, "jobs.reembed_image", "asset_id", in.AssetID, "extraction_status", ext.Status)
			return done("skipped_stuck_run")
		}
		return StepOutput{Retry: &StepRetry{Reason: "extraction in flight", After: reembedSnooze}}, nil
	}

	asset, err := p.Deps.Assets.GetByID(ctx, in.AssetID)
	if errors.Is(err, sql.ErrNoRows) {
		return done("skipped_deleted") // deleted since the edit
	}
	if err != nil {
		return retry("reembed_image "+in.AssetID+": load asset", err)
	}
	blocks, err := p.Deps.Blocks.ListByExtraction(ctx, ext.ID)
	if err != nil {
		return retry("reembed_image "+in.AssetID+": load blocks", err)
	}
	inputs := embedInputsFromPersisted(asset.Content, blocks)
	if len(inputs) == 0 {
		// Description cleared and no text regions: drop the stale chunks.
		if err := p.Deps.Chunks.UpsertChunks(ctx, in.AssetID, nil); err != nil {
			return retry("reembed_image", err)
		}
		return done("cleared")
	}
	runID, err := models.NewID()
	if err != nil {
		return StepOutput{}, err
	}
	if err := p.stages().Begin(ctx, runID, in.AssetID, stageSources(slices.Values(inputs))); err != nil {
		return retry("reembed_image "+in.AssetID+": stage chunks", err)
	}
	return p.embedNext(ctx, in, &reembedState{Embed: embedProgress{RunID: runID}})
}

func (p *ReembedImageProcessor) embedNext(ctx context.Context, in ReembedImageTask, st *reembedState) (StepOutput, error) {
	call, err := p.embedLoop().next(ctx, &st.Embed)
	if err != nil {
		return retry("reembed_image "+in.AssetID, err)
	}
	if call != nil {
		raw, err := encodeState(st)
		if err != nil {
			return StepOutput{}, err
		}
		return StepOutput{State: raw, HTTP: call}, nil
	}
	if _, err := commitImageChunks(ctx, p.Deps, p.stages(), in.AssetID, st.Embed.RunID); err != nil {
		return retry("reembed_image "+in.AssetID, err)
	}
	return done("reembedded")
}
