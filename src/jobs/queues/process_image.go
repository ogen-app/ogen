package queues

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/firebase/genkit/go/ai"
	"github.com/pgvector/pgvector-go"
	"github.com/riverqueue/river"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/genkit/embedopts"
	"github.com/ogen-app/ogen/src/infra/storage"
	"github.com/ogen-app/ogen/src/infra/vendors"
	"github.com/ogen-app/ogen/src/infra/vendors/llm"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
	"github.com/ogen-app/ogen/src/kernel/usage"
	imageclient "github.com/ogen-app/ogen/src/transport/grpc/client/image"
	"github.com/ogen-app/ogen/src/usecase/notify"
)

// ProcessImageQueue ingests a content-bank IMG asset (CON-281): presign the
// original + a normalized-derivative slot, run the full image-service pipeline
// (normalize + EXIF-strip + classify + per-shape structured extraction +
// description + alt text) in one Extract RPC, then persist the extraction +
// blocks, set the asset's description/alt text, embed the searchable text into
// the shared assets_chunks, and snapshot cost. Replaces the old synchronous
// imageprobe path (deleted, D6); image-service is the sole image authority.
const ProcessImageQueue = "process_image"

// ImageQueue is the dedicated River queue image jobs run on, isolated from the
// default queue so a backlog of heavy vision runs can't starve short ingestion
// for other tenants. Sized in server.go.
const ImageQueue = "image"

const (
	// imagePresignTTL bounds the presigned GET/PUT URLs handed to image-service.
	imagePresignTTL = time.Hour
	// normalizedImageContentType is the MIME bound on the normalized.png PUT.
	normalizedImageContentType = "image/png"
	defaultImageJobTimeout     = 10 * time.Minute
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
	SetImageResult(ctx context.Context, id, content, altText string, setAlt bool) error
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
	// Recorder meters vision usage on the existing gemini vendor (CON-86); Checker
	// gates the run against the tenant's cost cap BEFORE the vision spend. Both
	// nil-safe.
	Recorder *usage.Recorder
	Checker  *usage.Checker
	// EmbedModel is the price-map key for description/block embedding usage. The
	// Vision* model ids are config (never compiled in) passed through to
	// image-service; ConfidenceThreshold gates its one-shot escalation;
	// AltTextMaxChars is the alt-text generation target length.
	EmbedModel          string
	ClassifyModel       string
	ExtractModel        string
	EscalateModel       string
	ConfidenceThreshold float64
	AltTextMaxChars     int
	JobTimeout          time.Duration
	// Notifier drops an in-app notification to the asset's creator on a terminal
	// status (CON-242). Nil is a no-op.
	Notifier *notify.Service
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

// InsertOpts routes the job to the dedicated image queue and bounds retries.
// Transient failures (storage, gRPC Unavailable/DeadlineExceeded, embedder
// outage) retry with backoff; terminal ones (unsupported/vector/oversize/over-
// quota) short-circuit inside Work.
func (ProcessImageTask) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: ImageQueue, MaxAttempts: 5}
}

type ProcessImageProcessor struct {
	river.WorkerDefaults[ProcessImageTask]
	Deps ImageDeps
}

func init() {
	register(func(w *river.Workers, d Deps) {
		river.AddWorker(w, &ProcessImageProcessor{Deps: d.Image})
	})
}

func (p *ProcessImageProcessor) Work(ctx context.Context, job *river.Job[ProcessImageTask]) error {
	ctx = WithJobRequestID(ctx, job.JobRow)
	ctx = tenantctx.With(ctx, job.Args.TenantID)
	return p.process(ctx, job.Args, job.Attempt >= job.MaxAttempts)
}

func (p *ProcessImageProcessor) Timeout(*river.Job[ProcessImageTask]) time.Duration {
	if p.Deps.JobTimeout > 0 {
		return p.Deps.JobTimeout
	}
	return defaultImageJobTimeout
}

func (p *ProcessImageProcessor) process(ctx context.Context, in ProcessImageTask, lastAttempt bool) error {
	if p.Deps.Client == nil {
		slog.WarnContext(ctx, "image-service not configured", logging.AttrComponent, "jobs.process_image", "asset_id", in.AssetID)
		return nil
	}
	if p.Deps.Storage == nil || p.Deps.Extractions == nil || p.Deps.Blocks == nil {
		_ = p.setAssetStatus(ctx, in.AssetID, models.AssetStatusFailed)
		return fmt.Errorf("process_image %s: storage/repos not configured", in.AssetID)
	}
	// The description embed needs gemini_api_key (CON-104): checked up front so we
	// don't run the (paid) vision pipeline only to fail every embed. Retry rather
	// than fail — a key set via the secrets API takes effect without a restart;
	// give up (failed) only once attempts are exhausted.
	if !embedopts.Available(p.Deps.Embedder) {
		if lastAttempt {
			return p.setAssetStatus(ctx, in.AssetID, models.AssetStatusFailed)
		}
		slog.WarnContext(ctx, "embedder unavailable will retry", logging.AttrComponent, "jobs.process_image", "asset_id", in.AssetID)
		return fmt.Errorf("process_image %s: embedder unavailable", in.AssetID)
	}

	ext, err := p.ensureExtraction(ctx, in)
	if err != nil {
		return err
	}
	if ext.Status == models.ImageExtractionStatusComplete {
		return nil // idempotent re-drive of a finished run
	}
	if err := p.setAssetStatus(ctx, in.AssetID, models.AssetStatusProcessing); err != nil {
		return err
	}

	// Resume: if a PRIOR attempt already ran the (paid) vision pass and checkpointed
	// its results, skip the cost gate + Extract entirely and resume at embedding
	// from the persisted description + blocks. This is what stops a transient
	// downstream (embed) failure from re-invoking — and re-charging — the vision
	// model on every River retry.
	if ext.Status == models.ImageExtractionStatusDescribing {
		return p.resumeFromCheckpoint(ctx, in, ext)
	}

	// Cost-cap gate (CON-86 usage.Checker) BEFORE the vision spend. Nil checker =
	// no gate. Over-cap is terminal (reject before spend).
	if p.Deps.Checker != nil {
		if err := p.Deps.Checker.Enforce(ctx); err != nil {
			return p.terminalReject(ctx, in, ext, models.UploadCodeQuotaExceeded, "your usage limit has been reached — image processing was not started")
		}
	}

	// Presign the original (in) and the normalized-derivative slot (out). Bytes
	// never traverse gRPC — image-service reads/writes these directly.
	originalKey := storage.TenantKey(ctx, in.StorageKey)
	srcURL, err := p.Deps.Storage.PresignedGetURL(ctx, originalKey, imagePresignTTL)
	if err != nil {
		return fmt.Errorf("process_image %s: presign source: %w", in.AssetID, err)
	}
	normKey := storage.TenantKey(ctx, fmt.Sprintf("assets/%s/normalized.png", in.AssetID))
	dstURL, err := p.Deps.Storage.PresignedPutURL(ctx, normKey, normalizedImageContentType, imagePresignTTL)
	if err != nil {
		return fmt.Errorf("process_image %s: presign normalized: %w", in.AssetID, err)
	}

	res, err := p.Deps.Client.Extract(ctx, imageclient.ExtractOptions{
		SourceURL:           srcURL,
		DestPutURL:          dstURL,
		Filename:            in.OriginalName,
		ClassifyModel:       p.Deps.ClassifyModel,
		ExtractModel:        p.extractModel(in),
		EscalateModel:       p.Deps.EscalateModel,
		AltTextMaxChars:     p.Deps.AltTextMaxChars,
		ConfidenceThreshold: p.Deps.ConfidenceThreshold,
	})
	if err != nil {
		// Prefer the fine-grained reason image-service attaches to a terminal reject
		// (vector / too-large / dimensions / corrupt / unsupported) over the coarse
		// gRPC-code buckets, which stay as the fallback for an older service that
		// carries no ErrorInfo (CON-281 Phase 2).
		if code := models.UploadCodeFromImageReject(imageclient.RejectedReason(err)); code != "" {
			return p.terminalReject(ctx, in, ext, code, models.UploadRejectMessage(code))
		}
		switch {
		case imageclient.IsUnsupportedImage(err):
			return p.terminalReject(ctx, in, ext, models.UploadCodeUnsupportedMediaType, "the image format is not supported")
		case imageclient.IsInvalidImage(err):
			return p.terminalReject(ctx, in, ext, models.UploadCodeInvalidFile, "the image could not be processed (corrupt or too large)")
		case lastAttempt:
			// Transient (service down / deadline / 5xx). Retry — but on the FINAL
			// attempt settle to failed so the asset never strands in "processing"
			// once River gives up (mirrors process_audio/process_document handling).
			return p.terminalReject(ctx, in, ext, models.UploadCodeServiceUnavailable, "image processing is temporarily unavailable — please try again")
		default:
			return fmt.Errorf("process_image %s: extract: %w", in.AssetID, err)
		}
	}
	if res.RejectedReason != "" {
		// A structured verdict from the service. Phase 1 maps it to the coarse
		// "not a usable image" bucket; the image.v1 RejectedCode enum (CON-281
		// Phase 2) refines it to the exact reason without a client change.
		return p.terminalReject(ctx, in, ext, models.UploadCodeInvalidFile, res.RejectedReason)
	}

	// Persist the run's metadata + blocks, and the service-reported metadata onto
	// the asset_file row. Then set the description/alt text, embed, snapshot cost.
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
	p.accrueCost(ctx, in, ext, res.Usage)

	if err := p.persistBlocks(ctx, in, ext, res.Blocks); err != nil {
		return err
	}
	if err := p.stampFile(ctx, in, res); err != nil {
		return err
	}

	// Description → asset.Content; alt text → asset.AltText (guarded against a user
	// edit in SQL, D5). Title stays the upload filename.
	if err := p.Deps.Assets.SetImageResult(ctx, in.AssetID, res.Description, res.AltText, res.AltText != ""); err != nil {
		return fmt.Errorf("process_image %s: set description/alt: %w", in.AssetID, err)
	}

	// CHECKPOINT the successful vision pass BEFORE the retryable embed. The
	// extraction now durably holds the shape, quality flags, cost, blocks, and
	// description; marking it `describing` (== "vision done, embedding pending")
	// means a retry after an embed failure resumes here via resumeFromCheckpoint,
	// never re-calling the paid Extract.
	ext.Status = models.ImageExtractionStatusDescribing
	ext.FailureReason = ""
	ext.FailureCode = ""
	if err := p.Deps.Extractions.Update(ctx, ext); err != nil {
		return fmt.Errorf("process_image %s: checkpoint extraction: %w", in.AssetID, err)
	}

	return p.embedAndSettle(ctx, in, ext, embedInputsFromResult(res))
}

// resumeFromCheckpoint re-drives ONLY the embedding + settle steps of a run whose
// vision pass already completed and was checkpointed (status `describing`). It
// reloads the persisted description (asset.Content) + blocks and embeds them, so a
// transient embedder outage retries without a second (paid) Extract.
func (p *ProcessImageProcessor) resumeFromCheckpoint(ctx context.Context, in ProcessImageTask, ext *models.ImageExtraction) error {
	asset, err := p.Deps.Assets.GetByID(ctx, in.AssetID)
	if err != nil {
		return fmt.Errorf("process_image %s: reload asset for resume: %w", in.AssetID, err)
	}
	blocks, err := p.Deps.Blocks.ListByExtraction(ctx, ext.ID)
	if err != nil {
		return fmt.Errorf("process_image %s: reload blocks for resume: %w", in.AssetID, err)
	}
	return p.embedAndSettle(ctx, in, ext, embedInputsFromPersisted(asset.Content, blocks))
}

// embedInput is one text to embed with its anchor + citation label.
type embedInput struct {
	text   string
	anchor *models.SourceAnchor
	label  string
}

// embedInputsFromResult builds the embed set from a fresh Extract response.
func embedInputsFromResult(res *imageclient.ExtractResult) []embedInput {
	inputs := make([]embedInput, 0, len(res.Blocks)+1)
	if hasWords(res.Description) {
		inputs = append(inputs, embedInput{
			text:   res.Description,
			anchor: &models.SourceAnchor{Kind: "image", Provenance: "image_extraction"},
			label:  "Image description",
		})
	}
	for i, b := range res.Blocks {
		if !hasWords(b.Text) {
			continue
		}
		inputs = append(inputs, embedInput{text: b.Text, anchor: blockAnchor(b), label: fmt.Sprintf("Region %d", i+1)})
	}
	return inputs
}

// embedInputsFromPersisted rebuilds the embed set from checkpointed state on a
// resume: the description (asset.Content) + the stored image_blocks (which already
// carry their persisted SourceAnchor).
func embedInputsFromPersisted(description string, blocks []models.ImageBlock) []embedInput {
	inputs := make([]embedInput, 0, len(blocks)+1)
	if hasWords(description) {
		inputs = append(inputs, embedInput{
			text:   description,
			anchor: &models.SourceAnchor{Kind: "image", Provenance: "image_extraction"},
			label:  "Image description",
		})
	}
	for i := range blocks {
		if !hasWords(blocks[i].Text) {
			continue
		}
		inputs = append(inputs, embedInput{text: blocks[i].Text, anchor: blocks[i].Anchor, label: fmt.Sprintf("Region %d", i+1)})
	}
	return inputs
}

// embedAndSettle embeds the given inputs into assets_chunks and settles the asset
// + extraction to their terminal status. A total embed failure returns an error
// so the job retries (resuming from the checkpoint, no re-Extract). The
// partial-vs-ready decision reads the quality flags persisted on the extraction,
// so it is identical on the fresh and resume paths.
func (p *ProcessImageProcessor) embedAndSettle(ctx context.Context, in ProcessImageTask, ext *models.ImageExtraction, inputs []embedInput) error {
	embedFailures, err := p.embed(ctx, in, inputs)
	if err != nil {
		return err // transient embedder outage → retry (resumes from checkpoint)
	}

	// description-ok/extraction-failed → partial (searchable, blocks absent,
	// retriable); a partial embed → partial; else ready. The extraction-quality
	// signals come from the persisted extraction, so resume settles identically.
	status := models.AssetStatusReady
	extStatus := models.ImageExtractionStatusComplete
	failureCode, failureReason := "", ""
	if (ext.DescriptionOK && !ext.ExtractionOK && ext.Shape != models.ImageShapeCreative) || embedFailures > 0 {
		status = models.AssetStatusPartial
		extStatus = models.ImageExtractionStatusPartial
		// Partial is not a hard failure: the description landed and the asset is
		// searchable, but structured extraction (or a chunk embed) did not fully
		// complete. Carry a code so the client can word it as retriable rather
		// than broken (CON-281).
		failureCode = models.UploadCodeExtractionPartial
		failureReason = "the image was described and is searchable, but structured extraction did not fully complete"
	}
	if err := p.setAssetStatus(ctx, in.AssetID, status); err != nil {
		return err
	}
	ext.Status = extStatus
	ext.FailureReason = failureReason
	ext.FailureCode = failureCode
	return p.Deps.Extractions.Update(ctx, ext)
}

// ensureExtraction loads the (asset, run_key) extraction or creates a fresh
// pending one. Idempotent under a concurrent create via the unique index.
func (p *ProcessImageProcessor) ensureExtraction(ctx context.Context, in ProcessImageTask) (*models.ImageExtraction, error) {
	ext, err := p.Deps.Extractions.GetByAssetAndRunKey(ctx, in.AssetID, in.RunKey)
	if err == nil {
		return ext, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("process_image %s: load extraction: %w", in.AssetID, err)
	}
	id, err := models.NewID()
	if err != nil {
		return nil, fmt.Errorf("process_image %s: new extraction id: %w", in.AssetID, err)
	}
	ext = &models.ImageExtraction{
		ID:            id,
		AssetID:       in.AssetID,
		RunKey:        in.RunKey,
		Status:        models.ImageExtractionStatusPending,
		ClassifyModel: p.Deps.ClassifyModel,
		ExtractModel:  p.extractModel(in),
		EscalateModel: p.Deps.EscalateModel,
	}
	if err := p.Deps.Extractions.Create(ctx, ext); err != nil {
		if again, gErr := p.Deps.Extractions.GetByAssetAndRunKey(ctx, in.AssetID, in.RunKey); gErr == nil {
			return again, nil
		}
		return nil, fmt.Errorf("process_image %s: create extraction: %w", in.AssetID, err)
	}
	return ext, nil
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
// checksum (computed by ogen at upload) are preserved.
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

// embed embeds the given inputs into assets_chunks (anchored). Returns the count
// of chunks that failed to embed; a total embed failure (nothing landed though
// something was embeddable) is returned as an error so the job retries.
func (p *ProcessImageProcessor) embed(ctx context.Context, in ProcessImageTask, inputs []embedInput) (int, error) {
	chunks := make([]models.AssetChunk, 0, len(inputs))
	var embedAttempts, embedFailures int
	for idx, e := range inputs {
		embedAttempts++
		emb, eErr := p.Deps.Embedder.Embed(ctx, &ai.EmbedRequest{
			Input:   []*ai.Document{ai.DocumentFromText(e.text, nil)},
			Options: embedopts.Document(),
		})
		if eErr != nil || len(emb.Embeddings) != 1 {
			embedFailures++
			continue
		}
		label := e.label
		chunks = append(chunks, models.AssetChunk{
			ID:           fmt.Sprintf("%s:%d", in.AssetID, idx),
			AssetID:      in.AssetID,
			ChunkIndex:   idx,
			Content:      e.text,
			TokenCount:   estimateTokens(e.text),
			Embedding:    pgvector.NewHalfVector(emb.Embeddings[0].Embedding),
			Model:        p.Deps.Embedder.Name(),
			SourceLabel:  &label,
			SourceAnchor: e.anchor,
		})
	}

	// Every chunk failed to embed (transient embedder outage) — retry, unless there
	// was nothing embeddable (creative image with no text → ready, 0 chunks).
	if embedAttempts > 0 && len(chunks) == 0 {
		return embedFailures, fmt.Errorf("process_image %s: all %d chunk(s) failed to embed", in.AssetID, embedAttempts)
	}
	if len(chunks) > 0 && p.Deps.Chunks != nil {
		if err := p.Deps.Chunks.UpsertChunks(ctx, in.AssetID, chunks); err != nil {
			return embedFailures, fmt.Errorf("process_image %s: store chunks: %w", in.AssetID, err)
		}
	}
	return embedFailures, nil
}

// accrueCost prices each vision call's token usage via the gemini vendor,
// snapshots the run total onto the extraction, and records a per-call usage
// event (CON-86). Best-effort recording; the authoritative cost rides the
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
// reason and its stable machine-readable code (CON-281), and does NOT return an
// error (no retry).
func (p *ProcessImageProcessor) terminalReject(ctx context.Context, in ProcessImageTask, ext *models.ImageExtraction, code, reason string) error {
	ext.Status = models.ImageExtractionStatusFailed
	ext.FailureReason = reason
	ext.FailureCode = code
	if err := p.Deps.Extractions.Update(ctx, ext); err != nil {
		return fmt.Errorf("process_image %s: mark extraction failed: %w", in.AssetID, err)
	}
	slog.WarnContext(ctx, "image ingestion rejected", logging.AttrComponent, "jobs.process_image", "asset_id", in.AssetID, "reason", reason)
	return p.setAssetStatus(ctx, in.AssetID, models.AssetStatusFailed)
}

// setAssetStatus persists the asset status and announces terminal outcomes to
// the creator (CON-242). A nil Assets dep is a no-op.
func (p *ProcessImageProcessor) setAssetStatus(ctx context.Context, assetID, status string) error {
	if p.Deps.Assets == nil {
		return nil
	}
	if err := p.Deps.Assets.UpdateStatus(ctx, assetID, status); err != nil {
		return fmt.Errorf("process_image %s: set status %s: %w", assetID, status, err)
	}
	notifyAssetStatus(ctx, p.Deps.Notifier, p.Deps.Assets, assetID, status, "image")
	return nil
}

func (p *ProcessImageProcessor) extractModel(in ProcessImageTask) string {
	if in.PinnedModel != "" {
		return in.PinnedModel
	}
	return p.Deps.ExtractModel
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
