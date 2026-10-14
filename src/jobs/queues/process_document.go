package queues

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"log/slog"
	"sync"
	"time"

	"github.com/riverqueue/river"
	"google.golang.org/grpc"

	documentsv1 "github.com/ogen-app/ogen/gen/documents/v1"
	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/storage"
	"github.com/ogen-app/ogen/src/infra/vendors/llm"
	"github.com/ogen-app/ogen/src/kernel/logging"
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
	// Analysis queues the asset's analyses once it settles; nil skips them.
	Analysis AnalysisScheduler
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
	// Stages holds the parsed chunks while they embed. Nil keeps them in
	// memory, which serves only a run inside one process.
	Stages chunkStage
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
	return river.InsertOpts{Queue: IngestQueue, MaxAttempts: 5}
}

type ProcessDocumentProcessor struct {
	river.WorkerDefaults[ProcessDocumentTask]
	Deps DocumentDeps

	memOnce sync.Once
	mem     *memChunkStage
}

func init() {
	register(func(w *river.Workers, d Deps) {
		river.AddWorker(w, &ProcessDocumentProcessor{Deps: d.Document})
	})
}

// Work runs the job's steps in-process.
func (p *ProcessDocumentProcessor) Work(ctx context.Context, job *river.Job[ProcessDocumentTask]) error {
	ctx = WithJobRequestID(ctx, job.JobRow)
	return runLocal(ctx, p.step, job.Kind, job.Args, job.Attempt, job.MaxAttempts, localActions{})
}

// Timeout covers a worst-case extraction (large workbook exploding into many
// row chunks) plus per-chunk embedding.
func (p *ProcessDocumentProcessor) Timeout(*river.Job[ProcessDocumentTask]) time.Duration {
	return 16 * time.Minute
}

// process runs one attempt in-process. lastAttempt flips an otherwise
// retryable embedder outage to a terminal "failed" instead of abandoning the
// asset in "processing".
func (p *ProcessDocumentProcessor) process(ctx context.Context, in ProcessDocumentTask, lastAttempt bool) error {
	attempt, maxAttempts := 1, 2
	if lastAttempt {
		attempt = 2
	}
	return runLocal(ctx, p.step, ProcessDocumentQueue, in, attempt, maxAttempts, localActions{})
}

func (p *ProcessDocumentProcessor) stages() chunkStage {
	if p.Deps.Stages != nil {
		return p.Deps.Stages
	}
	p.memOnce.Do(func() { p.mem = newMemChunkStage(p.Deps.Chunks) })
	return p.mem
}

func (p *ProcessDocumentProcessor) embedLoop() embedLoop {
	return embedLoop{stage: p.stages(), embedder: p.Deps.Embedder}
}

// documentState carries a run between steps.
type documentState struct {
	Phase string        `json:"phase"`
	Embed embedProgress `json:"embed"`
	Size  int64         `json:"size,omitempty"`
}

const (
	documentPhaseParse = "parse"
	documentPhaseEmbed = "embed"
)

func documentOptions(in ProcessDocumentTask) documents.Options {
	return documents.Options{Filename: in.OriginalName, ContentType: in.MimeType}
}

// step parses the document (document-service), stages its chunks and embeds
// them a batch at a time, then persists the file row and settles the asset.
func (p *ProcessDocumentProcessor) step(ctx context.Context, in StepInput) (StepOutput, error) {
	var task ProcessDocumentTask
	if err := json.Unmarshal(in.Args, &task); err != nil {
		return done("skipped_bad_args")
	}
	ctx = withTaskTenant(ctx, task.TenantID)
	var st documentState
	if err := decodeState(in.State, &st); err != nil {
		return done("skipped_bad_state")
	}
	switch st.Phase {
	case "":
		return p.start(ctx, task, in.lastAttempt())
	case documentPhaseParse:
		return p.parsed(ctx, task, in.Result)
	case documentPhaseEmbed:
		if err := p.embedLoop().settle(ctx, &st.Embed, in.Result); err != nil {
			return retry("process_document "+task.AssetID, err)
		}
		return p.embedNext(ctx, task, &st, in.lastAttempt())
	default:
		return done("skipped_bad_state")
	}
}

func (p *ProcessDocumentProcessor) start(ctx context.Context, in ProcessDocumentTask, lastAttempt bool) (StepOutput, error) {
	if p.Deps.Client == nil {
		slog.WarnContext(ctx, "document-service not configured", logging.AttrComponent, "jobs.process_document", "asset_id", in.AssetID)
		return done("skipped_unconfigured")
	}
	status := p.statusWriter()
	if p.Deps.Storage == nil {
		// Best-effort status write; the returned error is more descriptive.
		_ = status.fail(ctx, in.AssetID, models.UploadCodeInternalError, "document processing is not configured")
		return retry("process_document "+in.AssetID, errors.New("storage not configured"))
	}
	giveUp := func() error {
		return status.fail(ctx, in.AssetID, models.UploadCodeServiceUnavailable, documentUnavailableReason)
	}
	if ok, err := requireEmbedder(ctx, p.Deps.Embedder, "process_document", in.AssetID, lastAttempt, giveUp); !ok {
		return settledOr(err)
	}
	if err := status.set(ctx, in.AssetID, models.AssetStatusProcessing); err != nil {
		return retry("process_document", err)
	}

	key := storage.TenantKey(ctx, in.StorageKey)
	src, err := sourceURL(ctx, p.Deps.Storage, key)
	if err != nil {
		return retry("process_document "+in.AssetID+": sign original", err)
	}
	opts := documentOptions(in)
	return sidecarStep(documentState{Phase: documentPhaseParse}, sidecarDocuments, 0,
		&StreamSource{SourceURL: src, ChunkField: chunkField(&documentsv1.ParseRequest{}, "chunk")},
		func(cc grpc.ClientConnInterface) error {
			_, err := documents.Over(cc).Parse(ctx, bytes.NewReader(nil), opts)
			return err
		},
		func(ctx context.Context) (any, error) {
			original, err := openOriginal(ctx, p.Deps.Storage, "process_document", in.AssetID, key)
			if err != nil {
				return nil, err
			}
			defer func() { _ = original.Close() }()
			return p.Deps.Client.Parse(ctx, original, opts)
		})
}

// parsed settles the parse: an unsupported, corrupt or encrypted document
// fails the asset, a service outage retries. Otherwise the chunks are staged
// and embedding starts.
func (p *ProcessDocumentProcessor) parsed(ctx context.Context, in ProcessDocumentTask, r *StepResult) (StepOutput, error) {
	opts := documentOptions(in)
	res, err := localOr(r, func(r *StepResult) (*documents.Result, error) {
		var res *documents.Result
		err := sidecarResult(r, func(cc grpc.ClientConnInterface) error {
			var err error
			res, err = documents.Over(cc).Parse(ctx, bytes.NewReader(nil), opts)
			return err
		})
		return res, err
	})
	if err != nil {
		if isTerminalParseErr(err) {
			slog.WarnContext(ctx, "unparseable document", logging.AttrComponent, "jobs.process_document", "asset_id", in.AssetID, logging.AttrError, err)
			return settledOr(p.statusWriter().fail(ctx, in.AssetID, models.UploadCodeInvalidFile, "the document could not be read (corrupt, encrypted, or unsupported)"))
		}
		return retry("process_document "+in.AssetID+": parse", err)
	}
	runID, err := models.NewID()
	if err != nil {
		return StepOutput{}, err
	}
	if err := p.stages().Begin(ctx, runID, in.AssetID, stageSources(documentChunkSources(res.Chunks))); err != nil {
		return retry("process_document "+in.AssetID+": stage chunks", err)
	}
	size, err := storedSize(ctx, p.Deps.Storage, storage.TenantKey(ctx, in.StorageKey))
	if err != nil {
		return retry("process_document "+in.AssetID, err)
	}
	st := &documentState{Phase: documentPhaseEmbed, Embed: embedProgress{RunID: runID}, Size: size}
	return p.embedNext(ctx, in, st, false)
}

// embedNext embeds the next batch, or, once every chunk is settled, commits
// the chunks, persists the file row and settles the asset.
func (p *ProcessDocumentProcessor) embedNext(ctx context.Context, in ProcessDocumentTask, st *documentState, lastAttempt bool) (StepOutput, error) {
	call, err := p.embedLoop().next(ctx, &st.Embed)
	if err != nil {
		return retry("process_document "+in.AssetID, err)
	}
	if call != nil {
		raw, err := encodeState(st)
		if err != nil {
			return StepOutput{}, err
		}
		return StepOutput{State: raw, HTTP: call}, nil
	}
	committed, err := p.stages().Commit(ctx, st.Embed.RunID, in.AssetID, false)
	if err != nil {
		return retry("process_document "+in.AssetID+": store chunks", err)
	}
	stats := stageStats(committed)
	// The file row is retried so the asset never lands "ready" without it.
	if err := p.persistFile(ctx, in, storage.TenantKey(ctx, in.StorageKey), st.Size); err != nil {
		return retry("process_document", err)
	}
	status := p.statusWriter()
	final, err := stats.settle(lastAttempt)
	if err != nil {
		return retry("process_document "+in.AssetID, err)
	}
	if final == models.AssetStatusFailed {
		return settledOr(status.fail(ctx, in.AssetID, models.UploadCodeServiceUnavailable, documentUnavailableReason))
	}
	if err := status.set(ctx, in.AssetID, final); err != nil {
		return retry("process_document", err)
	}
	// One usage event per ingest, recorded only after the durable writes so a
	// retry from a late failure can't double-count. The Gemini embed response
	// carries no usage, so the tokens are the chunks' counts.
	if stats.Tokens > 0 {
		p.Deps.Recorder.RecordResp(ctx, llm.VendorGemini, p.Deps.EmbedModel, "document_extract", llm.EmbedUsage{Tokens: stats.Tokens})
	}
	return done(final)
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
		analysis: p.Deps.Analysis,
	}
}

// persistFile upserts the asset_file row (s3 key, mime, size). The Upsert
// conflicts on asset_id, so it is idempotent across retries; the error is
// returned so a write failure retries the job rather than leaving the asset
// "ready" without its file row. No thumbnail / page count for documents in v1.
// A nil Files dep is a no-op.
func (p *ProcessDocumentProcessor) persistFile(ctx context.Context, in ProcessDocumentTask, s3Key string, size int64) error {
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
		SizeBytes:    size,
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
