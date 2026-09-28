package queues

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"iter"
	"log/slog"
	"time"

	"github.com/firebase/genkit/go/ai"
	"github.com/riverqueue/river"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/storage"
	"github.com/ogen-app/ogen/src/infra/vendors/llm"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
	"github.com/ogen-app/ogen/src/kernel/usage"
	"github.com/ogen-app/ogen/src/transport/grpc/client/pdf"
	"github.com/ogen-app/ogen/src/usecase/notify"
)

// ProcessPDFQueue ingests an uploaded PDF: download original.pdf from
// object storage, parse it via pdf-service over gRPC (text -> page-aware chunks
// + thumbnail), embed the chunks, and persist chunks + file metadata. It is a
// durable, retried River job, so a crash or transient failure never strands
// an asset in "processing".
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
	// Recorder + EmbedModel meter PDF-ingestion embedding usage. nil
	// Recorder = no-op. EmbedModel is the price-map key (cfg.EmbedModel).
	Recorder   *usage.Recorder
	EmbedModel string
	// Notifier drops an in-app notification to the asset's creator when ingest
	// reaches a terminal status. Nil is a no-op.
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
	status := p.statusWriter()
	if p.Deps.Storage == nil {
		// Best-effort status write; the returned error is more descriptive.
		_ = status.set(ctx, in.AssetID, models.AssetStatusFailed)
		return fmt.Errorf("process_pdf %s: storage not configured", in.AssetID)
	}
	giveUp := func() error { return status.set(ctx, in.AssetID, models.AssetStatusFailed) }
	if ok, err := requireEmbedder(ctx, p.Deps.Embedder, "process_pdf", in.AssetID, lastAttempt, giveUp); !ok {
		return err
	}
	if err := status.set(ctx, in.AssetID, models.AssetStatusProcessing); err != nil {
		return err
	}

	key := storage.TenantKey(ctx, fmt.Sprintf("assets/%s/original.pdf", in.AssetID))
	data, err := downloadOriginal(ctx, p.Deps.Storage, "process_pdf", in.AssetID, key, "pdf")
	if err != nil {
		return err
	}
	// Corrupt/unsupported PDFs are terminal; service-down/deadline retry.
	res, err := p.Deps.Client.Parse(ctx, bytes.NewReader(data), pdf.Options{
		Filename:        in.OriginalName,
		RenderThumbnail: true,
		ThumbnailDPI:    p.thumbnailDPI(),
	})
	if err != nil {
		if isTerminalParseErr(err) {
			slog.WarnContext(ctx, "unparseable pdf", logging.AttrComponent, "jobs.process_pdf", "asset_id", in.AssetID, logging.AttrError, err)
			return status.set(ctx, in.AssetID, models.AssetStatusFailed)
		}
		return fmt.Errorf("process_pdf %s: parse: %w", in.AssetID, err)
	}

	chunks, stats := embedChunks(ctx, p.Deps.Embedder, in.AssetID, pdfChunkSources(res.Chunks))
	if err := storeChunks(ctx, p.Deps.Chunks, "process_pdf", in.AssetID, chunks, false); err != nil {
		return err
	}
	// One usage event per ingest; the Gemini embed response carries no usage,
	// so the tokens are the embedded chunks' estimates.
	p.Deps.Recorder.RecordResp(ctx, llm.VendorGemini, p.Deps.EmbedModel, "pdf_extract", llm.EmbedUsage{Tokens: stats.Tokens})

	// The thumbnail is non-fatal; the file row is retried so the asset never
	// lands "ready" without its file row, page count or thumbnail.
	thumbKey := p.uploadThumbnail(ctx, in.AssetID, res.ThumbnailPNG)
	if err := p.persistFile(ctx, in, key, thumbKey, len(data), res.PageCount); err != nil {
		return err
	}

	final, err := stats.settle(lastAttempt)
	if err != nil {
		return fmt.Errorf("process_pdf %s: %w", in.AssetID, err)
	}
	return status.set(ctx, in.AssetID, final)
}

// pdfChunkSources yields the parsed chunks with their page bounds. pdfium pages
// are 1-based, so a zero bound means unknown and persists as NULL.
func pdfChunkSources(chunks []pdf.Chunk) iter.Seq[chunkSource] {
	return func(yield func(chunkSource) bool) {
		for _, ch := range chunks {
			src := chunkSource{
				Index:     ch.Index,
				Text:      ch.Text,
				PageStart: positiveIntPtr(ch.PageStart),
				PageEnd:   positiveIntPtr(ch.PageEnd),
			}
			if !yield(src) {
				return
			}
		}
	}
}

func (p *ProcessPDFProcessor) thumbnailDPI() int {
	if p.Deps.ThumbnailDPI > 0 {
		return p.Deps.ThumbnailDPI
	}
	return thumbnailDPIDefault
}

func (p *ProcessPDFProcessor) statusWriter() assetStatusWriter {
	return assetStatusWriter{op: "process_pdf", assets: p.Deps.Assets, notifier: p.Deps.Notifier, label: "document", kind: models.AssetTypePDF}
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
