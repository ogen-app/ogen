package queues

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"google.golang.org/grpc"

	"github.com/ogen-app/ogen/src/domain/modelconfig"
	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/storage"
	"github.com/ogen-app/ogen/src/infra/vendors"
	"github.com/ogen-app/ogen/src/infra/vendors/llm"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/kernel/usage"
	imageclient "github.com/ogen-app/ogen/src/transport/grpc/client/image"
	"github.com/ogen-app/ogen/src/usecase/notify"
)

// ProcessImageQueue ingests a content-bank IMG asset: presign the
// original + a normalized-derivative slot, run the full image-service pipeline
// (normalize + EXIF-strip + classify + per-shape structured extraction +
// description + alt text) in one Extract RPC, then persist the extraction +
// blocks, set the asset's description/alt text, embed the searchable text into
// the shared assets_chunks, and snapshot cost. Replaces the old synchronous
// imageprobe path (deleted, D6); image-service is the sole image authority.
const ProcessImageQueue = "process_image"

const (
	// imagePresignTTL bounds the presigned GET/PUT URLs handed to image-service.
	imagePresignTTL = time.Hour
	// normalizedImageContentType is the MIME bound on the normalized.png PUT.
	normalizedImageContentType = "image/png"
)

// imageExtractor is the narrow slice of the image gRPC client the worker needs;
// a small fake satisfies it in tests.
type imageExtractor interface {
	Extract(ctx context.Context, opts imageclient.ExtractOptions) (*imageclient.ExtractResult, error)
}

// imageAssetWriter is the asset persistence the worker needs: status + creator
// (for notifications), the guarded description/alt-text write, and GetByID to
// reload the persisted description when resuming a checkpointed run. The asset
// repo satisfies it.
type imageAssetWriter interface {
	UpdateStatus(ctx context.Context, id, status string) error
	CreatorOf(ctx context.Context, id string) (string, error)
	SetImageResult(ctx context.Context, id, prevContent, content, altText string, setAlt bool) error
	GetByID(ctx context.Context, id string) (*models.Asset, error)
}

// imageFileStore lets the worker stamp the service-reported dimensions/animation
// onto the asset_file row created at upload (checksum + key + original mime are
// preserved). The asset-file repo satisfies it.
type imageFileStore interface {
	GetByAssetID(ctx context.Context, assetID string) (*models.AssetFile, error)
	Upsert(ctx context.Context, file *models.AssetFile) error
}

type imageExtractionStore interface {
	Create(ctx context.Context, e *models.ImageExtraction) error
	GetByAssetAndRunKey(ctx context.Context, assetID, runKey string) (*models.ImageExtraction, error)
	// GetLatestByAsset backs the description re-embed; sql.ErrNoRows
	// when the asset was never extracted.
	GetLatestByAsset(ctx context.Context, assetID string) (*models.ImageExtraction, error)
	Update(ctx context.Context, e *models.ImageExtraction) error
}

type imageBlockStore interface {
	ReplaceForExtraction(ctx context.Context, extractionID string, blocks []models.ImageBlock) error
	ListByExtraction(ctx context.Context, extractionID string) ([]models.ImageBlock, error)
}

// ImageDeps bundles the process_image worker's dependencies (built in
// server.go). A nil Client (no IMAGE_SERVICE_ADDR configured) disables the job —
// but note content-bank uploads already fail fast at the handler when the
// service is unwired (D6), so this is defensive.
type ImageDeps struct {
	Client      imageExtractor
	Embedder    chunkEmbedder
	Storage     audioBlobStore // PresignedGetURL + PresignedPutURL (bytes never traverse gRPC)
	Assets      imageAssetWriter
	Chunks      chunkUpserter
	Files       imageFileStore
	Extractions imageExtractionStore
	Blocks      imageBlockStore
	// Recorder meters vision usage on the existing gemini vendor; Checker
	// gates the run against the tenant's cost cap BEFORE the vision spend. Both
	// nil-safe.
	Recorder *usage.Recorder
	Checker  *usage.Checker
	// EmbedModel is the price-map key for description/block embedding usage.
	// Models picks the vision models passed through to image-service (nil = the
	// modelconfig resolver); a run resolves them once and keeps them on its
	// extraction row. ConfidenceThreshold gates the one-shot escalation;
	// AltTextMaxChars is the alt-text generation target length.
	EmbedModel          string
	Models              modelResolver
	ConfidenceThreshold float64
	AltTextMaxChars     int
	// Notifier drops an in-app notification to the asset's creator on a terminal
	// status. Nil is a no-op.
	Notifier *notify.Service
	// Stages holds the image's chunks while they embed. Nil keeps them in
	// memory, which serves only a run inside one process.
	Stages chunkStage
}

// ProcessImageTask carries the asset to ingest. The image bytes are NOT in the
// args — the worker hands image-service presigned URLs and re-reads state from
// the DB on each attempt. StorageKey is the tenant-relative object path the
// upload handler wrote (e.g. "assets/<id>/original.png"). RunKey makes the run
// idempotent; PinnedModel optionally overrides the extraction model.
type ProcessImageTask struct {
	AssetID      string `json:"asset_id"`
	TenantID     string `json:"tenant_id"`
	OriginalName string `json:"original_name"`
	MimeType     string `json:"mime_type"`
	StorageKey   string `json:"storage_key"`
	RunKey       string `json:"run_key"`
	PinnedModel  string `json:"pinned_model,omitempty"`
}

func (ProcessImageTask) Kind() string { return ProcessImageQueue }

type ProcessImageProcessor struct {
	Deps ImageDeps

	memOnce sync.Once
	mem     *memChunkStage
}

// process runs one attempt in-process.
func (p *ProcessImageProcessor) process(ctx context.Context, in ProcessImageTask, lastAttempt bool) error {
	attempt, maxAttempts := 1, 2
	if lastAttempt {
		attempt = 2
	}
	return runLocal(ctx, p.step, ProcessImageQueue, in, attempt, maxAttempts, localActions{})
}

func (p *ProcessImageProcessor) stages() chunkStage {
	if p.Deps.Stages != nil {
		return p.Deps.Stages
	}
	p.memOnce.Do(func() { p.mem = newMemChunkStage(p.Deps.Chunks) })
	return p.mem
}

func (p *ProcessImageProcessor) embedLoop() embedLoop {
	return embedLoop{stage: p.stages(), embedder: p.Deps.Embedder}
}

// imageState carries a run between steps. The run itself lives on its
// extraction row, which each step reloads.
type imageState struct {
	Phase string `json:"phase"`
	// Before is the description as it stood before the vision call: the
	// result replaces it only if nobody edited it meanwhile.
	Before  string        `json:"before,omitempty"`
	NormKey string        `json:"norm_key,omitempty"`
	Embed   embedProgress `json:"embed"`
}

const (
	imagePhaseExtract = "extract"
	imagePhaseEmbed   = "embed"
)

// step runs the paid vision pass (image-service Extract) behind the cost-cap
// gate, persists and checkpoints its result, then embeds from the persisted
// state and settles the run.
func (p *ProcessImageProcessor) step(ctx context.Context, in StepInput) (StepOutput, error) {
	var task ProcessImageTask
	if err := json.Unmarshal(in.Args, &task); err != nil {
		return done("skipped_bad_args")
	}
	ctx = withTaskTenant(ctx, task.TenantID)
	var st imageState
	if err := decodeState(in.State, &st); err != nil {
		return done("skipped_bad_state")
	}
	if st.Phase == "" {
		return p.start(ctx, task, in.lastAttempt())
	}
	ext, err := p.ensureExtraction(ctx, task)
	if err != nil {
		return retry("process_image", err)
	}
	switch st.Phase {
	case imagePhaseExtract:
		return p.extracted(ctx, task, ext, &st, in.Result, in.lastAttempt())
	case imagePhaseEmbed:
		if err := p.embedLoop().settle(ctx, &st.Embed, in.Result); err != nil {
			return retry("process_image "+task.AssetID, err)
		}
		return p.embedNext(ctx, task, ext, &st, in.lastAttempt())
	default:
		return done("skipped_bad_state")
	}
}

func (p *ProcessImageProcessor) start(ctx context.Context, in ProcessImageTask, lastAttempt bool) (StepOutput, error) {
	if p.Deps.Client == nil {
		slog.WarnContext(ctx, "image-service not configured", logging.AttrComponent, "jobs.process_image", "asset_id", in.AssetID)
		return done("skipped_unconfigured")
	}
	status := p.statusWriter()
	if p.Deps.Storage == nil || p.Deps.Extractions == nil || p.Deps.Blocks == nil {
		_ = status.fail(ctx, in.AssetID, models.UploadCodeInternalError, status.notConfiguredReason())
		return retry("process_image "+in.AssetID, errors.New("storage/repos not configured"))
	}
	giveUp := func() error {
		return status.fail(ctx, in.AssetID, models.UploadCodeServiceUnavailable, status.unavailableReason())
	}
	if ok, err := requireEmbedder(ctx, p.Deps.Embedder, "process_image", in.AssetID, lastAttempt, giveUp); !ok {
		return settledOr(err)
	}

	ext, err := p.ensureExtraction(ctx, in)
	if err != nil {
		return retry("process_image", err)
	}
	if ext.Status == models.ImageExtractionStatusComplete {
		return done("already_complete") // idempotent re-drive of a finished run
	}
	// A run loaded without its models gets them now, persisted before any
	// retryable work so the next attempt can't resolve different ones.
	if p.freezeModels(ctx, in, ext) {
		if err := p.Deps.Extractions.Update(ctx, ext); err != nil {
			return retry("process_image "+in.AssetID+": persist models", err)
		}
	}
	if err := status.set(ctx, in.AssetID, models.AssetStatusProcessing); err != nil {
		return retry("process_image", err)
	}
	// A prior attempt already ran and checkpointed the paid vision pass: resume
	// at embedding so a transient embed failure never re-charges Extract.
	if ext.Status == models.ImageExtractionStatusDescribing {
		return p.resumeFromCheckpoint(ctx, in, ext)
	}

	// Over-cap is terminal: reject before the spend. A nil checker is no gate.
	if p.Deps.Checker != nil {
		if err := p.Deps.Checker.Enforce(ctx); err != nil {
			return settledOr(p.terminalReject(ctx, in, ext, models.UploadCodeQuotaExceeded, "your usage limit has been reached — image processing was not started"))
		}
	}
	before, err := p.Deps.Assets.GetByID(ctx, in.AssetID)
	if err != nil {
		return retry("process_image "+in.AssetID+": load asset", err)
	}
	srcURL, dstURL, normKey, err := p.presign(ctx, in)
	if err != nil {
		return retry("process_image", err)
	}
	opts := p.extractOptions(in, ext, srcURL, dstURL)
	client := p.Deps.Client
	return sidecarStep(imageState{Phase: imagePhaseExtract, Before: before.Content, NormKey: normKey}, sidecarImage, 0, nil,
		func(cc grpc.ClientConnInterface) error {
			_, err := imageclient.Over(cc).Extract(ctx, opts)
			return err
		},
		func(ctx context.Context) (any, error) { return client.Extract(ctx, opts) })
}

// extracted settles the vision pass, persists its result and checkpoints the
// run, then embeds from the persisted state.
func (p *ProcessImageProcessor) extracted(ctx context.Context, in ProcessImageTask, ext *models.ImageExtraction, st *imageState, r *StepResult, lastAttempt bool) (StepOutput, error) {
	res, err := localOr(r, func(r *StepResult) (*imageclient.ExtractResult, error) {
		var res *imageclient.ExtractResult
		err := sidecarResult(r, func(cc grpc.ClientConnInterface) error {
			var err error
			res, err = imageclient.Over(cc).Extract(ctx, imageclient.ExtractOptions{})
			return err
		})
		return res, err
	})
	if err != nil {
		code, reason, terminal := classifyExtractErr(err, lastAttempt)
		if !terminal {
			return retry("process_image "+in.AssetID+": extract", err)
		}
		return settledOr(p.terminalReject(ctx, in, ext, code, reason))
	}
	if res.RejectedReason != "" {
		return settledOr(p.terminalReject(ctx, in, ext, models.UploadCodeInvalidFile, res.RejectedReason))
	}

	applyExtraction(ext, res, st.NormKey)
	p.accrueCost(ctx, in, ext, res.Usage)
	if err := p.persistBlocks(ctx, in, ext, res.Blocks); err != nil {
		return retry("process_image", err)
	}
	if err := p.stampFile(ctx, in, res); err != nil {
		return retry("process_image", err)
	}
	// Description → asset.Content, alt text → asset.AltText, both guarded
	// against a user edit in SQL. The title stays the upload filename.
	if err := p.Deps.Assets.SetImageResult(ctx, in.AssetID, st.Before, res.Description, res.AltText, res.AltText != ""); err != nil {
		return retry("process_image "+in.AssetID+": set description/alt", err)
	}
	// Checkpoint the vision pass before the retryable embed: `describing` means
	// "vision done, embedding pending", so a retry resumes instead of
	// re-calling the paid Extract.
	ext.Status = models.ImageExtractionStatusDescribing
	ext.FailureReason = ""
	ext.FailureCode = ""
	if err := p.Deps.Extractions.Update(ctx, ext); err != nil {
		return retry("process_image "+in.AssetID+": checkpoint extraction", err)
	}
	// Embed from the persisted state, not res: the description may have been
	// edited mid-run and kept by the compare-and-set above.
	return p.resumeFromCheckpoint(ctx, in, ext)
}

// presign returns presigned URLs for the original (in) and the
// normalized-derivative slot (out), plus the normalized key: image-service
// reads and writes these directly, so bytes never traverse gRPC.
func (p *ProcessImageProcessor) presign(ctx context.Context, in ProcessImageTask) (srcURL, dstURL, normKey string, err error) {
	srcURL, err = p.Deps.Storage.PresignedGetURL(ctx, storage.TenantKey(ctx, in.StorageKey), imagePresignTTL)
	if err != nil {
		return "", "", "", fmt.Errorf("process_image %s: presign source: %w", in.AssetID, err)
	}
	normKey = storage.TenantKey(ctx, fmt.Sprintf("assets/%s/normalized.png", in.AssetID))
	dstURL, err = p.Deps.Storage.PresignedPutURL(ctx, normKey, normalizedImageContentType, imagePresignTTL)
	if err != nil {
		return "", "", "", fmt.Errorf("process_image %s: presign normalized: %w", in.AssetID, err)
	}
	return srcURL, dstURL, normKey, nil
}

// extractOptions sends the models frozen on the run, so a retry after an
// operator change still runs (and records) the models the run started with.
func (p *ProcessImageProcessor) extractOptions(in ProcessImageTask, ext *models.ImageExtraction, srcURL, dstURL string) imageclient.ExtractOptions {
	return imageclient.ExtractOptions{
		SourceURL:           srcURL,
		DestPutURL:          dstURL,
		Filename:            in.OriginalName,
		ClassifyModel:       ext.ClassifyModel,
		ExtractModel:        ext.ExtractModel,
		EscalateModel:       ext.EscalateModel,
		AltTextMaxChars:     p.Deps.AltTextMaxChars,
		ConfidenceThreshold: p.Deps.ConfidenceThreshold,
	}
}

// classifyExtractErr maps an extract failure to a terminal reject (code,
// reason, true) or reports it as retryable (false). The fine-grained reason
// image-service attaches wins over the coarse gRPC-code buckets, which remain
// the fallback for a service that sends no ErrorInfo. A transient failure on
// the last attempt settles to failed so the asset never strands in
// "processing" once River gives up.
func classifyExtractErr(err error, lastAttempt bool) (code, reason string, terminal bool) {
	if code := imageclient.UploadCode(err); code != "" {
		return code, models.UploadRejectMessage(code), true
	}
	switch {
	case imageclient.IsUnsupportedImage(err):
		return models.UploadCodeUnsupportedMediaType, "the image format is not supported", true
	case imageclient.IsInvalidImage(err):
		return models.UploadCodeInvalidFile, "the image could not be processed (corrupt or too large)", true
	case lastAttempt:
		return models.UploadCodeServiceUnavailable, imageUnavailableReason, true
	default:
		return "", "", false
	}
}

// imageUnavailableReason is the tenant-visible reason when image-service or
// the embedder stays down for every attempt; it words the failure as retriable.
const imageUnavailableReason = "image processing is temporarily unavailable — please try again"

// applyExtraction copies the service-reported shape, quality flags and
// normalized-derivative metadata onto the extraction row.
func applyExtraction(ext *models.ImageExtraction, res *imageclient.ExtractResult, normKey string) {
	ext.Shape = res.Shape
	ext.ClassifyConfidence = res.ClassifyConfidence
	ext.Escalated = res.Escalated
	ext.EscalationImproved = res.EscalationImproved
	ext.DescriptionOK = res.DescriptionOK
	ext.ExtractionOK = res.ExtractionOK
	ext.Truncated = res.Truncated
	ext.NormalizedS3Key = &normKey
	ext.NormalizedMime = res.Normalized.Mime
	ext.Width = res.Normalized.Width
	ext.Height = res.Normalized.Height
	ext.IsAnimated = res.Normalized.IsAnimated
	if res.Normalized.ChecksumSHA256 != "" {
		ext.ChecksumSHA256 = res.Normalized.ChecksumSHA256
	}
}

// resumeFromCheckpoint embeds a run whose vision pass already completed and
// was checkpointed (status `describing`). It stages the persisted
// description (asset.Content) and blocks, so a transient embedder outage
// retries without a second (paid) Extract. The fresh path settles through it
// too, so both embed the same stored state.
func (p *ProcessImageProcessor) resumeFromCheckpoint(ctx context.Context, in ProcessImageTask, ext *models.ImageExtraction) (StepOutput, error) {
	runID, err := stageImageText(ctx, p.Deps, p.stages(), in.AssetID, ext.ID)
	if err != nil {
		return retry("process_image "+in.AssetID, err)
	}
	return p.embedNext(ctx, in, ext, &imageState{Phase: imagePhaseEmbed, Embed: embedProgress{RunID: runID}}, false)
}

// stageImageText stages an image's searchable text: its description and the
// blocks of extraction extID.
func stageImageText(ctx context.Context, d ImageDeps, stage chunkStage, assetID, extID string) (string, error) {
	asset, err := d.Assets.GetByID(ctx, assetID)
	if err != nil {
		return "", fmt.Errorf("reload asset: %w", err)
	}
	blocks, err := d.Blocks.ListByExtraction(ctx, extID)
	if err != nil {
		return "", fmt.Errorf("reload blocks: %w", err)
	}
	runID, err := models.NewID()
	if err != nil {
		return "", err
	}
	if err := stage.Begin(ctx, runID, assetID, stageSources(slices.Values(embedInputsFromPersisted(asset.Content, blocks)))); err != nil {
		return "", fmt.Errorf("stage chunks: %w", err)
	}
	return runID, nil
}

// embedNext embeds the next batch, or, once every chunk is settled, commits
// them and settles the run.
func (p *ProcessImageProcessor) embedNext(ctx context.Context, in ProcessImageTask, ext *models.ImageExtraction, st *imageState, lastAttempt bool) (StepOutput, error) {
	call, err := p.embedLoop().next(ctx, &st.Embed)
	if err != nil {
		return retry("process_image "+in.AssetID, err)
	}
	if call != nil {
		raw, err := encodeState(st)
		if err != nil {
			return StepOutput{}, err
		}
		return StepOutput{State: raw, HTTP: call}, nil
	}
	return p.embedAndSettle(ctx, in, ext, st.Embed.RunID, lastAttempt)
}

// embedAndSettle commits the run's chunks and settles the asset and
// extraction. A total embed failure retries (resuming from the checkpoint, no
// re-Extract), except on the last attempt, where it settles terminally so the
// asset never strands in `processing` once retries run out. The
// partial-vs-ready decision reads the quality flags persisted on the
// extraction, so it is identical on the fresh and resume paths.
func (p *ProcessImageProcessor) embedAndSettle(ctx context.Context, in ProcessImageTask, ext *models.ImageExtraction, runID string, lastAttempt bool) (StepOutput, error) {
	embedFailures, err := commitImageChunks(ctx, p.Deps, p.stages(), in.AssetID, runID)
	if err != nil {
		if _, ok := errors.AsType[*allChunksFailedError](err); ok && lastAttempt {
			// Nothing embedded, so the asset is not searchable; the retriable
			// reason lets the tenant re-extract once the embedder recovers.
			return settledOr(p.terminalReject(ctx, in, ext, models.UploadCodeServiceUnavailable, imageUnavailableReason))
		}
		return retry("process_image "+in.AssetID, err)
	}

	// description-ok/extraction-failed → partial (searchable, blocks absent,
	// retriable); a partial embed → partial; else ready.
	status := models.AssetStatusReady
	extStatus := models.ImageExtractionStatusComplete
	failureCode, failureReason := "", ""
	if (ext.DescriptionOK && !ext.ExtractionOK && ext.Shape != models.ImageShapeCreative) || embedFailures > 0 {
		status = models.AssetStatusPartial
		extStatus = models.ImageExtractionStatusPartial
		// Partial is not a hard failure: the description landed and the asset is
		// searchable, but structured extraction (or a chunk embed) did not fully
		// complete. Carry a code so the client can word it as retriable rather
		// than broken.
		failureCode = models.UploadCodeExtractionPartial
		failureReason = "the image was described and is searchable, but structured extraction did not fully complete"
	}
	if err := p.statusWriter().set(ctx, in.AssetID, status); err != nil {
		return retry("process_image", err)
	}
	// The run has settled searchable (partial|complete) and the normalized.png
	// exists — only now is it safe to publish the browser-drawable key.
	if err := p.exposeNormalizedDerivative(ctx, in); err != nil {
		return retry("process_image", err)
	}
	ext.Status = extStatus
	ext.FailureReason = failureReason
	ext.FailureCode = failureCode
	if err := p.Deps.Extractions.Update(ctx, ext); err != nil {
		return retry("process_image", err)
	}
	return done(status)
}

// commitImageChunks commits an image's staged chunks and meters the embeds.
// It returns how many chunks failed; a total embed failure (nothing landed
// though something was embeddable) is an *allChunksFailedError. A creative
// image with no text has nothing embeddable: ready, no chunks.
func commitImageChunks(ctx context.Context, d ImageDeps, stage chunkStage, assetID, runID string) (int, error) {
	committed, err := stage.Commit(ctx, runID, assetID, false)
	if err != nil {
		return 0, fmt.Errorf("store chunks: %w", err)
	}
	stats := stageStats(committed)
	if _, err := stats.settle(false); err != nil {
		return stats.Failures, err
	}
	// Metered once the chunks are stored, so a retry after a failed store
	// doesn't double-count. Covers ingestion and the re-embed.
	if stats.Tokens > 0 {
		d.Recorder.RecordResp(ctx, llm.VendorGemini, d.EmbedModel, "image_embed", llm.EmbedUsage{Tokens: stats.Tokens})
	}
	return stats.Failures, nil
}

// embedInputsFromPersisted builds the embed set from the stored state: the
// description (asset.Content) + the stored image_blocks (which already carry
// their persisted SourceAnchor). Each source is indexed by its position.
func embedInputsFromPersisted(description string, blocks []models.ImageBlock) []chunkSource {
	inputs := make([]chunkSource, 0, len(blocks)+1)
	if hasWords(description) {
		inputs = append(inputs, chunkSource{
			Text:   description,
			Anchor: &models.SourceAnchor{Kind: "image", Provenance: "image_extraction"},
			Label:  "Image description",
		})
	}
	for i := range blocks {
		if !hasWords(blocks[i].Text) {
			continue
		}
		inputs = append(inputs, chunkSource{Index: len(inputs), Text: blocks[i].Text, Anchor: blocks[i].Anchor, Label: fmt.Sprintf("Region %d", i+1)})
	}
	return inputs
}

// ensureExtraction loads the (asset, run_key) extraction or creates a fresh
// pending one with the run's models resolved.
func (p *ProcessImageProcessor) ensureExtraction(ctx context.Context, in ProcessImageTask) (*models.ImageExtraction, error) {
	return ensureExtractionRun(ctx, p.Deps.Extractions, "process_image", in.AssetID, in.RunKey, func(id string) *models.ImageExtraction {
		ext := &models.ImageExtraction{
			ID:      id,
			AssetID: in.AssetID,
			RunKey:  in.RunKey,
			Status:  models.ImageExtractionStatusPending,
		}
		p.freezeModels(ctx, in, ext)
		return ext
	})
}

// freezeModels resolves any model the run doesn't carry yet. A new run gets all
// three at creation; a loaded run keeps what it recorded, so retries never pick
// up an operator change made mid-run. The pinned model overrides extract. It
// reports whether it filled any model in.
func (p *ProcessImageProcessor) freezeModels(ctx context.Context, in ProcessImageTask, ext *models.ImageExtraction) bool {
	before := *ext
	if ext.ClassifyModel == "" {
		ext.ClassifyModel = p.Deps.Models.model(ctx, modelconfig.FlowVision, modelconfig.SlotClassify)
	}
	if ext.ExtractModel == "" {
		ext.ExtractModel = in.PinnedModel
		if ext.ExtractModel == "" {
			ext.ExtractModel = p.Deps.Models.model(ctx, modelconfig.FlowVision, modelconfig.SlotExtract)
		}
	}
	if ext.EscalateModel == "" {
		ext.EscalateModel = p.Deps.Models.model(ctx, modelconfig.FlowVision, modelconfig.SlotEscalate)
	}
	return ext.ClassifyModel != before.ClassifyModel || ext.ExtractModel != before.ExtractModel || ext.EscalateModel != before.EscalateModel
}

// persistBlocks maps the service Blocks to image_blocks rows (with image-region
// anchors) and replaces the extraction's set.
func (p *ProcessImageProcessor) persistBlocks(ctx context.Context, in ProcessImageTask, ext *models.ImageExtraction, blocks []imageclient.Block) error {
	rows := make([]models.ImageBlock, 0, len(blocks))
	for i, b := range blocks {
		id, err := models.NewID()
		if err != nil {
			return fmt.Errorf("process_image %s: new block id: %w", in.AssetID, err)
		}
		row := models.ImageBlock{
			ID:            id,
			ExtractionID:  ext.ID,
			AssetID:       in.AssetID,
			Index:         i,
			Kind:          b.Kind,
			Level:         b.Level,
			Text:          b.Text,
			Provenance:    b.Provenance,
			LowConfidence: b.LowConfidence,
			Anchor:        blockAnchor(b),
		}
		if len(b.Cells) > 0 {
			row.Cells = make([]models.ImageCell, 0, len(b.Cells))
			for _, c := range b.Cells {
				row.Cells = append(row.Cells, models.ImageCell{Row: c.Row, Col: c.Col, Text: c.Text})
			}
		}
		rows = append(rows, row)
	}
	if err := p.Deps.Blocks.ReplaceForExtraction(ctx, ext.ID, rows); err != nil {
		return fmt.Errorf("process_image %s: store blocks: %w", in.AssetID, err)
	}
	return nil
}

// stampFile writes the service-reported dimensions/animation onto the asset_file
// row the upload created. The original S3 key, mime, name, size, and dedupe
// checksum (computed by ogen at upload) are preserved. The browser-drawable
// normalized key is stamped separately, at settlement (exposeNormalizedDerivative),
// not here — this runs on the fresh path before the retryable embed, so it must
// not publish anything a still-pending or later-failed run would expose (AC4).
func (p *ProcessImageProcessor) stampFile(ctx context.Context, in ProcessImageTask, res *imageclient.ExtractResult) error {
	if p.Deps.Files == nil {
		return nil
	}
	file, err := p.Deps.Files.GetByAssetID(ctx, in.AssetID)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && file == nil) {
		// No file row (shouldn't happen — upload creates it). Non-fatal: metadata
		// stamping is best-effort; the extraction/description are the point.
		slog.WarnContext(ctx, "no asset_file to stamp", logging.AttrComponent, "jobs.process_image", "asset_id", in.AssetID)
		return nil
	}
	if err != nil {
		// A real read failure (not "no rows") must NOT be swallowed as "skip" — that
		// would silently drop the dimension stamping on a transient DB blip. Propagate
		// so the job retries.
		return fmt.Errorf("process_image %s: load asset_file: %w", in.AssetID, err)
	}
	file.Width = res.Normalized.Width
	file.Height = res.Normalized.Height
	file.IsAnimated = res.Normalized.IsAnimated
	if err := p.Deps.Files.Upsert(ctx, file); err != nil {
		return fmt.Errorf("process_image %s: stamp asset_file: %w", in.AssetID, err)
	}
	return nil
}

// exposeNormalizedDerivative records the browser-drawable normalized.png key on the
// asset_file so decorateFile can mint normalized_url — the copy an <img> renders for
// HEIC/TIFF, which no browser decodes. It runs ONLY after a run settles
// searchable (partial|complete): the key is deterministic and the derivative already
// exists in storage (image-service wrote it before the checkpoint), so a pending
// (describing) or failed run never publishes a URL (AC4). It also fills a missing
// thumbnail so the list preview cell draws the derivative instead of the original.
func (p *ProcessImageProcessor) exposeNormalizedDerivative(ctx context.Context, in ProcessImageTask) error {
	if p.Deps.Files == nil {
		return nil
	}
	file, err := p.Deps.Files.GetByAssetID(ctx, in.AssetID)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && file == nil) {
		slog.WarnContext(ctx, "no asset_file to expose normalized derivative", logging.AttrComponent, "jobs.process_image", "asset_id", in.AssetID)
		return nil
	}
	if err != nil {
		return fmt.Errorf("process_image %s: load asset_file: %w", in.AssetID, err)
	}
	normKey := storage.TenantKey(ctx, fmt.Sprintf("assets/%s/normalized.png", in.AssetID))
	file.NormalizedS3Key = &normKey
	// Only fill an empty slot — a real downscaled thumbnail (or a PDF's first-page
	// preview) is never clobbered.
	if file.ThumbnailS3Key == nil || *file.ThumbnailS3Key == "" {
		file.ThumbnailS3Key = &normKey
	}
	if err := p.Deps.Files.Upsert(ctx, file); err != nil {
		return fmt.Errorf("process_image %s: expose normalized derivative: %w", in.AssetID, err)
	}
	return nil
}

// accrueCost prices each vision call's token usage via the gemini vendor,
// snapshots the run total onto the extraction, and records a per-call usage
// event. Best-effort recording; the authoritative cost rides the
// extraction row updated by the caller.
func (p *ProcessImageProcessor) accrueCost(ctx context.Context, in ProcessImageTask, ext *models.ImageExtraction, usageList []imageclient.TokenUsage) {
	var costSum, inTok, outTok int64
	for _, u := range usageList {
		inTok += u.Input
		outTok += u.Output
		usg := vendors.Usage{}
		if u.Input > 0 {
			usg[vendors.KindInput] = u.Input
		}
		if u.Output > 0 {
			usg[vendors.KindOutput] = u.Output
		}
		if len(usg) == 0 {
			continue
		}
		if micros, _, ok := vendors.CostOf(llm.VendorGemini, u.Model, usg); ok {
			costSum += micros
		}
		p.Deps.Recorder.RecordResp(ctx, llm.VendorGemini, u.Model, "image_extract", llm.VisionUsage{
			Step:         u.Step,
			InputTokens:  u.Input,
			OutputTokens: u.Output,
		})
	}
	ext.InputTokens = inTok
	ext.OutputTokens = outTok
	ext.CostMicros = costSum
	if desc, ok := vendors.Get(llm.VendorGemini); ok {
		ext.PriceVersion = desc.Prices.Version
	}
}

// terminalReject marks the extraction + asset failed with a tenant-visible
// reason and its stable machine-readable code, and does NOT return an
// error (no retry).
func (p *ProcessImageProcessor) terminalReject(ctx context.Context, in ProcessImageTask, ext *models.ImageExtraction, code, reason string) error {
	ext.Status = models.ImageExtractionStatusFailed
	ext.FailureReason = reason
	ext.FailureCode = code
	if err := p.Deps.Extractions.Update(ctx, ext); err != nil {
		return fmt.Errorf("process_image %s: mark extraction failed: %w", in.AssetID, err)
	}
	slog.WarnContext(ctx, "image ingestion rejected", logging.AttrComponent, "jobs.process_image", "asset_id", in.AssetID, "reason", reason)
	return p.statusWriter().fail(ctx, in.AssetID, code, reason)
}

func (p *ProcessImageProcessor) statusWriter() assetStatusWriter {
	return assetStatusWriter{op: "process_image", assets: p.Deps.Assets, notifier: p.Deps.Notifier, label: "image", kind: models.AssetTypeImage}
}

// blockAnchor maps a service Block's image-region anchor to the persisted
// SourceAnchor jsonb (Kind "image", normalized bbox, provenance). Returns nil
// when the block carries no region.
func blockAnchor(b imageclient.Block) *models.SourceAnchor {
	prov := b.Provenance
	if prov == "" {
		prov = "image_extraction"
	}
	a := &models.SourceAnchor{Kind: "image", Provenance: prov}
	if b.Anchor.Bbox != nil {
		a.Bbox = &models.Bbox{X: b.Anchor.Bbox.X, Y: b.Anchor.Bbox.Y, W: b.Anchor.Bbox.W, H: b.Anchor.Bbox.H}
	}
	return a
}
