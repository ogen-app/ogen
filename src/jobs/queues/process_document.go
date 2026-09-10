package queues

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/firebase/genkit/go/ai"
	"github.com/pgvector/pgvector-go"
	"github.com/riverqueue/river"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/genkit/embedopts"
	"github.com/ogen-app/ogen/src/infra/storage"
	"github.com/ogen-app/ogen/src/infra/vendors/llm"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
	"github.com/ogen-app/ogen/src/kernel/usage"
	"github.com/ogen-app/ogen/src/transport/grpc/client/documents"
	"github.com/ogen-app/ogen/src/usecase/notify"
)

// ProcessDocumentQueue ingests an uploaded office/text document (CON-280):
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

// DocumentDeps bundles the process_document worker's dependencies (built in
// server.go). A nil Client (no DOCUMENTS_SERVICE_ADDR configured) disables the
// job — it no-ops.
type DocumentDeps struct {
	Client   documentsParser
	Embedder chunkEmbedder
	Storage  blobStore
	Assets   assetStatusUpdater
	Chunks   chunkUpserter
	Files    fileUpserter
	// Recorder + EmbedModel meter document-ingestion embedding usage (CON-86).
	// nil Recorder = no-op. EmbedModel is the price-map key (cfg.EmbedModel).
	Recorder   *usage.Recorder
	EmbedModel string
	// Notifier drops an in-app notification to the asset's creator when ingest
	// reaches a terminal status (CON-242). Nil is a no-op.
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
	if p.Deps.Storage == nil {
		// Best-effort status write; we return the more descriptive error below.
		_ = p.setStatus(ctx, in.AssetID, models.AssetStatusFailed)
		return fmt.Errorf("process_document %s: storage not configured", in.AssetID)
	}

	// No gemini_api_key configured yet (CON-104): checked up front so we don't
	// download + parse the document only to fail every chunk embed. Retry rather
	// than fail — a key set via the secrets API takes effect without a restart,
	// so a later attempt can succeed; give up (failed) only once attempts are
	// exhausted, so the asset never stays stuck in "processing".
	if !embedopts.Available(p.Deps.Embedder) {
		if lastAttempt {
			return p.setStatus(ctx, in.AssetID, models.AssetStatusFailed)
		}
		slog.WarnContext(ctx, "embedder unavailable will retry", logging.AttrComponent, "jobs.process_document", "asset_id", in.AssetID)
		return fmt.Errorf("process_document %s: embedder unavailable", in.AssetID)
	}

	if err := p.setStatus(ctx, in.AssetID, models.AssetStatusProcessing); err != nil {
		return err
	}

	// 1. Re-read the original (the upload handler stored it before enqueue).
	//    Transient read errors retry.
	key := storage.TenantKey(ctx, in.StorageKey)
	rc, err := p.Deps.Storage.Download(ctx, key)
	if err != nil {
		return fmt.Errorf("process_document %s: download %s: %w", in.AssetID, key, err)
	}
	data, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil {
		return fmt.Errorf("process_document %s: read document: %w", in.AssetID, err)
	}

	// 2. Parse via document-service. Unsupported/corrupt/encrypted documents are
	//    terminal (no retry); service-down/deadline are transient.
	res, err := p.Deps.Client.Parse(ctx, bytes.NewReader(data), documents.Options{
		Filename:    in.OriginalName,
		ContentType: in.MimeType,
	})
	if err != nil {
		if isTerminalParseErr(err) {
			slog.WarnContext(ctx, "unparseable document", logging.AttrComponent, "jobs.process_document", "asset_id", in.AssetID, logging.AttrError, err)
			return p.setStatus(ctx, in.AssetID, models.AssetStatusFailed)
		}
		return fmt.Errorf("process_document %s: parse: %w", in.AssetID, err)
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
		tokens := ch.TokenCount
		if tokens <= 0 {
			tokens = estimateTokens(ch.Text)
		}
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
		if ch.SourceLabel != "" {
			label := ch.SourceLabel
			chunk.SourceLabel = &label
		}
		chunk.SourceAnchor, chunk.PageStart, chunk.PageEnd = documentAnchor(ch.Anchor)
		chunks = append(chunks, chunk)
	}

	if len(chunks) > 0 && p.Deps.Chunks != nil {
		if err := p.Deps.Chunks.UpsertChunks(ctx, in.AssetID, chunks); err != nil {
			return fmt.Errorf("process_document %s: store chunks: %w", in.AssetID, err)
		}
	}

	// CON-86: one usage event per document ingest (sum of embedded-chunk token
	// estimates; the Gemini embed response carries no usage). Nil recorder = no-op.
	p.Deps.Recorder.RecordResp(ctx, llm.VendorGemini, p.Deps.EmbedModel, "document_extract", llm.EmbedUsage{Tokens: totalEmbedTokens})

	// 4. File metadata — retried on failure so the asset never lands "ready"
	//    without its file row.
	if err := p.persistFile(ctx, in, key, len(data)); err != nil {
		return err
	}

	// 5. Final status. Propagate a write failure so the worker retries rather
	//    than reporting success with the asset stuck in "processing".
	switch {
	case embedAttempts == 0:
		// No embeddable text (e.g. image-only or empty document) — ready with 0
		// chunks (searchable-but-empty; not an error).
		return p.setStatus(ctx, in.AssetID, models.AssetStatusReady)
	case len(chunks) == 0:
		// Every chunk failed to embed — almost always a transient embedder
		// outage. Retry; give up (failed) only once attempts are exhausted, so
		// the asset never stays stuck in "processing".
		if lastAttempt {
			return p.setStatus(ctx, in.AssetID, models.AssetStatusFailed)
		}
		return fmt.Errorf("process_document %s: all %d chunk(s) failed to embed", in.AssetID, embedAttempts)
	case embedFailures > 0:
		return p.setStatus(ctx, in.AssetID, models.AssetStatusPartial)
	default:
		return p.setStatus(ctx, in.AssetID, models.AssetStatusReady)
	}
}

// setStatus persists the asset status, returning the error so callers can fail
// the job rather than reporting success with an unpersisted status. A nil Assets
// dep (status updates disabled) is a no-op.
func (p *ProcessDocumentProcessor) setStatus(ctx context.Context, assetID, status string) error {
	if p.Deps.Assets == nil {
		return nil
	}
	if err := p.Deps.Assets.UpdateStatus(ctx, assetID, status); err != nil {
		return fmt.Errorf("process_document %s: set status %s: %w", assetID, status, err)
	}
	// CON-242: announce terminal outcomes to the asset's creator (no-op for the
	// intermediate "processing" write).
	notifyAssetStatus(ctx, p.Deps.Notifier, p.Deps.Assets, assetID, status, "document")
	return nil
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
		if a.PageStart > 0 {
			ps := a.PageStart
			pageStart = &ps
		}
		if a.PageEnd > 0 {
			pe := a.PageEnd
			pageEnd = &pe
		}
	}
	return anchor, pageStart, pageEnd
}
