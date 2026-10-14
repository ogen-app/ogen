package queues

import (
	"context"
	"errors"
	"fmt"
	"io"
	"iter"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/pgvector/pgvector-go"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/genkit/embedbatch"
	"github.com/ogen-app/ogen/src/genkit/embedopts"
	"github.com/ogen-app/ogen/src/infra/embedding"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/infra/storage"
	"github.com/ogen-app/ogen/src/transport/grpc/client/remote"
)

// Ingestion as steps. An ingest's outside calls (a sidecar parse, Gemini
// embeds, a Firecrawl scrape) are planned here and made by river-service, or
// in-process through each action's local closure when ogen runs the job. Its
// parsed chunks are staged in the database between steps (chunkStage), so a
// long document never rides in the job's state.

// Sidecar service names, as river-service's sidecar addresses know them.
const (
	sidecarPDF       = "pdf"
	sidecarDocuments = "documents"
	sidecarImage     = "image"
	sidecarAudio     = "audio"
)

// sourceURLTTL is how long the signed link of a stored file handed to a
// sidecar or transfer stays valid.
const sourceURLTTL = time.Hour

// presigner hands out signed download links. The production storage
// implements it; a store without it can't serve river-service, only a run
// inside ogen.
type presigner interface {
	PresignedGetURL(ctx context.Context, key string, ttl time.Duration) (string, error)
}

// sourceURL is a signed link to a stored file, or "" when the store can't sign
// one.
func sourceURL(ctx context.Context, store any, key string) (string, error) {
	ps, ok := store.(presigner)
	if !ok {
		return "", nil
	}
	return ps.PresignedGetURL(ctx, key, sourceURLTTL)
}

// header reads a stored object's metadata. The production storage implements
// it.
type header interface {
	Head(ctx context.Context, key string) (*storage.ObjectInfo, error)
}

// storedSize is a stored file's size: from its metadata when the store can
// read it, otherwise by reading the file once.
func storedSize(ctx context.Context, store blobStore, key string) (int64, error) {
	if h, ok := store.(header); ok {
		info, err := h.Head(ctx, key)
		if err != nil {
			return 0, fmt.Errorf("stat %s: %w", key, err)
		}
		return info.Size, nil
	}
	rc, err := store.Download(ctx, key)
	if err != nil {
		return 0, fmt.Errorf("download %s: %w", key, err)
	}
	defer func() { _ = rc.Close() }()
	n, err := io.Copy(io.Discard, rc)
	if err != nil {
		return 0, fmt.Errorf("read %s: %w", key, err)
	}
	return n, nil
}

// sidecarStep plans a sidecar RPC. op runs one typed client method over the
// connection it is given (for a streaming upload, with an empty file);
// stream, for a client-streaming method, names the stored file river-service
// streams after the first message. local makes the call in-process.
func sidecarStep(state any, service string, timeout time.Duration, stream *StreamSource, op func(grpc.ClientConnInterface) error, local localAction) (StepOutput, error) {
	call, err := remote.Plan(op)
	if err != nil {
		return StepOutput{}, err
	}
	raw, err := encodeState(state)
	if err != nil {
		return StepOutput{}, err
	}
	sc := &SidecarCall{Service: service, Method: call.Method, Timeout: timeout, local: local}
	if call.Streamed {
		if stream == nil {
			return StepOutput{}, fmt.Errorf("sidecar %s streams a file, but none was named", call.Method)
		}
		s := *stream
		s.Header = call.Request
		sc.Stream = &s
	} else {
		sc.Request = call.Request
	}
	return StepOutput{State: raw, Sidecar: sc}, nil
}

// settledOr ends the job once an outcome is recorded, or retries it when
// recording failed.
func settledOr(err error) (StepOutput, error) {
	if err != nil {
		return retry("ingest", err)
	}
	return done("settled")
}

// sidecarResult reads river-service's answer to a sidecar call through op.
func sidecarResult(r *StepResult, op func(grpc.ClientConnInterface) error) error {
	return remote.Decode(remote.Result{Code: r.Status, Message: r.Err, Body: r.Body}, op)
}

// chunkField is the field number of the bytes field name in msg, which a
// streamed upload's frames after the first carry.
func chunkField(msg proto.Message, name protoreflect.Name) int32 {
	f := msg.ProtoReflect().Descriptor().Fields().ByName(name)
	if f == nil {
		return 0
	}
	return int32(f.Number())
}

// chunkStage stages an ingest run's chunks while they embed.
// *repository.assetChunkStagesRepository implements it; memChunkStage stands
// in when none is wired.
type chunkStage interface {
	Begin(ctx context.Context, runID, assetID string, stages []models.AssetChunkStage) error
	Pending(ctx context.Context, runID string, limit int) ([]models.AssetChunkStage, error)
	Rows(ctx context.Context, runID string, idx []int) ([]models.AssetChunkStage, error)
	SetEmbeddings(ctx context.Context, runID, model string, vecs map[int][]float32) error
	MarkFailed(ctx context.Context, runID string, idx []int) error
	Commit(ctx context.Context, runID, assetID string, clearStale bool) (repository.ChunkStageStats, error)
}

// stageSources turns the sources with words into stage rows.
func stageSources(sources iter.Seq[chunkSource]) []models.AssetChunkStage {
	var out []models.AssetChunkStage
	for src := range sources {
		if !hasWords(src.Text) {
			continue
		}
		tokens := src.Tokens
		if tokens <= 0 {
			tokens = estimateTokens(src.Text)
		}
		st := models.AssetChunkStage{
			ChunkIndex: src.Index, Content: src.Text, TokenCount: tokens,
			PageStart: src.PageStart, PageEnd: src.PageEnd, SourceAnchor: src.Anchor,
		}
		if src.Label != "" {
			st.SourceLabel = new(src.Label)
		}
		out = append(out, st)
	}
	return out
}

// embedProgress is where a run's embedding stands: the batch in flight, and a
// failed batch's texts retried one at a time so one bad text can't fail its
// neighbours.
type embedProgress struct {
	RunID  string `json:"run_id"`
	Batch  []int  `json:"batch,omitempty"`
	Single []int  `json:"single,omitempty"`
}

// embedLoop embeds a run's staged chunks, a batch per step.
type embedLoop struct {
	stage    chunkStage
	embedder chunkEmbedder
}

// next plans the embed of the next texts, or returns nil once every staged
// text has a vector or has failed.
func (l embedLoop) next(ctx context.Context, p *embedProgress) (*HTTPCall, error) {
	var rows []models.AssetChunkStage
	var err error
	if len(p.Single) > 0 {
		rows, err = l.stage.Rows(ctx, p.RunID, p.Single[:1])
	} else {
		rows, err = l.stage.Pending(ctx, p.RunID, embedbatch.Size)
	}
	if err != nil {
		return nil, fmt.Errorf("load staged chunks: %w", err)
	}
	if len(rows) == 0 {
		return nil, nil
	}
	p.Batch = make([]int, len(rows))
	texts := make([]string, len(rows))
	for i, r := range rows {
		p.Batch[i], texts[i] = r.ChunkIndex, r.Content
	}
	req, err := embedding.PlanBatch(l.embedder.Name(), texts, embedopts.Document())
	if err != nil {
		return nil, err
	}
	embedder := l.embedder
	return &HTTPCall{
		Upstream: UpstreamGemini, Method: http.MethodPost, Path: req.Path, Body: req.Body,
		local: func(ctx context.Context) (any, error) {
			return embedbatch.Batch(ctx, embedder, texts, embedopts.Document())
		},
	}, nil
}

// settle stores the vectors of the batch in flight. A failed batch of several
// texts is retried one text at a time; a single text that fails is left out.
func (l embedLoop) settle(ctx context.Context, p *embedProgress, r *StepResult) error {
	batch := p.Batch
	p.Batch = nil
	if len(batch) == 0 {
		return nil
	}
	single := len(p.Single) > 0
	vecs, err := localOr(r, func(r *StepResult) ([][]float32, error) {
		if r.Err != "" {
			return nil, errors.New(r.Err)
		}
		return embedding.DecodeBatch(r.Status, r.Body, len(batch))
	})
	if err == nil && len(vecs) != len(batch) {
		err = fmt.Errorf("embed: got %d embeddings for %d texts", len(vecs), len(batch))
	}
	switch {
	case err == nil:
		byIndex := make(map[int][]float32, len(batch))
		for i, idx := range batch {
			byIndex[idx] = vecs[i]
		}
		if serr := l.stage.SetEmbeddings(ctx, p.RunID, l.embedder.Name(), byIndex); serr != nil {
			return fmt.Errorf("store embeddings: %w", serr)
		}
	case len(batch) > 1 && !single:
		p.Single = batch
		return nil
	default:
		if merr := l.stage.MarkFailed(ctx, p.RunID, batch); merr != nil {
			return fmt.Errorf("mark failed embeddings: %w", merr)
		}
	}
	if single {
		p.Single = p.Single[1:]
	}
	return nil
}

// stats converts a committed run's tally.
func stageStats(s repository.ChunkStageStats) embedStats {
	return embedStats{Attempts: s.Attempts, Embedded: s.Embedded, Failures: s.Failures, Tokens: s.Tokens}
}

// memChunkStage keeps runs in memory and commits through a chunk store. It
// stands in for the stage table when none is wired (tests): a run inside one
// process only.
type memChunkStage struct {
	mu     sync.Mutex
	runs   map[string][]models.AssetChunkStage
	chunks chunkUpserter
}

func newMemChunkStage(chunks chunkUpserter) *memChunkStage {
	return &memChunkStage{runs: map[string][]models.AssetChunkStage{}, chunks: chunks}
}

func (m *memChunkStage) Begin(_ context.Context, runID, assetID string, stages []models.AssetChunkStage) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	rows := slices.Clone(stages)
	for i := range rows {
		rows[i].RunID, rows[i].AssetID = runID, assetID
	}
	m.runs[runID] = rows
	return nil
}

func (m *memChunkStage) Pending(_ context.Context, runID string, limit int) ([]models.AssetChunkStage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []models.AssetChunkStage
	for _, r := range m.runs[runID] {
		if r.Embedding == nil && !r.Failed && len(out) < limit {
			out = append(out, r)
		}
	}
	return out, nil
}

func (m *memChunkStage) Rows(_ context.Context, runID string, idx []int) ([]models.AssetChunkStage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []models.AssetChunkStage
	for _, r := range m.runs[runID] {
		if slices.Contains(idx, r.ChunkIndex) {
			out = append(out, r)
		}
	}
	return out, nil
}

func (m *memChunkStage) SetEmbeddings(_ context.Context, runID, model string, vecs map[int][]float32) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	rows := m.runs[runID]
	for i := range rows {
		if v, ok := vecs[rows[i].ChunkIndex]; ok {
			hv := pgvector.NewHalfVector(v)
			rows[i].Embedding, rows[i].Model = &hv, model
		}
	}
	return nil
}

func (m *memChunkStage) MarkFailed(_ context.Context, runID string, idx []int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	rows := m.runs[runID]
	for i := range rows {
		if slices.Contains(idx, rows[i].ChunkIndex) {
			rows[i].Failed = true
		}
	}
	return nil
}

func (m *memChunkStage) Commit(ctx context.Context, runID, assetID string, clearStale bool) (repository.ChunkStageStats, error) {
	m.mu.Lock()
	rows := m.runs[runID]
	delete(m.runs, runID)
	m.mu.Unlock()
	var st repository.ChunkStageStats
	var chunks []models.AssetChunk
	for _, r := range rows {
		st.Attempts++
		if r.Failed {
			st.Failures++
		}
		if r.Embedding == nil {
			continue
		}
		st.Embedded++
		st.Tokens += int64(r.TokenCount)
		chunks = append(chunks, models.AssetChunk{
			ID: fmt.Sprintf("%s:%d", assetID, r.ChunkIndex), AssetID: assetID, ChunkIndex: r.ChunkIndex,
			Content: r.Content, TokenCount: r.TokenCount, Embedding: *r.Embedding, Model: r.Model,
			SourceLabel: r.SourceLabel, SourceAnchor: r.SourceAnchor, PageStart: r.PageStart, PageEnd: r.PageEnd,
		})
	}
	stats := embedStats{Attempts: st.Attempts, Embedded: st.Embedded, Failures: st.Failures, Tokens: st.Tokens}
	return st, storeChunks(ctx, m.chunks, "stage", assetID, chunks, stats, clearStale)
}
