package queues

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"iter"
	"log/slog"
	"time"

	"github.com/riverqueue/river"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/storage"
	"github.com/ogen-app/ogen/src/infra/vendors/llm"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
	"github.com/ogen-app/ogen/src/kernel/usage"
	"github.com/ogen-app/ogen/src/transport/grpc/client/documents"
	"github.com/ogen-app/ogen/src/usecase/notify"
)

// ProcessDocumentQueue ingests an uploaded office/text document:
// download original.<ext> from object storage, parse it via document-service
// over gRPC (text -> source-anchored chunks), embed the chunks with the same
// Gemini embedder used for PDFs, and persist chunks + file metadata. The direct
// sibling of process_pdf, minus thumbnails/page rendering.
const ProcessDocumentQueue = "process_document"

// documentsParser is the narrow slice of the documents gRPC client the worker
// needs; a small fake satisfies it in tests. The embedder/storage/repo
// interfaces are shared with process_pdf (defined there in this package).
type documentsParser interface {
	Parse(ctx context.Context, r io.Reader, opts documents.Options) (*documents.Result, error)
}

// documentAssetWriter is the asset status surface plus MarkFailed, which records
// why a document failed. The asset repo satisfies it.
type documentAssetWriter interface {
	assetStatusUpdater
	MarkFailed(ctx context.Context, id, code, reason string) error
}

// DocumentDeps bundles the process_document worker's dependencies (built in
// server.go). A nil Client (no DOCUMENTS_SERVICE_ADDR configured) disables the
// job — it no-ops.
type DocumentDeps struct {
	Client   documentsParser
	Embedder chunkEmbedder
	Storage  blobStore
	Assets   documentAssetWriter
	Chunks   chunkUpserter
	Files    fileUpserter
	// Recorder + EmbedModel meter document-ingestion embedding usage.
	// nil Recorder = no-op. EmbedModel is the price-map key (cfg.EmbedModel).
	Recorder   *usage.Recorder
	EmbedModel string
	// Notifier drops an in-app notification to the asset's creator when ingest
	// reaches a terminal status. Nil is a no-op.
	Notifier *notify.Service
}

// ProcessDocumentTask carries the asset to ingest. The document bytes are NOT in
// the args (River args are JSON in Postgres, far too small for a document); the
// worker re-reads the original from storage on each attempt. StorageKey is the
// tenant-relative object path (e.g. "assets/<id>/original.docx") the upload
// handler wrote, so the worker downloads the right extension without guessing.
type ProcessDocumentTask struct {
	AssetID      string `json:"asset_id"`
	TenantID     string `json:"tenant_id"`
	OriginalName string `json:"original_name"`
	MimeType     string `json:"mime_type"`
	StorageKey   string `json:"storage_key"`
}

func (ProcessDocumentTask) Kind() string { return ProcessDocumentQueue }

// InsertOpts bounds retries. Transient failures (storage, gRPC Unavailable /
// DeadlineExceeded, embedder outage) retry with backoff; terminal ones
// (unsupported/corrupt/encrypted document) short-circuit to "failed" inside Work.
func (ProcessDocumentTask) InsertOpts() river.InsertOpts {
	return river.InsertOpts{MaxAttempts: 5}
}

type ProcessDocumentProcessor struct {
	river.WorkerDefaults[ProcessDocumentTask]
	Deps DocumentDeps
}

func init() {
	register(func(w *river.Workers, d Deps) {
		river.AddWorker(w, &ProcessDocumentProcessor{Deps: d.Document})
	})
}

// Work scopes the job to the asset's tenant and processes it. lastAttempt lets
// process() flip an otherwise-retryable embedder outage to a terminal "failed"
// instead of abandoning the asset in "processing".
func (p *ProcessDocumentProcessor) Work(ctx context.Context, job *river.Job[ProcessDocumentTask]) error {
	ctx = WithJobRequestID(ctx, job.JobRow)
	ctx = tenantctx.With(ctx, job.Args.TenantID)
	return p.process(ctx, job.Args, job.Attempt >= job.MaxAttempts)
}

// Timeout covers a worst-case extraction (large workbook exploding into many
// row chunks) plus per-chunk embedding.
func (p *ProcessDocumentProcessor) Timeout(*river.Job[ProcessDocumentTask]) time.Duration {
	return 16 * time.Minute
}

func (p *ProcessDocumentProcessor) process(ctx context.Context, in ProcessDocumentTask, lastAttempt bool) error {
	if p.Deps.Client == nil {
		slog.WarnContext(ctx, "document-service not configured", logging.AttrComponent, "jobs.process_document", "asset_id", in.AssetID)
		return nil
	}
	status := p.statusWriter()
	if p.Deps.Storage == nil {
		// Best-effort status write; the returned error is more descriptive.
		_ = status.fail(ctx, in.AssetID, models.UploadCodeInternalError, "document processing is not configured")
		return fmt.Errorf("process_document %s: storage not configured", in.AssetID)
	}
	giveUp := func() error {
		return status.fail(ctx, in.AssetID, models.UploadCodeServiceUnavailable, documentUnavailableReason)
	}
	if ok, err := requireEmbedder(ctx, p.Deps.Embedder, "process_document", in.AssetID, lastAttempt, giveUp); !ok {
		return err
	}
	if err := status.set(ctx, in.AssetID, models.AssetStatusProcessing); err != nil {
		return err
	}

	key := storage.TenantKey(ctx, in.StorageKey)
	data, err := downloadOriginal(ctx, p.Deps.Storage, "process_document", in.AssetID, key, "document")
	if err != nil {
		return err
	}
	// Unsupported/corrupt/encrypted documents are terminal; service-down and
	// deadline errors retry.
	res, err := p.Deps.Client.Parse(ctx, bytes.NewReader(data), documents.Options{
		Filename:    in.OriginalName,
		ContentType: in.MimeType,
	})
	if err != nil {
		if isTerminalParseErr(err) {
			slog.WarnContext(ctx, "unparseable document", logging.AttrComponent, "jobs.process_document", "asset_id", in.AssetID, logging.AttrError, err)
			return status.fail(ctx, in.AssetID, models.UploadCodeInvalidFile, "the document could not be read (corrupt, encrypted, or unsupported)")
		}
		return fmt.Errorf("process_document %s: parse: %w", in.AssetID, err)
	}

	chunks, stats := embedChunks(ctx, p.Deps.Embedder, in.AssetID, documentChunkSources(res.Chunks))
	if err := storeChunks(ctx, p.Deps.Chunks, "process_document", in.AssetID, chunks, false); err != nil {
		return err
	}
	// The file row is retried so the asset never lands "ready" without it.
	if err := p.persistFile(ctx, in, key, len(data)); err != nil {
		return err
	}

	final, err := stats.settle(lastAttempt)
	if err != nil {
		return fmt.Errorf("process_document %s: %w", in.AssetID, err)
	}
	if final == models.AssetStatusFailed {
		return status.fail(ctx, in.AssetID, models.UploadCodeServiceUnavailable, documentUnavailableReason)
	}
	if err := status.set(ctx, in.AssetID, final); err != nil {
		return err
	}

	// One usage event per ingest, recorded only after the durable writes so a
	// retry from a late failure can't double-count. The Gemini embed response
	// carries no usage, so the tokens are the chunks' counts.
	if stats.Tokens > 0 {
		p.Deps.Recorder.RecordResp(ctx, llm.VendorGemini, p.Deps.EmbedModel, "document_extract", llm.EmbedUsage{Tokens: stats.Tokens})
	}
	return nil
}

// documentUnavailableReason is the tenant-visible reason when the embedder
// stays down for every attempt.
const documentUnavailableReason = "document processing is temporarily unavailable — please try again"

// documentChunkSources yields the parsed chunks with their service-reported
// token counts, citation labels and anchors.
func documentChunkSources(chunks []documents.Chunk) iter.Seq[chunkSource] {
	return func(yield func(chunkSource) bool) {
		for _, ch := range chunks {
			src := chunkSource{Index: ch.Index, Text: ch.Text, Tokens: ch.TokenCount, Label: ch.SourceLabel}
			src.Anchor, src.PageStart, src.PageEnd = documentAnchor(ch.Anchor)
			if !yield(src) {
				return
			}
		}
	}
}

func (p *ProcessDocumentProcessor) statusWriter() assetStatusWriter {
	return assetStatusWriter{
		op: "process_document", assets: p.Deps.Assets, marker: p.Deps.Assets,
		notifier: p.Deps.Notifier, label: "document", kind: models.AssetTypeDocument,
	}
}

// persistFile upserts the asset_file row (s3 key, mime, size). The Upsert
// conflicts on asset_id, so it is idempotent across retries; the error is
// returned so a write failure retries the job rather than leaving the asset
// "ready" without its file row. No thumbnail / page count for documents in v1.
// A nil Files dep is a no-op.
func (p *ProcessDocumentProcessor) persistFile(ctx context.Context, in ProcessDocumentTask, s3Key string, size int) error {
	if p.Deps.Files == nil {
		return nil
	}
	fileID, err := models.NewID()
	if err != nil {
		return fmt.Errorf("process_document %s: new file id: %w", in.AssetID, err)
	}
	if err := p.Deps.Files.Upsert(ctx, &models.AssetFile{
		ID:           fileID,
		AssetID:      in.AssetID,
		OriginalName: in.OriginalName,
		MimeType:     in.MimeType,
		SizeBytes:    int64(size),
		S3Key:        s3Key,
	}); err != nil {
		return fmt.Errorf("process_document %s: upsert asset_files: %w", in.AssetID, err)
	}
	return nil
}

// documentAnchor maps a document-service chunk anchor to its persisted form: the
// jsonb source_anchor plus the retained page_start/page_end columns (populated
// only for page-flow chunks so existing page-based readers keep working). A
// chunk with no anchor (Kind == "") persists all three as nil.
func documentAnchor(a documents.Anchor) (anchor *models.SourceAnchor, pageStart, pageEnd *int) {
	if a.Kind == "" {
		return nil, nil, nil
	}
	anchor = &models.SourceAnchor{
		Kind:        a.Kind,
		Page:        a.PageStart,
		Slide:       a.Slide,
		Sheet:       a.Sheet,
		CellRange:   a.CellRange,
		HeadingPath: a.HeadingPath,
	}
	if a.Kind == "page" {
		pageStart, pageEnd = positiveIntPtr(a.PageStart), positiveIntPtr(a.PageEnd)
	}
	return anchor, pageStart, pageEnd
}
