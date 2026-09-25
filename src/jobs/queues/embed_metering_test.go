package queues

import (
	"context"
	"expvar"
	"sync"
	"testing"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
	"github.com/ogen-app/ogen/src/kernel/usage"
	"github.com/ogen-app/ogen/src/transport/grpc/client/audio"
	imageclient "github.com/ogen-app/ogen/src/transport/grpc/client/image"
)

// captureUsage is a usage.Writer that keeps every flushed event.
type captureUsage struct {
	mu     sync.Mutex
	events []*models.UsageEvent
}

func (w *captureUsage) Insert(_ context.Context, events []*models.UsageEvent) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.events = append(w.events, events...)
	return nil
}

// newCaptureRecorder returns a real Recorder backed by captureUsage. Call the
// returned flush to drain the recorder before asserting.
func newCaptureRecorder(t *testing.T) (*usage.Recorder, func() []*models.UsageEvent) {
	t.Helper()
	w := &captureUsage{}
	m := &usage.Metrics{
		EventsRecorded: new(expvar.Int), EventsDropped: new(expvar.Int), WriteErrors: new(expvar.Int),
		UnknownModel: new(expvar.Int), UnknownVendor: new(expvar.Int), LimitBlocks: new(expvar.Int),
		LimitWarnings: new(expvar.Int), EnforcementDegraded: new(expvar.Int),
	}
	rec := usage.NewRecorder(w, m, usage.Config{})
	return rec, func() []*models.UsageEvent {
		if err := rec.Close(context.Background()); err != nil {
			t.Fatalf("close recorder: %v", err)
		}
		w.mu.Lock()
		defer w.mu.Unlock()
		return w.events
	}
}

func eventsFor(events []*models.UsageEvent, feature string) []*models.UsageEvent {
	var out []*models.UsageEvent
	for _, e := range events {
		if e.Feature == feature {
			out = append(out, e)
		}
	}
	return out
}

// TestProcessAudio_MetersEmbedding: transcript-chunk embeddings are recorded on
// the gemini vendor as audio_embed (CON-312), beside the transcribe events.
func TestProcessAudio_MetersEmbedding(t *testing.T) {
	client := &fakeAudioClient{
		probe: &audio.ProbeResult{DurationMs: 60_000},
		norm:  &audio.NormalizeResult{DurationMs: 60_000},
		transcribe: &audio.TranscribeSegmentResult{Utterances: []audio.Utterance{
			{Text: "hello world", StartMs: 0, EndMs: 2_000, IsSpeech: true},
		}},
	}
	ext, seg, utt, status, chunks := &fakeExtractions{}, &fakeSegments{}, &fakeUtterances{}, &fakeStatus{}, &fakeChunks{}
	d := baseAudioDeps(client, ext, seg, utt, status, chunks)
	rec, flush := newCaptureRecorder(t)
	d.Recorder = rec

	task := ProcessAudioTask{AssetID: "a1", RunKey: "run-1", StorageKey: "assets/a1/original.mp3", MimeType: "audio/mpeg"}
	// Usage is attributed per tenant; an untenanted call is dropped.
	if err := newAudioProc(d).process(tenantctx.With(t.Context(), "t1"), task, false); err != nil {
		t.Fatalf("process: %v", err)
	}
	got := eventsFor(flush(), "audio_embed")
	if len(got) != 1 {
		t.Fatalf("want 1 audio_embed event, got %d", len(got))
	}
	if got[0].Vendor != "gemini" || got[0].Model != "gemini-embedding-2" || got[0].Operation != "embed" {
		t.Fatalf("event = %+v", got[0])
	}
}

// TestProcessImage_MetersEmbedding: description + region embeddings are
// recorded as image_embed (CON-312), beside the image_extract vision events.
func TestProcessImage_MetersEmbedding(t *testing.T) {
	client := &fakeImageClient{res: &imageclient.ExtractResult{
		Description: "a bar chart of revenue", DescriptionOK: true, ExtractionOK: true,
		Blocks: []imageclient.Block{{Text: "Revenue by month"}},
	}}
	d, _, _, _, _ := baseImageDeps(client)
	d.EmbedModel = "gemini-embedding-2"
	rec, flush := newCaptureRecorder(t)
	d.Recorder = rec

	task := ProcessImageTask{AssetID: "a1", RunKey: "run-1", StorageKey: "assets/a1/original.png", MimeType: "image/png"}
	if err := newImageProc(d).process(tenantctx.With(t.Context(), "t1"), task, false); err != nil {
		t.Fatalf("process: %v", err)
	}
	got := eventsFor(flush(), "image_embed")
	if len(got) != 1 {
		t.Fatalf("want 1 image_embed event, got %d", len(got))
	}
	if got[0].Vendor != "gemini" || got[0].Operation != "embed" {
		t.Fatalf("event = %+v", got[0])
	}
}
