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

	"github.com/firebase/genkit/go/ai"
	"github.com/riverqueue/river"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	pdfv1 "github.com/ogen-app/ogen/gen/pdf/v1"
	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/storage"
	"github.com/ogen-app/ogen/src/infra/vendors/llm"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/kernel/pubdate"
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

// pdfMetaWindow is how much of each end of a PDF is searched for its creation
// date.
const pdfMetaWindow = 256 << 10

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
	// Analysis queues the asset's analyses once it settles; nil skips them.
	Analysis AnalysisScheduler
	Client   pdfParser
	Embedder chunkEmbedder
	Storage  blobStore
	Assets   assetStatusUpdater
	Chunks   chunkUpserter
	Files    fileUpserter
	// Published records the PDF's creation date; nil skips it.
	Published    publishedAtSetter
	ThumbnailDPI int
	// Recorder + EmbedModel meter PDF-ingestion embedding usage. nil
	// Recorder = no-op. EmbedModel is the price-map key (cfg.EmbedModel).
	Recorder   *usage.Recorder
	EmbedModel string
	// Notifier drops an in-app notification to the asset's creator when ingest
	// reaches a terminal status. Nil is a no-op.
	Notifier *notify.Service
	// Stages holds the parsed chunks while they embed. Nil keeps them in
	// memory, which serves only a run inside one process.
	Stages chunkStage
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
	return river.InsertOpts{Queue: IngestQueue, MaxAttempts: 5}
}

type ProcessPDFProcessor struct {
	river.WorkerDefaults[ProcessPDFTask]
	Deps PDFDeps

	memOnce sync.Once
	mem     *memChunkStage
}

func init() {
	register(func(w *river.Workers, d Deps) {
		river.AddWorker(w, &ProcessPDFProcessor{Deps: d.PDF})
	})
}

// Work runs the job's steps in-process.
func (p *ProcessPDFProcessor) Work(ctx context.Context, job *river.Job[ProcessPDFTask]) error {
	ctx = WithJobRequestID(ctx, job.JobRow)
	return runLocal(ctx, p.step, job.Kind, job.Args, job.Attempt, job.MaxAttempts, localActions{})
}

// Timeout covers a worst-case parse (large PDF) plus per-chunk embedding.
func (p *ProcessPDFProcessor) Timeout(*river.Job[ProcessPDFTask]) time.Duration {
	return 16 * time.Minute
}

// process runs one attempt in-process. lastAttempt flips an otherwise
// retryable embedder outage to a terminal "failed" instead of abandoning the
// asset in "processing".
func (p *ProcessPDFProcessor) process(ctx context.Context, in ProcessPDFTask, lastAttempt bool) error {
	attempt, maxAttempts := 1, 2
	if lastAttempt {
		attempt = 2
	}
	return runLocal(ctx, p.step, ProcessPDFQueue, in, attempt, maxAttempts, localActions{})
}

func (p *ProcessPDFProcessor) stages() chunkStage {
	if p.Deps.Stages != nil {
		return p.Deps.Stages
	}
	p.memOnce.Do(func() { p.mem = newMemChunkStage(p.Deps.Chunks) })
	return p.mem
}

// pdfState carries a run between steps: what the parse left for the file row,
// and how far embedding got.
type pdfState struct {
	Phase     string        `json:"phase"`
	Embed     embedProgress `json:"embed"`
	ThumbKey  *string       `json:"thumb_key,omitempty"`
	Size      int           `json:"size,omitempty"`
	PageCount int           `json:"page_count,omitempty"`
}

const (
	pdfPhaseParse = "parse"
	pdfPhaseEmbed = "embed"
)

// step parses the PDF (pdf-service), stages its chunks and embeds them a
// batch at a time, then persists the file row and settles the asset.
func (p *ProcessPDFProcessor) step(ctx context.Context, in StepInput) (StepOutput, error) {
	var task ProcessPDFTask
	if err := json.Unmarshal(in.Args, &task); err != nil {
		return done("skipped_bad_args")
	}
	ctx = withTaskTenant(ctx, task.TenantID)
	var st pdfState
	if err := decodeState(in.State, &st); err != nil {
		return done("skipped_bad_state")
	}
	switch st.Phase {
	case "":
		return p.start(ctx, task, in.lastAttempt())
	case pdfPhaseParse:
		return p.parsed(ctx, task, &st, in.Result)
	case pdfPhaseEmbed:
		if err := p.embedLoop().settle(ctx, &st.Embed, in.Result); err != nil {
			return retry("process_pdf "+task.AssetID, err)
		}
		return p.embedNext(ctx, task, &st, in.lastAttempt())
	default:
		return done("skipped_bad_state")
	}
}

func (p *ProcessPDFProcessor) embedLoop() embedLoop {
	return embedLoop{stage: p.stages(), embedder: p.Deps.Embedder}
}

func (p *ProcessPDFProcessor) originalKey(ctx context.Context, assetID string) string {
	return storage.TenantKey(ctx, fmt.Sprintf("assets/%s/original.pdf", assetID))
}

func (p *ProcessPDFProcessor) parseOptions(in ProcessPDFTask) pdf.Options {
	return pdf.Options{Filename: in.OriginalName, RenderThumbnail: true, ThumbnailDPI: p.thumbnailDPI()}
}

// start checks the job can run, then has pdf-service parse the stored
// original.
func (p *ProcessPDFProcessor) start(ctx context.Context, in ProcessPDFTask, lastAttempt bool) (StepOutput, error) {
	if p.Deps.Client == nil {
		slog.WarnContext(ctx, "pdf-service not configured", logging.AttrComponent, "jobs.process_pdf", "asset_id", in.AssetID)
		return done("skipped_unconfigured")
	}
	status := p.statusWriter()
	if p.Deps.Storage == nil {
		// Best-effort status write; the returned error is more descriptive.
		_ = status.fail(ctx, in.AssetID, models.UploadCodeInternalError, status.notConfiguredReason())
		return retry("process_pdf "+in.AssetID, errors.New("storage not configured"))
	}
	giveUp := func() error {
		return status.fail(ctx, in.AssetID, models.UploadCodeServiceUnavailable, status.unavailableReason())
	}
	if ok, err := requireEmbedder(ctx, p.Deps.Embedder, "process_pdf", in.AssetID, lastAttempt, giveUp); !ok {
		return settledOr(err)
	}
	if err := status.set(ctx, in.AssetID, models.AssetStatusProcessing); err != nil {
		return retry("process_pdf", err)
	}

	key := p.originalKey(ctx, in.AssetID)
	src, err := sourceURL(ctx, p.Deps.Storage, key)
	if err != nil {
		return retry("process_pdf "+in.AssetID+": sign original", err)
	}
	opts := p.parseOptions(in)
	return sidecarStep(pdfState{Phase: pdfPhaseParse}, sidecarPDF, 0,
		&StreamSource{SourceURL: src, ChunkField: chunkField(&pdfv1.ParseRequest{}, "chunk")},
		func(cc grpc.ClientConnInterface) error {
			_, err := pdf.Over(cc).Parse(ctx, bytes.NewReader(nil), opts)
			return err
		},
		func(ctx context.Context) (any, error) {
			original, err := openOriginal(ctx, p.Deps.Storage, "process_pdf", in.AssetID, key)
			if err != nil {
				return nil, err
			}
			defer func() { _ = original.Close() }()
			return p.Deps.Client.Parse(ctx, original, opts)
		})
}

// parsed settles the parse: a corrupt or unsupported PDF fails the asset, a
// service outage retries. Otherwise the chunks are staged, the thumbnail
// stored and the original's creation date and size read, then embedding
// starts.
func (p *ProcessPDFProcessor) parsed(ctx context.Context, in ProcessPDFTask, st *pdfState, r *StepResult) (StepOutput, error) {
	opts := p.parseOptions(in)
	res, err := localOr(r, func(r *StepResult) (*pdf.Result, error) {
		var res *pdf.Result
		err := sidecarResult(r, func(cc grpc.ClientConnInterface) error {
			var err error
			res, err = pdf.Over(cc).Parse(ctx, bytes.NewReader(nil), opts)
			return err
		})
		return res, err
	})
	status := p.statusWriter()
	if err != nil {
		if isTerminalParseErr(err) {
			slog.WarnContext(ctx, "unparseable pdf", logging.AttrComponent, "jobs.process_pdf", "asset_id", in.AssetID, logging.AttrError, err)
			return settledOr(status.fail(ctx, in.AssetID, models.UploadCodeInvalidFile, "the PDF could not be read (corrupt, encrypted, or unsupported)"))
		}
		return retry("process_pdf "+in.AssetID+": parse", err)
	}

	runID, err := models.NewID()
	if err != nil {
		return StepOutput{}, err
	}
	if err := p.stages().Begin(ctx, runID, in.AssetID, stageSources(pdfChunkSources(res.Chunks))); err != nil {
		return retry("process_pdf "+in.AssetID+": stage chunks", err)
	}
	// The thumbnail is non-fatal.
	st.ThumbKey = p.uploadThumbnail(ctx, in.AssetID, res.ThumbnailPNG)
	st.PageCount = res.PageCount
	size, err := p.sniffOriginal(ctx, in)
	if err != nil {
		return retry("process_pdf "+in.AssetID, err)
	}
	st.Size = size
	st.Phase, st.Embed = pdfPhaseEmbed, embedProgress{RunID: runID}
	return p.embedNext(ctx, in, st, false)
}

// sniffOriginal reads the stored original once for its size and the creation
// date that sits near either end of the file.
func (p *ProcessPDFProcessor) sniffOriginal(ctx context.Context, in ProcessPDFTask) (int, error) {
	original, err := openOriginal(ctx, p.Deps.Storage, "process_pdf", in.AssetID, p.originalKey(ctx, in.AssetID))
	if err != nil {
		return 0, err
	}
	defer func() { _ = original.Close() }()
	sniff := pubdate.NewSniffer(pdfMetaWindow)
	if _, err := io.Copy(sniff, original); err != nil {
		return 0, fmt.Errorf("process_pdf %s: read original: %w", in.AssetID, err)
	}
	if t, ok := sniff.PDF(time.Now()); ok {
		recordPublishedAt(ctx, p.Deps.Published, "process_pdf", in.AssetID, t)
	}
	return original.N(), nil
}

// embedNext embeds the next batch, or, once every chunk is settled, commits
// the chunks, persists the file row and settles the asset.
func (p *ProcessPDFProcessor) embedNext(ctx context.Context, in ProcessPDFTask, st *pdfState, lastAttempt bool) (StepOutput, error) {
	call, err := p.embedLoop().next(ctx, &st.Embed)
	if err != nil {
		return retry("process_pdf "+in.AssetID, err)
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
		return retry("process_pdf "+in.AssetID+": store chunks", err)
	}
	stats := stageStats(committed)

	// The file row is retried so the asset never lands "ready" without its
	// file row, page count or thumbnail.
	if err := p.persistFile(ctx, in, p.originalKey(ctx, in.AssetID), st.ThumbKey, st.Size, st.PageCount); err != nil {
		return retry("process_pdf", err)
	}
	status := p.statusWriter()
	final, err := stats.settle(lastAttempt)
	if err != nil {
		return retry("process_pdf "+in.AssetID, err)
	}
	if final == models.AssetStatusFailed {
		return settledOr(status.fail(ctx, in.AssetID, models.UploadCodeServiceUnavailable, status.unavailableReason()))
	}
	if err := status.set(ctx, in.AssetID, final); err != nil {
		return retry("process_pdf", err)
	}
	// One usage event per ingest, after the durable writes so a retry from a
	// late failure can't double-count. The Gemini embed response carries no
	// usage, so the tokens are the embedded chunks' estimates.
	if stats.Tokens > 0 {
		p.Deps.Recorder.RecordResp(ctx, llm.VendorGemini, p.Deps.EmbedModel, "pdf_extract", llm.EmbedUsage{Tokens: stats.Tokens})
	}
	return done(final)
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
	return assetStatusWriter{op: "process_pdf", assets: p.Deps.Assets, notifier: p.Deps.Notifier, label: "document", kind: models.AssetTypePDF, analysis: p.Deps.Analysis}
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
