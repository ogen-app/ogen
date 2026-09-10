package queues

import (
	"context"
	"database/sql"
	"sort"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/transport/grpc/client/audio"
)

// --- fakes specific to process_audio (embedder/status/chunks are shared) ---

type fakeAudioClient struct {
	probe    *audio.ProbeResult
	probeErr error
	norm     *audio.NormalizeResult
	normErr  error
	// transcribe returns a canned result (or transcribeErr) for every segment.
	transcribe    *audio.TranscribeSegmentResult
	transcribeErr error

	probeCalls, normalizeCalls, transcribeCalls int
}

func (f *fakeAudioClient) Probe(context.Context, audio.ProbeOptions) (*audio.ProbeResult, error) {
	f.probeCalls++
	return f.probe, f.probeErr
}

func (f *fakeAudioClient) Normalize(context.Context, audio.NormalizeOptions) (*audio.NormalizeResult, error) {
	f.normalizeCalls++
	return f.norm, f.normErr
}

func (f *fakeAudioClient) TranscribeSegment(_ context.Context, _ audio.TranscribeSegmentOptions) (*audio.TranscribeSegmentResult, error) {
	f.transcribeCalls++
	if f.transcribeErr != nil {
		return nil, f.transcribeErr
	}
	return f.transcribe, nil
}

// presignBlob satisfies audioBlobStore — presign is a pure string op here.
type presignBlob struct{}

func (presignBlob) PresignedGetURL(_ context.Context, key string, _ time.Duration) (string, error) {
	return "https://get/" + key, nil
}
func (presignBlob) PresignedPutURL(_ context.Context, key, _ string, _ time.Duration) (string, error) {
	return "https://put/" + key, nil
}

type fakeExtractions struct {
	m map[string]*models.AudioExtraction
}

func (f *fakeExtractions) key(assetID, runKey string) string { return assetID + "|" + runKey }
func (f *fakeExtractions) Create(_ context.Context, e *models.AudioExtraction) error {
	if f.m == nil {
		f.m = map[string]*models.AudioExtraction{}
	}
	f.m[f.key(e.AssetID, e.RunKey)] = e
	return nil
}
func (f *fakeExtractions) GetByAssetAndRunKey(_ context.Context, assetID, runKey string) (*models.AudioExtraction, error) {
	if e, ok := f.m[f.key(assetID, runKey)]; ok {
		return e, nil
	}
	return nil, sql.ErrNoRows
}
func (f *fakeExtractions) Update(_ context.Context, e *models.AudioExtraction) error {
	if f.m == nil {
		f.m = map[string]*models.AudioExtraction{}
	}
	f.m[f.key(e.AssetID, e.RunKey)] = e
	return nil
}

type fakeSegments struct {
	m map[string]map[int]models.AudioSegment // extractionID -> index -> segment
}

func (f *fakeSegments) ensure(extID string) {
	if f.m == nil {
		f.m = map[string]map[int]models.AudioSegment{}
	}
	if f.m[extID] == nil {
		f.m[extID] = map[int]models.AudioSegment{}
	}
}
func (f *fakeSegments) CreateMany(_ context.Context, segs []models.AudioSegment) error {
	for _, s := range segs {
		f.ensure(s.ExtractionID)
		if _, exists := f.m[s.ExtractionID][s.Index]; !exists {
			f.m[s.ExtractionID][s.Index] = s
		}
	}
	return nil
}
func (f *fakeSegments) ListByExtraction(_ context.Context, extID string) ([]models.AudioSegment, error) {
	byIdx := f.m[extID]
	out := make([]models.AudioSegment, 0, len(byIdx))
	for _, s := range byIdx {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Index < out[j].Index })
	return out, nil
}
func (f *fakeSegments) Update(_ context.Context, s *models.AudioSegment) error {
	f.ensure(s.ExtractionID)
	f.m[s.ExtractionID][s.Index] = *s
	return nil
}

type fakeUtterances struct {
	bySeg map[string][]models.Utterance
}

func (f *fakeUtterances) ReplaceForSegment(_ context.Context, segID string, utts []models.Utterance) error {
	if f.bySeg == nil {
		f.bySeg = map[string][]models.Utterance{}
	}
	f.bySeg[segID] = utts
	return nil
}

// ListByExtraction returns every stored utterance (the job tests use a single
// extraction), sorted into timeline order like the real repo.
func (f *fakeUtterances) ListByExtraction(_ context.Context, _ string) ([]models.Utterance, error) {
	var out []models.Utterance
	for _, utts := range f.bySeg {
		out = append(out, utts...)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].StartMs != out[j].StartMs {
			return out[i].StartMs < out[j].StartMs
		}
		return out[i].Index < out[j].Index
	})
	return out, nil
}

func newAudioProc(d AudioDeps) *ProcessAudioProcessor { return &ProcessAudioProcessor{Deps: d} }

func baseAudioDeps(client audioTranscriber, ext *fakeExtractions, seg *fakeSegments, utt *fakeUtterances, status *fakeStatus, chunks *fakeChunks) AudioDeps {
	return AudioDeps{
		Client:          client,
		Embedder:        &fakeEmbedder{},
		Storage:         presignBlob{},
		Assets:          status,
		Chunks:          chunks,
		Extractions:     ext,
		Segments:        seg,
		Utterances:      utt,
		EmbedModel:      "gemini-embedding-2",
		TranscribeModel: "gemini-2.5-flash",
		SegmentMaxMs:    defaultSegmentMaxMs,
	}
}

// TestProcessAudio_Success_TimeAnchoredChunks is the heart of the job: a clean
// run normalizes once, transcribes the single window, and assembles a time-
// anchored chunk (kind "time", original-timeline offsets, "M:SS–M:SS" label).
func TestProcessAudio_Success_TimeAnchoredChunks(t *testing.T) {
	client := &fakeAudioClient{
		probe: &audio.ProbeResult{DurationMs: 120_000, Channels: 2, SampleRate: 44_100},
		norm:  &audio.NormalizeResult{DurationMs: 120_000, Channels: 1, SampleRate: 16_000},
		transcribe: &audio.TranscribeSegmentResult{
			DetectedLanguage: "en", InputTokens: 100, OutputTokens: 50,
			Utterances: []audio.Utterance{
				{Text: "hello world", StartMs: 0, EndMs: 2_000, IsSpeech: true, Language: "en", Confidence: 0.9},
				{Text: "second line", StartMs: 2_000, EndMs: 4_000, IsSpeech: true, Language: "en", Confidence: 0.8},
			},
		},
	}
	ext, seg, utt, status, chunks := &fakeExtractions{}, &fakeSegments{}, &fakeUtterances{}, &fakeStatus{}, &fakeChunks{}
	p := newAudioProc(baseAudioDeps(client, ext, seg, utt, status, chunks))

	task := ProcessAudioTask{AssetID: "a1", RunKey: "run-1", StorageKey: "assets/a1/original.mp3", MimeType: "audio/mpeg"}
	if err := p.process(t.Context(), task, false); err != nil {
		t.Fatalf("process: %v", err)
	}

	if client.probeCalls != 1 || client.normalizeCalls != 1 || client.transcribeCalls != 1 {
		t.Fatalf("call counts: probe=%d normalize=%d transcribe=%d", client.probeCalls, client.normalizeCalls, client.transcribeCalls)
	}
	if len(chunks.got) != 1 {
		t.Fatalf("want 1 assembled chunk, got %d", len(chunks.got))
	}
	c := chunks.got[0]
	if c.SourceAnchor == nil || c.SourceAnchor.Kind != "time" || c.SourceAnchor.StartMs != 0 || c.SourceAnchor.EndMs != 4_000 || c.SourceAnchor.Provenance != "transcript" {
		t.Fatalf("time anchor wrong: %+v", c.SourceAnchor)
	}
	if c.SourceLabel == nil || *c.SourceLabel != "0:00–0:04" {
		t.Fatalf("source label wrong: %v", c.SourceLabel)
	}
	if c.Content != "hello world second line" {
		t.Fatalf("chunk content wrong: %q", c.Content)
	}
	if status.last() != models.AssetStatusReady {
		t.Fatalf("final status = %q, want ready (saw %v)", status.last(), status.all)
	}
	got := ext.m["a1|run-1"]
	if got.Status != models.AudioExtractionStatusComplete || got.DetectedLanguage != "en" {
		t.Fatalf("extraction end state wrong: %+v", got)
	}
	if got.CostMicros <= 0 || got.PriceVersion == "" {
		t.Fatalf("cost not snapshotted: micros=%d ver=%q", got.CostMicros, got.PriceVersion)
	}
}

// TestProcessAudio_ResumeFromIncomplete: a pre-normalized run with segment 0
// already done transcribes ONLY segment 1 (no re-probe / re-normalize).
func TestProcessAudio_ResumeFromIncomplete(t *testing.T) {
	normKey := "t/x/assets/a2/normalized.opus"
	ext := &fakeExtractions{m: map[string]*models.AudioExtraction{
		"a2|run-1": {ID: "e2", AssetID: "a2", RunKey: "run-1", Status: models.AudioExtractionStatusTranscribing, NormalizedS3Key: &normKey, SourceDurationMs: 120_000, SegmentCount: 2},
	}}
	seg := &fakeSegments{m: map[string]map[int]models.AudioSegment{
		"e2": {
			0: {ID: "s0", ExtractionID: "e2", AssetID: "a2", Index: 0, StartMs: 0, EndMs: 60_000, Status: models.AudioSegmentStatusDone},
			1: {ID: "s1", ExtractionID: "e2", AssetID: "a2", Index: 1, StartMs: 60_000, EndMs: 120_000, Status: models.AudioSegmentStatusPending},
		},
	}}
	utt := &fakeUtterances{bySeg: map[string][]models.Utterance{
		"s0": {{ID: "u0", SegmentID: "s0", AssetID: "a2", Index: 0, StartMs: 0, EndMs: 3_000, Text: "already done", IsSpeech: true}},
	}}
	client := &fakeAudioClient{transcribe: &audio.TranscribeSegmentResult{
		Utterances: []audio.Utterance{{Text: "the rest", StartMs: 61_000, EndMs: 63_000, IsSpeech: true}},
	}}
	status, chunks := &fakeStatus{}, &fakeChunks{}
	p := newAudioProc(baseAudioDeps(client, ext, seg, utt, status, chunks))

	if err := p.process(t.Context(), ProcessAudioTask{AssetID: "a2", RunKey: "run-1", StorageKey: "assets/a2/original.mp3"}, false); err != nil {
		t.Fatalf("process: %v", err)
	}
	if client.probeCalls != 0 || client.normalizeCalls != 0 {
		t.Fatalf("resume must skip probe/normalize: probe=%d normalize=%d", client.probeCalls, client.normalizeCalls)
	}
	if client.transcribeCalls != 1 {
		t.Fatalf("resume must transcribe only the 1 incomplete segment, got %d", client.transcribeCalls)
	}
	if status.last() != models.AssetStatusReady {
		t.Fatalf("status = %q, want ready", status.last())
	}
	// Both segments' utterances end up in the assembled chunk.
	if len(chunks.got) != 1 || chunks.got[0].Content != "already done the rest" {
		t.Fatalf("assembled chunk wrong: %+v", chunks.got)
	}
}

func TestProcessAudio_SilentIsTerminal(t *testing.T) {
	client := &fakeAudioClient{probe: &audio.ProbeResult{DurationMs: 30_000, Silent: true}}
	ext, status := &fakeExtractions{}, &fakeStatus{}
	p := newAudioProc(baseAudioDeps(client, ext, &fakeSegments{}, &fakeUtterances{}, status, &fakeChunks{}))
	if err := p.process(t.Context(), ProcessAudioTask{AssetID: "a3", RunKey: "run-1", StorageKey: "assets/a3/original.wav"}, false); err != nil {
		t.Fatalf("silent audio must not retry: %v", err)
	}
	if client.normalizeCalls != 0 || client.transcribeCalls != 0 {
		t.Fatalf("silent reject must not normalize/transcribe")
	}
	if status.last() != models.AssetStatusFailed {
		t.Fatalf("status = %q, want failed", status.last())
	}
	if ext.m["a3|run-1"].Status != models.AudioExtractionStatusFailed {
		t.Fatalf("extraction not marked failed")
	}
}

func TestProcessAudio_OverMaxDurationIsTerminal(t *testing.T) {
	client := &fakeAudioClient{probe: &audio.ProbeResult{DurationMs: 600_000}}
	status := &fakeStatus{}
	deps := baseAudioDeps(client, &fakeExtractions{}, &fakeSegments{}, &fakeUtterances{}, status, &fakeChunks{})
	deps.MaxDurationMs = 60_000 // 1 min cap, source is 10 min
	p := newAudioProc(deps)
	if err := p.process(t.Context(), ProcessAudioTask{AssetID: "a4", RunKey: "run-1", StorageKey: "assets/a4/original.mp3"}, false); err != nil {
		t.Fatalf("over-limit must not retry: %v", err)
	}
	if client.normalizeCalls != 0 {
		t.Fatalf("over-limit must reject before normalize")
	}
	if status.last() != models.AssetStatusFailed {
		t.Fatalf("status = %q, want failed", status.last())
	}
}

func TestProcessAudio_TransientTranscribeRetries(t *testing.T) {
	client := &fakeAudioClient{
		probe:         &audio.ProbeResult{DurationMs: 120_000},
		norm:          &audio.NormalizeResult{DurationMs: 120_000},
		transcribeErr: grpcstatus.Error(codes.Unavailable, "gemini down"),
	}
	seg := &fakeSegments{}
	p := newAudioProc(baseAudioDeps(client, &fakeExtractions{}, seg, &fakeUtterances{}, &fakeStatus{}, &fakeChunks{}))
	if err := p.process(t.Context(), ProcessAudioTask{AssetID: "a5", RunKey: "run-1", StorageKey: "assets/a5/original.mp3"}, false); err == nil {
		t.Fatal("transient transcribe error should retry (want non-nil err)")
	}
	// The segment was checkpointed with an incremented retry count.
	segs, _ := seg.ListByExtraction(t.Context(), extractionID(seg))
	if len(segs) == 0 || segs[0].RetryCount != 1 {
		t.Fatalf("segment retry not checkpointed: %+v", segs)
	}
}

func TestProcessAudio_PartialOnLastAttempt(t *testing.T) {
	client := &fakeAudioClient{
		probe:         &audio.ProbeResult{DurationMs: 120_000},
		norm:          &audio.NormalizeResult{DurationMs: 120_000},
		transcribeErr: grpcstatus.Error(codes.Unavailable, "gemini down"),
	}
	ext, status, chunks := &fakeExtractions{}, &fakeStatus{}, &fakeChunks{}
	p := newAudioProc(baseAudioDeps(client, ext, &fakeSegments{}, &fakeUtterances{}, status, chunks))
	if err := p.process(t.Context(), ProcessAudioTask{AssetID: "a6", RunKey: "run-1", StorageKey: "assets/a6/original.mp3"}, true); err != nil {
		t.Fatalf("last attempt should settle (not error): %v", err)
	}
	if status.last() != models.AssetStatusPartial {
		t.Fatalf("status = %q, want partial", status.last())
	}
	if chunks.calls != 0 {
		t.Fatalf("partial run must NOT write searchable chunks")
	}
	if ext.m["a6|run-1"].Status != models.AudioExtractionStatusPartial {
		t.Fatalf("extraction not marked partial")
	}
}

func TestProcessAudio_AlreadyCompleteIsNoOp(t *testing.T) {
	client := &fakeAudioClient{}
	ext := &fakeExtractions{m: map[string]*models.AudioExtraction{
		"a7|run-1": {ID: "e7", AssetID: "a7", RunKey: "run-1", Status: models.AudioExtractionStatusComplete},
	}}
	status := &fakeStatus{}
	p := newAudioProc(baseAudioDeps(client, ext, &fakeSegments{}, &fakeUtterances{}, status, &fakeChunks{}))
	if err := p.process(t.Context(), ProcessAudioTask{AssetID: "a7", RunKey: "run-1"}, false); err != nil {
		t.Fatalf("complete run should no-op: %v", err)
	}
	if client.probeCalls != 0 || client.transcribeCalls != 0 || len(status.all) != 0 {
		t.Fatalf("complete run must do nothing (probe=%d transcribe=%d status=%v)", client.probeCalls, client.transcribeCalls, status.all)
	}
}

func TestProcessAudio_DisabledClientNoOp(t *testing.T) {
	status := &fakeStatus{}
	p := newAudioProc(AudioDeps{Client: nil, Assets: status})
	if err := p.process(t.Context(), ProcessAudioTask{AssetID: "a8"}, false); err != nil {
		t.Fatalf("disabled client should no-op: %v", err)
	}
	if len(status.all) != 0 {
		t.Fatalf("disabled client must not touch status, saw %v", status.all)
	}
}

// --- pure-helper tests ---

func TestAudioWindows(t *testing.T) {
	if got := audioWindows(120_000, 300_000, 5_000); len(got) != 1 || got[0] != [2]int64{0, 120_000} {
		t.Fatalf("single-window: %v", got)
	}
	got := audioWindows(600_000, 300_000, 5_000)
	want := [][2]int64{{0, 300_000}, {295_000, 595_000}, {590_000, 600_000}}
	if len(got) != len(want) {
		t.Fatalf("multi-window count: got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("window %d: got %v want %v", i, got[i], want[i])
		}
	}
}

func TestAssembleAudioChunks_DedupeAndSkipNonSpeech(t *testing.T) {
	utts := []models.Utterance{
		{StartMs: 0, EndMs: 1_000, Text: "a", IsSpeech: true},
		{StartMs: 1_000, EndMs: 2_000, Text: "b", IsSpeech: true},
		{StartMs: 1_000, EndMs: 2_000, Text: "b", IsSpeech: true}, // overlap dup (from next segment)
		{StartMs: 2_000, EndMs: 3_000, Text: "", IsSpeech: false}, // no-speech region, dropped
		{StartMs: 2_000, EndMs: 3_000, Text: "c", IsSpeech: true},
	}
	got := assembleAudioChunks(utts)
	if len(got) != 1 {
		t.Fatalf("want 1 chunk, got %d: %+v", len(got), got)
	}
	if got[0].text() != "a b c" || got[0].startMs != 0 || got[0].endMs != 3_000 {
		t.Fatalf("assembled wrong: %q [%d,%d]", got[0].text(), got[0].startMs, got[0].endMs)
	}
}

func TestFormatTimecode(t *testing.T) {
	cases := map[int64]string{0: "0:00", 63_000: "1:03", 723_000: "12:03", 3_661_000: "1:01:01"}
	for ms, want := range cases {
		if got := formatTimecode(ms); got != want {
			t.Fatalf("formatTimecode(%d) = %q, want %q", ms, got, want)
		}
	}
	if got := formatTimeRange(723_000, 767_000); got != "12:03–12:47" {
		t.Fatalf("formatTimeRange = %q", got)
	}
}

// extractionID returns the single extraction id a fakeSegments holds (test helper).
func extractionID(f *fakeSegments) string {
	for id := range f.m {
		return id
	}
	return ""
}
