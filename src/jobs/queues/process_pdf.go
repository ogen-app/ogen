package queues

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/firebase/genkit/go/ai"
	"github.com/pgvector/pgvector-go"
	"github.com/riverqueue/river"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/genkit/embedopts"
	"github.com/ogen-app/ogen/src/infra/storage"
	"github.com/ogen-app/ogen/src/infra/vendors/llm"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
	"github.com/ogen-app/ogen/src/kernel/usage"
	"github.com/ogen-app/ogen/src/transport/grpc/client/pdf"
	"github.com/ogen-app/ogen/src/usecase/notify"
)

// ProcessPDFQueue ingests an uploaded PDF (CON-103): download original.pdf from
// object storage, parse it via pdf-service over gRPC (text -> page-aware chunks
// + thumbnail), embed the chunks, and persist chunks + file metadata. Replaces
// the old fire-and-forget goroutine with a durable, retried River job, so a
// crash or transient failure no longer strands an asset in "processing".
const ProcessPDFQueue = "process_pdf"

// thumbnailDPIDefault is used when PDFDeps.ThumbnailDPI is 0.
const thumbnailDPIDefault = 96

// Narrow dependency interfaces — the real client/embedder/storage/repos satisfy
// these structurally; tests provide small fakes.

type pdfParser interface {
	Parse(ctx context.Context, r io.Reader, opts pdf.Options) (*pdf.Result, error)
}

type chunkEmbedder interface {
	Embed(ctx context.Context, req *ai.EmbedRequest) (*ai.EmbedResponse, error)
	Name() string
}

type blobStore interface {
	Download(ctx context.Context, key string) (io.ReadCloser, error)
	Upload(ctx context.Context, key string, r io.Reader, size int64, contentType string) (string, error)
}

type assetStatusUpdater interface {
	UpdateStatus(ctx context.Context, id, status string) error
	CreatorOf(ctx context.Context, id string) (string, error)
}

type chunkUpserter interface {
	UpsertChunks(ctx context.Context, assetID string, chunks []models.AssetChunk) error
}

type fileUpserter interface {
	Upsert(ctx context.Context, file *models.AssetFile) error
}

// PDFDeps bundles the process_pdf worker's dependencies (built in server.go). A
// nil Client (no PDF_SERVICE_ADDR configured) disables the job — it no-ops.
type PDFDeps struct {
	Client       pdfParser
	Embedder     chunkEmbedder
	Storage      blobStore
	Assets       assetStatusUpdater
	Chunks       chunkUpserter
	Files        fileUpserter
	ThumbnailDPI int
	// Recorder + EmbedModel meter PDF-ingestion embedding usage (CON-86). nil
	// Recorder = no-op. EmbedModel is the price-map key (cfg.EmbedModel).
	Recorder   *usage.Recorder
	EmbedModel string
	// Notifier drops an in-app notification to the asset's creator when ingest
	// reaches a terminal status (CON-242). Nil is a no-op.
	Notifier *notify.Service
}

// ProcessPDFTask carries the asset to ingest. The PDF bytes are NOT in the args
// (River args are JSON in Postgres, far too small for a 50 MB PDF); the worker
// re-reads original.pdf from storage on each attempt.
type ProcessPDFTask struct {
	AssetID      string `json:"asset_id"`
	TenantID     string `json:"tenant_id"`
	OriginalName string `json:"original_name"`
	MimeType     string `json:"mime_type"`
}

func (ProcessPDFTask) Kind() string { return ProcessPDFQueue }

// InsertOpts bounds retries. Transient failures (storage, gRPC Unavailable /
// DeadlineExceeded, embedder outage) retry with backoff; terminal ones (corrupt
// PDF) short-circuit to "failed" inside Work.
func (ProcessPDFTask) InsertOpts() river.InsertOpts {
	return river.InsertOpts{MaxAttempts: 5}
}

type ProcessPDFProcessor struct {
	river.WorkerDefaults[ProcessPDFTask]
	Deps PDFDeps
}

func init() {
	register(func(w *river.Workers, d Deps) {
		river.AddWorker(w, &ProcessPDFProcessor{Deps: d.PDF})
	})
}

// Work scopes the job to the asset's tenant and processes it. lastAttempt lets
// process() flip an otherwise-retryable embedder outage to a terminal "failed"
// instead of abandoning the asset in "processing".
func (p *ProcessPDFProcessor) Work(ctx context.Context, job *river.Job[ProcessPDFTask]) error {
	ctx = WithJobRequestID(ctx, job.JobRow)
	ctx = tenantctx.With(ctx, job.Args.TenantID)
	return p.process(ctx, job.Args, job.Attempt >= job.MaxAttempts)
}

// Timeout covers a worst-case parse (large PDF) plus per-chunk embedding.
func (p *ProcessPDFProcessor) Timeout(*river.Job[ProcessPDFTask]) time.Duration {
	return 16 * time.Minute
}

func (p *ProcessPDFProcessor) process(ctx context.Context, in ProcessPDFTask, lastAttempt bool) error {
	if p.Deps.Client == nil {
		slog.WarnContext(ctx, "pdf-service not configured", logging.AttrComponent, "jobs.process_pdf", "asset_id", in.AssetID)
		return nil
	}
	if p.Deps.Storage == nil {
		// Best-effort status write; we return the more descriptive error below.
		_ = p.setStatus(ctx, in.AssetID, models.AssetStatusFailed)
		return fmt.Errorf("process_pdf %s: storage not configured", in.AssetID)
	}

	// No gemini_api_key configured yet (CON-104): checked up front so we don't
	// download + parse the PDF only to fail every chunk embed. Retry rather than
	// fail — a key set via the secrets API takes effect without a restart, so a
	// later attempt can succeed; give up (failed) only once attempts are
	// exhausted, mirroring the all-chunks-failed path below, so the asset never
	// stays stuck in "processing".
	if !embedopts.Available(p.Deps.Embedder) {
		if lastAttempt {
			return p.setStatus(ctx, in.AssetID, models.AssetStatusFailed)
		}
		slog.WarnContext(ctx, "embedder unavailable will retry", logging.AttrComponent, "jobs.process_pdf", "asset_id", in.AssetID)
		return fmt.Errorf("process_pdf %s: embedder unavailable", in.AssetID)
	}

	if err := p.setStatus(ctx, in.AssetID, models.AssetStatusProcessing); err != nil {
		return err
	}

	// 1. Re-read original.pdf (the upload handler stored it before enqueue).
	//    Transient read errors retry.
	key := storage.TenantKey(ctx, fmt.Sprintf("assets/%s/original.pdf", in.AssetID))
	rc, err := p.Deps.Storage.Download(ctx, key)
	if err != nil {
		return fmt.Errorf("process_pdf %s: download %s: %w", in.AssetID, key, err)
	}
	data, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil {
		return fmt.Errorf("process_pdf %s: read pdf: %w", in.AssetID, err)
	}

	// 2. Parse via pdf-service. Corrupt/unsupported PDFs are terminal (no retry).
	res, err := p.Deps.Client.Parse(ctx, bytes.NewReader(data), pdf.Options{
		Filename:        in.OriginalName,
		RenderThumbnail: true,
		ThumbnailDPI:    p.thumbnailDPI(),
	})
	if err != nil {
		if isTerminalParseErr(err) {
			slog.WarnContext(ctx, "unparseable pdf", logging.AttrComponent, "jobs.process_pdf", "asset_id", in.AssetID, logging.AttrError, err)
			return p.setStatus(ctx, in.AssetID, models.AssetStatusFailed)
		}
		return fmt.Errorf("process_pdf %s: parse: %w", in.AssetID, err)
	}

	// 3. Embed each chunk that has words.
	chunks := make([]models.AssetChunk, 0, len(res.Chunks))
	var embedAttempts, embedFailures int
	var totalEmbedTokens int64
	for _, ch := range res.Chunks {
		if !hasWords(ch.Text) {
			continue
		}
		embedAttempts++
		emb, eErr := p.Deps.Embedder.Embed(ctx, &ai.EmbedRequest{
			Input:   []*ai.Document{ai.DocumentFromText(ch.Text, nil)},
			Options: embedopts.Document(),
		})
		if eErr != nil || len(emb.Embeddings) != 1 {
			embedFailures++
			continue
		}
		tokens := estimateTokens(ch.Text)
		totalEmbedTokens += int64(tokens)
		chunk := models.AssetChunk{
			ID:         fmt.Sprintf("%s:%d", in.AssetID, ch.Index),
			AssetID:    in.AssetID,
			ChunkIndex: ch.Index,
			Content:    ch.Text,
			TokenCount: tokens,
			Embedding:  pgvector.NewHalfVector(emb.Embeddings[0].Embedding),
			Model:      p.Deps.Embedder.Name(),
		}
		// Page bounds are optional: pdfium pages are 1-based, so a zero value
		// means "unknown" — leave the pointers nil rather than store page 0.
		if ch.PageStart > 0 {
			ps := ch.PageStart
			chunk.PageStart = &ps
		}
		if ch.PageEnd > 0 {
			pe := ch.PageEnd
			chunk.PageEnd = &pe
		}
		chunks = append(chunks, chunk)
	}

	if len(chunks) > 0 && p.Deps.Chunks != nil {
		if err := p.Deps.Chunks.UpsertChunks(ctx, in.AssetID, chunks); err != nil {
			return fmt.Errorf("process_pdf %s: store chunks: %w", in.AssetID, err)
		}
	}

	// CON-86: one usage event per PDF ingest (sum of embedded-chunk token
	// estimates; the Gemini embed response carries no usage). Nil recorder = no-op.
	p.Deps.Recorder.RecordResp(ctx, llm.VendorGemini, p.Deps.EmbedModel, "pdf_extract", llm.EmbedUsage{Tokens: totalEmbedTokens})

	// 4. Thumbnail (non-fatal). 5. File metadata — retried on failure so the
	//    asset never lands "ready" without its file row / page count / thumbnail.
	thumbKey := p.uploadThumbnail(ctx, in.AssetID, res.ThumbnailPNG)
	if err := p.persistFile(ctx, in, key, thumbKey, len(data), res.PageCount); err != nil {
		return err
	}

	// 6. Final status. Propagate a write failure so the worker retries rather
	//    than reporting success with the asset stuck in "processing".
	switch {
	case embedAttempts == 0:
		// No embeddable text (empty or image-only PDF) — ready with 0 chunks.
		return p.setStatus(ctx, in.AssetID, models.AssetStatusReady)
	case len(chunks) == 0:
		// Every chunk failed to embed — almost always a transient embedder
		// outage. Retry; give up (failed) only once attempts are exhausted, so
		// the asset never stays stuck in "processing".
		if lastAttempt {
			return p.setStatus(ctx, in.AssetID, models.AssetStatusFailed)
		}
		return fmt.Errorf("process_pdf %s: all %d chunk(s) failed to embed", in.AssetID, embedAttempts)
	case embedFailures > 0:
		return p.setStatus(ctx, in.AssetID, models.AssetStatusPartial)
	default:
		return p.setStatus(ctx, in.AssetID, models.AssetStatusReady)
	}
}

func (p *ProcessPDFProcessor) thumbnailDPI() int {
	if p.Deps.ThumbnailDPI > 0 {
		return p.Deps.ThumbnailDPI
	}
	return thumbnailDPIDefault
}

// setStatus persists the asset status, returning the error so callers can fail
// the job rather than reporting success with an unpersisted status. A nil Assets
// dep (status updates disabled) is a no-op.
func (p *ProcessPDFProcessor) setStatus(ctx context.Context, assetID, status string) error {
	if p.Deps.Assets == nil {
		return nil
	}
	if err := p.Deps.Assets.UpdateStatus(ctx, assetID, status); err != nil {
		return fmt.Errorf("process_pdf %s: set status %s: %w", assetID, status, err)
	}
	// CON-242: announce terminal outcomes to the asset's creator (no-op for the
	// intermediate "processing" write).
	notifyAssetStatus(ctx, p.Deps.Notifier, p.Deps.Assets, assetID, status, "document", models.AssetTypePDF)
	return nil
}

func (p *ProcessPDFProcessor) uploadThumbnail(ctx context.Context, assetID string, png []byte) *string {
	if len(png) == 0 {
		return nil
	}
	k := storage.TenantKey(ctx, fmt.Sprintf("assets/%s/thumbnail.png", assetID))
	if _, err := p.Deps.Storage.Upload(ctx, k, bytes.NewReader(png), int64(len(png)), "image/png"); err != nil {
		slog.WarnContext(ctx, "thumbnail upload failed", logging.AttrComponent, "jobs.process_pdf", "asset_id", assetID, logging.AttrError, err)
		return nil
	}
	return &k
}

// persistFile upserts the asset_file row (page count, thumbnail key, s3 key).
// The Upsert conflicts on asset_id, so it is idempotent across retries; the
// error is returned so a write failure retries the job rather than leaving the
// asset "ready" without its file row. A nil Files dep is a no-op.
func (p *ProcessPDFProcessor) persistFile(ctx context.Context, in ProcessPDFTask, s3Key string, thumbKey *string, size, pageCount int) error {
	if p.Deps.Files == nil {
		return nil
	}
	fileID, err := models.NewID()
	if err != nil {
		return fmt.Errorf("process_pdf %s: new file id: %w", in.AssetID, err)
	}
	var pcPtr *int
	if pageCount > 0 {
		pc := pageCount
		pcPtr = &pc
	}
	if err := p.Deps.Files.Upsert(ctx, &models.AssetFile{
		ID:             fileID,
		AssetID:        in.AssetID,
		OriginalName:   in.OriginalName,
		MimeType:       in.MimeType,
		SizeBytes:      int64(size),
		S3Key:          s3Key,
		ThumbnailS3Key: thumbKey,
		PageCount:      pcPtr,
	}); err != nil {
		return fmt.Errorf("process_pdf %s: upsert asset_files: %w", in.AssetID, err)
	}
	return nil
}

// isTerminalParseErr reports whether a pdf-service Parse error is terminal (the
// PDF is corrupt / encrypted / unsupported) and must not be retried. gRPC
// transport errors (Unavailable, DeadlineExceeded) and Internal are transient.
func isTerminalParseErr(err error) bool {
	st, ok := grpcstatus.FromError(err)
	if !ok {
		return false
	}
	switch st.Code() {
	case codes.InvalidArgument, codes.FailedPrecondition, codes.Unimplemented:
		return true
	default:
		return false
	}
}

// estimateTokens mirrors flows.EstimateTokens (≈4 chars/token). Kept local so
// the queue package doesn't depend on genkit/flows.
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
