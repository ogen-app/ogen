package queues

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
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
	"github.com/ogen-app/ogen/src/transport/grpc/client/audio"
	"github.com/ogen-app/ogen/src/usecase/notify"
)

// ProcessAudioKind is the River worker kind for audio ingestion (CON-282):
// download the original, probe + tier-gate, normalize once, segment, transcribe
// each segment via audio-service (Gemini multimodal), then — on full completion
// — assemble time-anchored transcript chunks, embed them, and store them in the
// shared assets_chunks. Unlike the one-shot pdf/document/url jobs, ogen owns a
// resumable state machine here (audio-service is stateless compute): each step
// checkpoints, so a crash resumes from the first incomplete segment.
const ProcessAudioKind = "process_audio"

// AudioQueue is the dedicated River queue audio jobs run on, isolated from the
// default queue so a backlog of hour-long assets can't starve short ingestion
// for other tenants (CON-282 §5, §13). Its MaxWorkers + job timeout are sized
// separately in server.go.
const AudioQueue = "audio"

const (
	// audioChunkTargetChars packs assembled transcript into ~token-budget chunks
	// on utterance boundaries (never splitting an utterance). ~4 chars/token, so
	// ≈1k tokens per chunk — in the same ballpark as pdf/document chunks.
	audioChunkTargetChars = 4000
	// audioMinChunkChars: a trailing chunk below this is merged into the previous
	// one rather than emitted standalone (CON-282 §8).
	audioMinChunkChars = 200
	// audioPresignTTL bounds the presigned GET/PUT URLs handed to audio-service.
	// Generous so a long per-segment transcription can't outlive its URL.
	audioPresignTTL = time.Hour
	// normalizedContentType is the MIME bound on the normalized.opus PUT.
	normalizedContentType = "audio/ogg"
	// defaultSegmentMaxMs / defaultSegmentOverlapMs back the segmentation when
	// the deps leave them zero (mirrors the config defaults).
	defaultSegmentMaxMs     int64 = 300_000 // 5 min
	defaultSegmentOverlapMs int64 = 5_000   // 5 s
	defaultAudioJobTimeout        = 3 * time.Hour
)

// audioTranscriber is the narrow slice of the audio gRPC client the worker
// needs; a small fake satisfies it in tests.
type audioTranscriber interface {
	Probe(ctx context.Context, opts audio.ProbeOptions) (*audio.ProbeResult, error)
	Normalize(ctx context.Context, opts audio.NormalizeOptions) (*audio.NormalizeResult, error)
	TranscribeSegment(ctx context.Context, opts audio.TranscribeSegmentOptions) (*audio.TranscribeSegmentResult, error)
}

// audioBlobStore is the storage surface audio ingestion needs: presigned GET
// (source + normalized reads) and PUT (normalized write) so bytes never traverse
// gRPC. s3Storage satisfies it.
type audioBlobStore interface {
	PresignedGetURL(ctx context.Context, key string, ttl time.Duration) (string, error)
	PresignedPutURL(ctx context.Context, key, contentType string, ttl time.Duration) (string, error)
}

type audioExtractionStore interface {
	Create(ctx context.Context, e *models.AudioExtraction) error
	GetByAssetAndRunKey(ctx context.Context, assetID, runKey string) (*models.AudioExtraction, error)
	Update(ctx context.Context, e *models.AudioExtraction) error
}

type audioSegmentStore interface {
	CreateMany(ctx context.Context, segments []models.AudioSegment) error
	ListByExtraction(ctx context.Context, extractionID string) ([]models.AudioSegment, error)
	Update(ctx context.Context, s *models.AudioSegment) error
}

type assetContentSetter interface {
	SetContent(ctx context.Context, id, content string) error
}

type utteranceStore interface {
	ReplaceForSegment(ctx context.Context, segmentID string, utterances []models.Utterance) error
	// ListByExtraction scopes to the current run's segments so a re-extraction
	// never assembles this run's transcript together with a prior run's leftover
	// utterances.
	ListByExtraction(ctx context.Context, extractionID string) ([]models.Utterance, error)
}

// AudioDeps bundles the process_audio worker's dependencies (built in
// server.go). A nil Client (no AUDIO_SERVICE_ADDR configured) disables the job —
// it no-ops.
type AudioDeps struct {
	Client   audioTranscriber
	Embedder chunkEmbedder
	Storage  audioBlobStore
	Assets   assetStatusUpdater
	// Content writes the assembled transcript onto asset.Content on completion
	// (CON-312), so previews and the assistant's asset-content tools see it. Nil
	// skips the write.
	Content     assetContentSetter
	Chunks      chunkUpserter
	Extractions audioExtractionStore
	Segments    audioSegmentStore
	Utterances  utteranceStore
	// Recorder meters transcription usage on the existing gemini vendor (CON-86);
	// Checker gates the run against the tenant's cost cap before spend. Both
	// nil-safe.
	Recorder *usage.Recorder
	Checker  *usage.Checker
	// EmbedModel is the price-map key for transcript-chunk embedding usage;
	// TranscribeModel is the Gemini multimodal model id (config, never compiled
	// in) passed through to audio-service and used as the metering model.
	EmbedModel      string
	TranscribeModel string
	// Segmentation + tier-gate knobs (config). Zero falls back to the defaults.
	SegmentMaxMs     int64
	SegmentOverlapMs int64
	MaxDurationMs    int64
	JobTimeout       time.Duration
	// Notifier drops an in-app notification to the asset's creator on a terminal
	// status (CON-242). Nil is a no-op.
	Notifier *notify.Service
}

// ProcessAudioTask carries the asset to ingest. The audio bytes are NOT in the
// args — the worker hands audio-service presigned URLs and re-reads state from
// the DB on each attempt. StorageKey is the tenant-relative object path the
// finalize handler wrote (e.g. "assets/<id>/original.mp3"). RunKey makes the
// run idempotent; PinnedModel optionally overrides the transcription model.
type ProcessAudioTask struct {
	AssetID      string `json:"asset_id"`
	TenantID     string `json:"tenant_id"`
	OriginalName string `json:"original_name"`
	MimeType     string `json:"mime_type"`
	StorageKey   string `json:"storage_key"`
	RunKey       string `json:"run_key"`
	PinnedModel  string `json:"pinned_model,omitempty"`
}

func (ProcessAudioTask) Kind() string { return ProcessAudioKind }

// InsertOpts routes the job to the dedicated audio queue and bounds retries.
// Transient failures (storage, gRPC Unavailable/DeadlineExceeded, normalization
// Internal) retry with backoff and resume from the first incomplete segment;
// terminal ones (unusable/silent audio, over-limit) short-circuit inside Work.
func (ProcessAudioTask) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: AudioQueue, MaxAttempts: 10}
}

type ProcessAudioProcessor struct {
	river.WorkerDefaults[ProcessAudioTask]
	Deps AudioDeps
}

func init() {
	register(func(w *river.Workers, d Deps) {
		river.AddWorker(w, &ProcessAudioProcessor{Deps: d.Audio})
	})
}

// Work scopes the job to the asset's tenant and drives the pipeline. lastAttempt
// flips an otherwise-retryable per-segment transient failure into a terminal
// segment failure (→ partial) instead of leaving the asset stuck mid-run.
func (p *ProcessAudioProcessor) Work(ctx context.Context, job *river.Job[ProcessAudioTask]) error {
	ctx = WithJobRequestID(ctx, job.JobRow)
	ctx = tenantctx.With(ctx, job.Args.TenantID)
	return p.process(ctx, job.Args, job.Attempt >= job.MaxAttempts)
}

// Timeout bounds a single attempt. Each attempt makes checkpointed progress
// (normalize, then per-segment), so a run longer than one timeout simply resumes
// on the next attempt.
func (p *ProcessAudioProcessor) Timeout(*river.Job[ProcessAudioTask]) time.Duration {
	if p.Deps.JobTimeout > 0 {
		return p.Deps.JobTimeout
	}
	return defaultAudioJobTimeout
}

func (p *ProcessAudioProcessor) process(ctx context.Context, in ProcessAudioTask, lastAttempt bool) error {
	if p.Deps.Client == nil {
		slog.WarnContext(ctx, "audio-service not configured", logging.AttrComponent, "jobs.process_audio", "asset_id", in.AssetID)
		return nil
	}
	if p.Deps.Storage == nil || p.Deps.Extractions == nil || p.Deps.Segments == nil || p.Deps.Utterances == nil {
		_ = p.setAssetStatus(ctx, in.AssetID, models.AssetStatusFailed)
		return fmt.Errorf("process_audio %s: storage/repos not configured", in.AssetID)
	}
	// Transcription AND embedding both need gemini_api_key (CON-104): checked up
	// front so we don't normalize + transcribe only to fail every chunk embed.
	// Retry rather than fail — a key set via the secrets API takes effect without
	// a restart; give up (failed) only once attempts are exhausted.
	if !embedopts.Available(p.Deps.Embedder) {
		if lastAttempt {
			return p.setAssetStatus(ctx, in.AssetID, models.AssetStatusFailed)
		}
		slog.WarnContext(ctx, "embedder unavailable will retry", logging.AttrComponent, "jobs.process_audio", "asset_id", in.AssetID)
		return fmt.Errorf("process_audio %s: embedder unavailable", in.AssetID)
	}

	ext, err := p.ensureExtraction(ctx, in)
	if err != nil {
		return err
	}
	if ext.Status == models.AudioExtractionStatusComplete {
		return nil // idempotent re-drive of a finished run
	}
	if err := p.setAssetStatus(ctx, in.AssetID, models.AssetStatusProcessing); err != nil {
		return err
	}

	// 1–2. Probe + tier gate + normalize, only until the normalized derivative
	//      exists. On resume this whole block is skipped.
	if ext.NormalizedS3Key == nil {
		if err := p.probeGateNormalize(ctx, in, ext); err != nil {
			return err
		}
		if ext.Status == models.AudioExtractionStatusFailed {
			return nil // terminal reject already persisted (asset failed)
		}
	}

	// 3. Segment: compute bounded windows once.
	segments, err := p.Deps.Segments.ListByExtraction(ctx, ext.ID)
	if err != nil {
		return fmt.Errorf("process_audio %s: list segments: %w", in.AssetID, err)
	}
	if len(segments) == 0 {
		segments, err = p.createSegments(ctx, in, ext)
		if err != nil {
			return err
		}
	}

	// 4. Transcribe each incomplete segment (checkpointed).
	model := p.transcribeModel(in)
	for i := range segments {
		seg := &segments[i]
		if seg.Status == models.AudioSegmentStatusDone || seg.Status == models.AudioSegmentStatusFailed {
			continue
		}
		if err := p.transcribeSegment(ctx, in, ext, seg, model, lastAttempt); err != nil {
			return err // transient → River resumes from this segment
		}
	}

	// 5. All segments terminal → assemble/embed on full success; else partial.
	return p.finalize(ctx, in, ext)
}

// ensureExtraction loads the (asset, run_key) extraction or creates a fresh
// pending one. Idempotent under a concurrent create via the unique index.
func (p *ProcessAudioProcessor) ensureExtraction(ctx context.Context, in ProcessAudioTask) (*models.AudioExtraction, error) {
	ext, err := p.Deps.Extractions.GetByAssetAndRunKey(ctx, in.AssetID, in.RunKey)
	if err == nil {
		return ext, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("process_audio %s: load extraction: %w", in.AssetID, err)
	}
	id, err := models.NewID()
	if err != nil {
		return nil, fmt.Errorf("process_audio %s: new extraction id: %w", in.AssetID, err)
	}
	ext = &models.AudioExtraction{
		ID:              id,
		AssetID:         in.AssetID,
		RunKey:          in.RunKey,
		Status:          models.AudioExtractionStatusPending,
		TranscribeModel: p.transcribeModel(in),
		EmbedModel:      p.Deps.EmbedModel,
	}
	if err := p.Deps.Extractions.Create(ctx, ext); err != nil {
		// Concurrent create (unique asset_id+run_key): reload the winner.
		if again, gErr := p.Deps.Extractions.GetByAssetAndRunKey(ctx, in.AssetID, in.RunKey); gErr == nil {
			return again, nil
		}
		return nil, fmt.Errorf("process_audio %s: create extraction: %w", in.AssetID, err)
	}
	return ext, nil
}

// probeGateNormalize runs Probe, enforces the max-duration + cost gates, then
// Normalize. On a terminal reject it marks the extraction + asset failed and
// leaves ext.Status == failed for the caller to short-circuit.
func (p *ProcessAudioProcessor) probeGateNormalize(ctx context.Context, in ProcessAudioTask, ext *models.AudioExtraction) error {
	originalKey := storage.TenantKey(ctx, in.StorageKey)
	srcURL, err := p.Deps.Storage.PresignedGetURL(ctx, originalKey, audioPresignTTL)
	if err != nil {
		return fmt.Errorf("process_audio %s: presign source: %w", in.AssetID, err)
	}
	probe, err := p.Deps.Client.Probe(ctx, audio.ProbeOptions{SourceURL: srcURL, Filename: in.OriginalName})
	if err != nil {
		if audio.IsInvalidAudio(err) || audio.IsUnsupportedAudio(err) {
			return p.terminalReject(ctx, in, ext, "the audio could not be read (corrupt or unsupported format)")
		}
		return fmt.Errorf("process_audio %s: probe: %w", in.AssetID, err)
	}
	if probe.Silent || probe.DurationMs <= 0 {
		return p.terminalReject(ctx, in, ext, "the audio is empty or silent throughout")
	}
	// Max-duration tier gate (before any transcode/transcribe spend).
	if p.Deps.MaxDurationMs > 0 && probe.DurationMs > p.Deps.MaxDurationMs {
		return p.terminalReject(ctx, in, ext, fmt.Sprintf("audio is %d min, over the %d min limit for your plan", probe.DurationMs/60000, p.Deps.MaxDurationMs/60000))
	}
	// Cost-cap gate (CON-86 usage.Checker). Nil checker = no gate.
	if p.Deps.Checker != nil {
		if err := p.Deps.Checker.Enforce(ctx); err != nil {
			return p.terminalReject(ctx, in, ext, "your usage limit has been reached — transcription was not started")
		}
	}

	ext.SourceDurationMs = probe.DurationMs
	ext.AudioSeconds = probe.DurationMs / 1000
	ext.Status = models.AudioExtractionStatusNormalizing
	if err := p.Deps.Extractions.Update(ctx, ext); err != nil {
		return fmt.Errorf("process_audio %s: mark normalizing: %w", in.AssetID, err)
	}

	// Normalize to the canonical derivative (mono 16k opus), reused by every
	// segment and evicted with the asset (D5).
	normKey := storage.TenantKey(ctx, fmt.Sprintf("assets/%s/normalized.opus", in.AssetID))
	// Re-presign the source for the (separate) normalize call so a slow probe
	// can't have expired it.
	srcURL, err = p.Deps.Storage.PresignedGetURL(ctx, originalKey, audioPresignTTL)
	if err != nil {
		return fmt.Errorf("process_audio %s: presign source (normalize): %w", in.AssetID, err)
	}
	dstURL, err := p.Deps.Storage.PresignedPutURL(ctx, normKey, normalizedContentType, audioPresignTTL)
	if err != nil {
		return fmt.Errorf("process_audio %s: presign normalized: %w", in.AssetID, err)
	}
	norm, err := p.Deps.Client.Normalize(ctx, audio.NormalizeOptions{SourceURL: srcURL, DestPutURL: dstURL})
	if err != nil {
		if audio.IsInvalidAudio(err) || audio.IsUnsupportedAudio(err) {
			return p.terminalReject(ctx, in, ext, "the audio could not be transcoded (corrupt or unsupported format)")
		}
		return fmt.Errorf("process_audio %s: normalize: %w", in.AssetID, err) // transient → retry
	}
	if norm.DurationMs > 0 {
		ext.SourceDurationMs = norm.DurationMs
		ext.AudioSeconds = norm.DurationMs / 1000
	}
	ext.NormalizedS3Key = &normKey
	ext.Status = models.AudioExtractionStatusTranscribing
	if err := p.Deps.Extractions.Update(ctx, ext); err != nil {
		return fmt.Errorf("process_audio %s: mark transcribing: %w", in.AssetID, err)
	}
	return nil
}

// createSegments computes the bounded windows and persists the initial pending
// segment set. Idempotent across retries (ON CONFLICT DO NOTHING).
func (p *ProcessAudioProcessor) createSegments(ctx context.Context, in ProcessAudioTask, ext *models.AudioExtraction) ([]models.AudioSegment, error) {
	windows := audioWindows(ext.SourceDurationMs, p.segmentMaxMs(), p.segmentOverlapMs())
	segs := make([]models.AudioSegment, 0, len(windows))
	for i, w := range windows {
		id, err := models.NewID()
		if err != nil {
			return nil, fmt.Errorf("process_audio %s: new segment id: %w", in.AssetID, err)
		}
		segs = append(segs, models.AudioSegment{
			ID:           id,
			ExtractionID: ext.ID,
			AssetID:      in.AssetID,
			Index:        i,
			StartMs:      w[0],
			EndMs:        w[1],
			Status:       models.AudioSegmentStatusPending,
		})
	}
	if err := p.Deps.Segments.CreateMany(ctx, segs); err != nil {
		return nil, fmt.Errorf("process_audio %s: create segments: %w", in.AssetID, err)
	}
	ext.SegmentCount = len(windows)
	if err := p.Deps.Extractions.Update(ctx, ext); err != nil {
		return nil, fmt.Errorf("process_audio %s: record segment count: %w", in.AssetID, err)
	}
	return p.Deps.Segments.ListByExtraction(ctx, ext.ID)
}

// transcribeSegment transcribes one window, persists its utterances, checkpoints
// the segment, meters + accrues cost. A transient failure bumps retry_count and
// returns the error (River resumes here) — unless it's the last attempt, when it
// is downgraded to a terminal segment failure so the run can settle to partial.
func (p *ProcessAudioProcessor) transcribeSegment(ctx context.Context, in ProcessAudioTask, ext *models.AudioExtraction, seg *models.AudioSegment, model string, lastAttempt bool) error {
	url, err := p.Deps.Storage.PresignedGetURL(ctx, *ext.NormalizedS3Key, audioPresignTTL)
	if err != nil {
		return fmt.Errorf("process_audio %s: presign normalized: %w", in.AssetID, err)
	}
	res, err := p.Deps.Client.TranscribeSegment(ctx, audio.TranscribeSegmentOptions{
		NormalizedURL: url,
		StartMs:       seg.StartMs,
		EndMs:         seg.EndMs,
		Model:         model,
	})
	if err != nil {
		if audio.IsInvalidAudio(err) || audio.IsUnsupportedAudio(err) {
			return p.failSegment(ctx, seg, "segment could not be transcribed")
		}
		// Transient: bump the retry counter and either retry the job or, on the
		// last attempt, give up on just this segment (→ partial).
		seg.RetryCount++
		if lastAttempt {
			return p.failSegment(ctx, seg, "transcription repeatedly failed")
		}
		if uErr := p.Deps.Segments.Update(ctx, seg); uErr != nil {
			return fmt.Errorf("process_audio %s: checkpoint segment retry: %w", in.AssetID, uErr)
		}
		return fmt.Errorf("process_audio %s: transcribe segment %d: %w", in.AssetID, seg.Index, err)
	}

	utts := make([]models.Utterance, 0, len(res.Utterances))
	for i, u := range res.Utterances {
		id, idErr := models.NewID()
		if idErr != nil {
			return fmt.Errorf("process_audio %s: new utterance id: %w", in.AssetID, idErr)
		}
		utts = append(utts, models.Utterance{
			ID:         id,
			SegmentID:  seg.ID,
			AssetID:    in.AssetID,
			Index:      i,
			StartMs:    u.StartMs,
			EndMs:      u.EndMs,
			Text:       u.Text,
			Confidence: u.Confidence,
			Language:   u.Language,
			IsSpeech:   u.IsSpeech,
		})
	}
	if err := p.Deps.Utterances.ReplaceForSegment(ctx, seg.ID, utts); err != nil {
		return fmt.Errorf("process_audio %s: store utterances: %w", in.AssetID, err)
	}

	// Compute this segment's cost and persist it ON THE SEGMENT in the SAME write
	// that marks it done, so cost and completion commit atomically (CON-282): a
	// crash between the two can never leave a done segment whose cost is then lost
	// on the resume that skips it. finalize sums the per-segment costs.
	var segCost int64
	if res.InputTokens > 0 || res.OutputTokens > 0 {
		u := vendors.Usage{}
		if res.InputTokens > 0 {
			u[vendors.KindInput] = res.InputTokens
		}
		if res.OutputTokens > 0 {
			u[vendors.KindOutput] = res.OutputTokens
		}
		if micros, _, ok := vendors.CostOf(llm.VendorGemini, model, u); ok {
			segCost = micros
		}
	}
	seg.Status = models.AudioSegmentStatusDone
	seg.UtteranceCount = len(utts)
	seg.CostMicros = segCost
	seg.FailureReason = ""
	if err := p.Deps.Segments.Update(ctx, seg); err != nil {
		return fmt.Errorf("process_audio %s: checkpoint segment done: %w", in.AssetID, err)
	}

	// Best-effort AFTER the durable segment write: a usage event (CON-86) and an
	// early detected-language hint. A crash here loses only an analytics event or
	// a cosmetic hint (re-derived at finalize) — never the authoritative cost,
	// which now rides the segment row.
	if res.InputTokens > 0 || res.OutputTokens > 0 {
		p.Deps.Recorder.RecordResp(ctx, llm.VendorGemini, model, "transcribe", llm.TranscribeUsage{
			InputTokens:  res.InputTokens,
			OutputTokens: res.OutputTokens,
		})
	}
	if ext.DetectedLanguage == "" && res.DetectedLanguage != "" {
		ext.DetectedLanguage = res.DetectedLanguage
		_ = p.Deps.Extractions.Update(ctx, ext) // non-fatal; finalize re-derives it
	}
	return nil
}

// finalize inspects the settled segment set: all done → assemble + embed + store
// chunks and mark ready; any failed → partial (queryable, NOT searchable — no
// chunks written, awaiting retry).
func (p *ProcessAudioProcessor) finalize(ctx context.Context, in ProcessAudioTask, ext *models.AudioExtraction) error {
	segments, err := p.Deps.Segments.ListByExtraction(ctx, ext.ID)
	if err != nil {
		return fmt.Errorf("process_audio %s: reload segments: %w", in.AssetID, err)
	}
	var failed int
	var costSum int64
	for i := range segments {
		switch segments[i].Status {
		case models.AudioSegmentStatusFailed:
			failed++
		case models.AudioSegmentStatusDone:
		default:
			// A still-pending segment means the loop returned early on a transient
			// error; propagate so the job retries rather than settling prematurely.
			return fmt.Errorf("process_audio %s: segment %d not terminal", in.AssetID, segments[i].Index)
		}
		costSum += segments[i].CostMicros
	}
	// The run cost is the sum of the per-segment costs each done segment persisted
	// atomically, so it's exact no matter how many attempts the run took (CON-282).
	ext.CostMicros = costSum
	if desc, ok := vendors.Get(llm.VendorGemini); ok {
		ext.PriceVersion = desc.Prices.Version
	}

	if failed > 0 {
		ext.Status = models.AudioExtractionStatusPartial
		ext.FailureReason = "some segments could not be transcribed"
		if err := p.Deps.Extractions.Update(ctx, ext); err != nil {
			return err
		}
		return p.setAssetStatus(ctx, in.AssetID, models.AssetStatusPartial)
	}

	// All done → assemble time-anchored chunks from THIS run's utterances only
	// (scoped to the extraction, so a re-run never mixes in a prior run's spans).
	utts, err := p.Deps.Utterances.ListByExtraction(ctx, ext.ID)
	if err != nil {
		return fmt.Errorf("process_audio %s: load utterances: %w", in.AssetID, err)
	}
	// Fall back to the first speech utterance's language if a segment's best-effort
	// detected-language write didn't land (crash-then-resume).
	if ext.DetectedLanguage == "" {
		for i := range utts {
			if utts[i].IsSpeech && utts[i].Language != "" {
				ext.DetectedLanguage = utts[i].Language
				break
			}
		}
	}
	assembled := assembleAudioChunks(utts)

	chunks := make([]models.AssetChunk, 0, len(assembled))
	var embedAttempts, embedFailures int
	var totalEmbedTokens int64
	for idx, a := range assembled {
		text := a.text()
		if !hasWords(text) {
			continue
		}
		embedAttempts++
		emb, eErr := p.Deps.Embedder.Embed(ctx, &ai.EmbedRequest{
			Input:   []*ai.Document{ai.DocumentFromText(text, nil)},
			Options: embedopts.Document(),
		})
		if eErr != nil || len(emb.Embeddings) != 1 {
			embedFailures++
			continue
		}
		label := formatTimeRange(a.startMs, a.endMs)
		tokens := estimateTokens(text)
		totalEmbedTokens += int64(tokens)
		chunks = append(chunks, models.AssetChunk{
			ID:          fmt.Sprintf("%s:%d", in.AssetID, idx),
			AssetID:     in.AssetID,
			ChunkIndex:  idx,
			Content:     text,
			TokenCount:  tokens,
			Embedding:   pgvector.NewHalfVector(emb.Embeddings[0].Embedding),
			Model:       p.Deps.Embedder.Name(),
			SourceLabel: &label,
			SourceAnchor: &models.SourceAnchor{
				Kind:       "time",
				StartMs:    a.startMs,
				EndMs:      a.endMs,
				Provenance: "transcript",
			},
		})
	}

	// Every chunk failed to embed (transient embedder outage) — retry, unless
	// there was nothing to embed (silent/no-speech transcript → ready, 0 chunks).
	if embedAttempts > 0 && len(chunks) == 0 {
		return fmt.Errorf("process_audio %s: all %d chunk(s) failed to embed", in.AssetID, embedAttempts)
	}
	if len(chunks) > 0 && p.Deps.Chunks != nil {
		if err := p.Deps.Chunks.UpsertChunks(ctx, in.AssetID, chunks); err != nil {
			return fmt.Errorf("process_audio %s: store chunks: %w", in.AssetID, err)
		}
	}
	// The transcript becomes the asset's content (read-only to PUT, CON-312):
	// the same de-overlapped text the chunks hold, one paragraph per chunk.
	if p.Deps.Content != nil {
		paras := make([]string, 0, len(assembled))
		for _, a := range assembled {
			if t := a.text(); hasWords(t) {
				paras = append(paras, t)
			}
		}
		if err := p.Deps.Content.SetContent(ctx, in.AssetID, strings.Join(paras, "\n\n")); err != nil {
			return fmt.Errorf("process_audio %s: store transcript: %w", in.AssetID, err)
		}
	}

	// Persist the ASSET terminal status BEFORE marking the extraction complete
	// (CON-282): the completed-run guard in process() short-circuits a retry, so
	// flipping the extraction to complete first and then failing the asset-status
	// write would strand the asset in "processing" forever. This order lets a
	// retry re-run finalize (idempotent — chunks upsert-replace) and re-attempt
	// the status write. A partial embed (some chunks failed) is ready-with-gaps; a
	// clean run is ready. Either way the asset is searchable.
	status := models.AssetStatusReady
	if embedFailures > 0 {
		status = models.AssetStatusPartial
	}
	if err := p.setAssetStatus(ctx, in.AssetID, status); err != nil {
		return err
	}
	// Meter the transcript-chunk embeddings on the gemini vendor, like
	// document_extract (CON-312): only after the durable writes (chunks +
	// transcript + status), so a retry from a late failure can't double-count.
	if totalEmbedTokens > 0 {
		p.Deps.Recorder.RecordResp(ctx, llm.VendorGemini, p.Deps.EmbedModel, "audio_embed", llm.EmbedUsage{Tokens: totalEmbedTokens})
	}
	ext.Status = models.AudioExtractionStatusComplete
	ext.FailureReason = ""
	return p.Deps.Extractions.Update(ctx, ext)
}

// terminalReject marks the extraction + asset failed with a tenant-visible
// reason and does NOT return an error (no retry). Leaves ext.Status == failed.
func (p *ProcessAudioProcessor) terminalReject(ctx context.Context, in ProcessAudioTask, ext *models.AudioExtraction, reason string) error {
	ext.Status = models.AudioExtractionStatusFailed
	ext.FailureReason = reason
	if err := p.Deps.Extractions.Update(ctx, ext); err != nil {
		return fmt.Errorf("process_audio %s: mark extraction failed: %w", in.AssetID, err)
	}
	slog.WarnContext(ctx, "audio ingestion rejected", logging.AttrComponent, "jobs.process_audio", "asset_id", in.AssetID, "reason", reason)
	return p.setAssetStatus(ctx, in.AssetID, models.AssetStatusFailed)
}

func (p *ProcessAudioProcessor) failSegment(ctx context.Context, seg *models.AudioSegment, reason string) error {
	seg.Status = models.AudioSegmentStatusFailed
	seg.FailureReason = reason
	return p.Deps.Segments.Update(ctx, seg)
}

// setAssetStatus persists the asset status and announces terminal outcomes to
// the creator (CON-242). A nil Assets dep is a no-op.
func (p *ProcessAudioProcessor) setAssetStatus(ctx context.Context, assetID, status string) error {
	if p.Deps.Assets == nil {
		return nil
	}
	if err := p.Deps.Assets.UpdateStatus(ctx, assetID, status); err != nil {
		return fmt.Errorf("process_audio %s: set status %s: %w", assetID, status, err)
	}
	notifyAssetStatus(ctx, p.Deps.Notifier, p.Deps.Assets, assetID, status, "audio", models.AssetTypeAudio)
	return nil
}

func (p *ProcessAudioProcessor) transcribeModel(in ProcessAudioTask) string {
	if in.PinnedModel != "" {
		return in.PinnedModel
	}
	return p.Deps.TranscribeModel
}

func (p *ProcessAudioProcessor) segmentMaxMs() int64 {
	if p.Deps.SegmentMaxMs > 0 {
		return p.Deps.SegmentMaxMs
	}
	return defaultSegmentMaxMs
}

func (p *ProcessAudioProcessor) segmentOverlapMs() int64 {
	if p.Deps.SegmentOverlapMs > 0 {
		return p.Deps.SegmentOverlapMs
	}
	return defaultSegmentOverlapMs
}

// audioWindows splits [0, durationMs) into bounded windows of at most maxMs with
// a trailing overlap so an utterance straddling a boundary is captured by both
// neighbours (de-duplicated at assembly). Overlap is clamped below maxMs so the
// walk always advances.
func audioWindows(durationMs, maxMs, overlapMs int64) [][2]int64 {
	if durationMs <= 0 {
		return nil
	}
	if maxMs <= 0 {
		maxMs = defaultSegmentMaxMs
	}
	if overlapMs < 0 {
		overlapMs = 0
	}
	if overlapMs >= maxMs {
		overlapMs = maxMs / 2
	}
	var out [][2]int64
	var start int64
	for start < durationMs {
		end := start + maxMs
		if end >= durationMs {
			out = append(out, [2]int64{start, durationMs})
			break
		}
		out = append(out, [2]int64{start, end})
		start = end - overlapMs
	}
	return out
}

// assembledAudioChunk is an in-progress transcript chunk built on utterance
// boundaries, carrying its original-timeline span.
type assembledAudioChunk struct {
	parts   []string
	startMs int64
	endMs   int64
}

func (a assembledAudioChunk) text() string { return strings.Join(a.parts, " ") }

// assembleAudioChunks turns the raw, possibly-overlapping utterance stream into
// embed-ready chunks (CON-282 §8): drop no-speech + overlap duplicates
// (midpoint inside an already-kept span), never split an utterance, pack to a
// char/token budget on utterance boundaries, and merge a sub-minimum tail into
// the previous chunk.
func assembleAudioChunks(utts []models.Utterance) []assembledAudioChunk {
	var kept []models.Utterance
	var lastEnd int64 = -1
	for _, u := range utts {
		if !u.IsSpeech || strings.TrimSpace(u.Text) == "" {
			continue
		}
		mid := (u.StartMs + u.EndMs) / 2
		if mid < lastEnd {
			continue // overlaps an already-kept utterance (from the prior segment)
		}
		kept = append(kept, u)
		if u.EndMs > lastEnd {
			lastEnd = u.EndMs
		}
	}

	var chunks []assembledAudioChunk
	var cur assembledAudioChunk
	var curChars int
	for _, u := range kept {
		t := strings.TrimSpace(u.Text)
		if len(cur.parts) == 0 {
			cur.startMs = u.StartMs
		}
		cur.parts = append(cur.parts, t)
		cur.endMs = u.EndMs
		curChars += len(t) + 1
		if curChars >= audioChunkTargetChars {
			chunks = append(chunks, cur)
			cur = assembledAudioChunk{}
			curChars = 0
		}
	}
	if len(cur.parts) > 0 {
		if len(chunks) > 0 && curChars < audioMinChunkChars {
			prev := &chunks[len(chunks)-1]
			prev.parts = append(prev.parts, cur.parts...)
			prev.endMs = cur.endMs
		} else {
			chunks = append(chunks, cur)
		}
	}
	return chunks
}

// formatTimeRange renders a chunk's span as a citation like "12:03–12:47".
func formatTimeRange(startMs, endMs int64) string {
	return formatTimecode(startMs) + "–" + formatTimecode(endMs)
}

// formatTimecode renders milliseconds as M:SS (or H:MM:SS past an hour).
func formatTimecode(ms int64) string {
	if ms < 0 {
		ms = 0
	}
	totalSec := ms / 1000
	h := totalSec / 3600
	m := (totalSec % 3600) / 60
	s := totalSec % 60
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%d:%02d", m, s)
}
