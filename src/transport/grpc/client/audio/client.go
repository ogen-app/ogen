// Package audio is a thin gRPC client for the audio-service (CON-282). It owns
// the connection, the raised receive limit, and per-call deadlines, and presents
// audio transcoding + transcription as three narrow unary calls: Probe,
// Normalize, and TranscribeSegment.
//
// Like video — and unlike pdf/documents which client-stream the file bytes —
// audio hands the service short-lived presigned URLs (GET for reads, PUT for the
// normalized derivative) and lets ffmpeg range-read / stream only what it needs.
// That keeps the (potentially hour-long, multi-hundred-MB) audio out of the API
// process entirely (CON-282 §5, §13). The service is stateless: ogen owns the
// resumable state machine and drives one segment per TranscribeSegment call.
//
// The link to audio-service is private-network-only (Railway), so the channel is
// plaintext h2c — no TLS between the two.
package audio

import (
	"context"
	"errors"
	"fmt"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	grpcstatus "google.golang.org/grpc/status"

	audiov1 "github.com/ogen-app/ogen/gen/audio/v1"
)

const defaultTimeout = 10 * time.Minute

// ErrDisabled is returned when a call is made on a disabled (nil) client — i.e.
// AUDIO_SERVICE_ADDR was not configured.
var ErrDisabled = errors.New("audio: disabled (AUDIO_SERVICE_ADDR not set)")

// IsInvalidAudio reports whether err is the service's terminal "not a usable
// audio" verdict — gRPC InvalidArgument: the input is corrupt, truncated,
// zero-length, or silent-throughout, and will never transcribe, so it must not
// be retried. Transport/internal failures (Unavailable, DeadlineExceeded,
// Internal) are transient and return false, so the job retries them.
func IsInvalidAudio(err error) bool {
	st, ok := grpcstatus.FromError(err)
	return ok && st.Code() == codes.InvalidArgument
}

// IsUnsupportedAudio reports whether err is the service's terminal "format not
// supported" verdict — gRPC Unimplemented: an unrecognised container/codec the
// service can't decode. The file must be converted before ingestion, so this
// must not be retried.
func IsUnsupportedAudio(err error) bool {
	st, ok := grpcstatus.FromError(err)
	return ok && st.Code() == codes.Unimplemented
}

// ProbeOptions controls a single Probe call.
type ProbeOptions struct {
	SourceURL string // short-lived presigned GET URL to the uploaded object
	Filename  string // client-declared name, for logging / container hints
}

// ProbeResult is the metadata audio-service extracted without transcoding. Zero
// fields mean "unknown". Silent marks a silent-throughout / zero-length input
// the caller must reject terminally before any transcode/transcribe spend.
type ProbeResult struct {
	DurationMs int64
	Channels   int
	SampleRate int
	Container  string
	Codec      string
	Silent     bool
}

// NormalizeOptions controls a single Normalize call.
type NormalizeOptions struct {
	SourceURL        string // presigned GET (source)
	DestPutURL       string // presigned PUT (normalized.opus)
	TargetSampleRate int    // 0 -> service default (16000)
}

// NormalizeResult is the normalized derivative's metadata.
type NormalizeResult struct {
	DurationMs   int64
	SampleRate   int
	Channels     int
	BytesWritten int64
}

// TranscribeSegmentOptions controls a single TranscribeSegment call. StartMs/
// EndMs bound the window on the ORIGINAL asset timeline; Model is the Gemini
// transcription model id (config, never compiled in).
type TranscribeSegmentOptions struct {
	NormalizedURL string
	StartMs       int64
	EndMs         int64
	LanguageHint  string
	Model         string
}

// Utterance is one transcribed span with offsets rebased to the ORIGINAL asset
// timeline (model-reported, approximate). IsSpeech is false for a marked
// no-speech region (not empty text).
type Utterance struct {
	Text       string
	StartMs    int64
	EndMs      int64
	Confidence float64
	Language   string
	IsSpeech   bool
}

// TranscribeSegmentResult carries a segment's utterances plus the Gemini token
// usage ogen prices via the existing gemini vendor (CON-86).
type TranscribeSegmentResult struct {
	DetectedLanguage string
	Utterances       []Utterance
	InputTokens      int64
	OutputTokens     int64
}

// Config wires a Client.
type Config struct {
	Addr         string        // gRPC target (private-network host:port); empty disables
	Timeout      time.Duration // per-call deadline; <=0 uses the default
	MaxRecvBytes int           // client max-recv; <=0 leaves the gRPC default
}

// Client is a thin gRPC client for the audio-service, safe for concurrent use.
// A nil *Client is the "disabled" sentinel (no AUDIO_SERVICE_ADDR) and every
// method fails cleanly with ErrDisabled.
type Client struct {
	conn    *grpc.ClientConn
	rpc     audiov1.AudioServiceClient
	timeout time.Duration
}

// New dials the audio-service. When cfg.Addr is empty it returns (nil, nil): a
// nil client that reports ErrDisabled, mirroring the video/pdf clients.
// grpc.NewClient connects lazily, so this never blocks on the service being up.
func New(cfg Config) (*Client, error) {
	if cfg.Addr == "" {
		return nil, nil
	}
	// The correlation interceptors copy request_id/tenant_id from the call
	// context into outgoing gRPC metadata so audio-service's logs join the
	// API's (CON-111).
	dialOpts := []grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithChainStreamInterceptor(correlationStreamInterceptor),
		grpc.WithChainUnaryInterceptor(correlationUnaryInterceptor),
	}
	if cfg.MaxRecvBytes > 0 {
		dialOpts = append(dialOpts, grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(cfg.MaxRecvBytes)))
	}
	conn, err := grpc.NewClient(cfg.Addr, dialOpts...)
	if err != nil {
		return nil, fmt.Errorf("audio: dial %q: %w", cfg.Addr, err)
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	return &Client{conn: conn, rpc: audiov1.NewAudioServiceClient(conn), timeout: timeout}, nil
}

// Close releases the underlying connection. Safe on a nil client.
func (c *Client) Close() error {
	if c == nil || c.conn == nil {
		return nil
	}
	return c.conn.Close()
}

// Probe reads the audio at opts.SourceURL and returns its metadata + a
// silence/zero-length verdict, without transcoding.
func (c *Client) Probe(ctx context.Context, opts ProbeOptions) (*ProbeResult, error) {
	if c == nil {
		return nil, ErrDisabled
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	resp, err := c.rpc.Probe(ctx, &audiov1.ProbeRequest{
		SourceUrl: opts.SourceURL,
		Filename:  opts.Filename,
	})
	if err != nil {
		return nil, fmt.Errorf("audio: probe: %w", err)
	}
	return &ProbeResult{
		DurationMs: resp.GetDurationMs(),
		Channels:   int(resp.GetChannels()),
		SampleRate: int(resp.GetSampleRate()),
		Container:  resp.GetContainer(),
		Codec:      resp.GetCodec(),
		Silent:     resp.GetSilent(),
	}, nil
}

// Normalize transcodes the source to mono @ target sample rate and streams the
// result to opts.DestPutURL, returning the derivative's metadata.
func (c *Client) Normalize(ctx context.Context, opts NormalizeOptions) (*NormalizeResult, error) {
	if c == nil {
		return nil, ErrDisabled
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	resp, err := c.rpc.Normalize(ctx, &audiov1.NormalizeRequest{
		SourceUrl:        opts.SourceURL,
		DestPutUrl:       opts.DestPutURL,
		TargetSampleRate: int32(opts.TargetSampleRate),
	})
	if err != nil {
		return nil, fmt.Errorf("audio: normalize: %w", err)
	}
	return &NormalizeResult{
		DurationMs:   resp.GetDurationMs(),
		SampleRate:   int(resp.GetSampleRate()),
		Channels:     int(resp.GetChannels()),
		BytesWritten: resp.GetBytesWritten(),
	}, nil
}

// TranscribeSegment cuts [opts.StartMs, opts.EndMs) from the normalized
// derivative and transcribes it, returning utterances rebased to the original
// timeline plus the Gemini token usage.
func (c *Client) TranscribeSegment(ctx context.Context, opts TranscribeSegmentOptions) (*TranscribeSegmentResult, error) {
	if c == nil {
		return nil, ErrDisabled
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	resp, err := c.rpc.TranscribeSegment(ctx, &audiov1.TranscribeSegmentRequest{
		NormalizedUrl: opts.NormalizedURL,
		StartMs:       opts.StartMs,
		EndMs:         opts.EndMs,
		LanguageHint:  opts.LanguageHint,
		Model:         opts.Model,
	})
	if err != nil {
		return nil, fmt.Errorf("audio: transcribe segment: %w", err)
	}
	out := &TranscribeSegmentResult{
		DetectedLanguage: resp.GetDetectedLanguage(),
		InputTokens:      resp.GetInputTokens(),
		OutputTokens:     resp.GetOutputTokens(),
	}
	out.Utterances = make([]Utterance, 0, len(resp.GetUtterances()))
	for _, u := range resp.GetUtterances() {
		out.Utterances = append(out.Utterances, Utterance{
			Text:       u.GetText(),
			StartMs:    u.GetStartMs(),
			EndMs:      u.GetEndMs(),
			Confidence: float64(u.GetConfidence()),
			Language:   u.GetLanguage(),
			IsSpeech:   u.GetIsSpeech(),
		})
	}
	return out, nil
}
