package audio_test

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	audiov1 "github.com/ogen-app/ogen/gen/audio/v1"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
	"github.com/ogen-app/ogen/src/transport/grpc/client/audio"
)

// stubServer records each request and returns canned responses, or fails every
// call with err when set.
type stubServer struct {
	audiov1.UnimplementedAudioServiceServer
	err   error
	gotMD metadata.MD // inbound metadata of the most recent call

	gotProbe      *audiov1.ProbeRequest
	probeResp     *audiov1.ProbeResponse
	gotNormalize  *audiov1.NormalizeRequest
	normalizeResp *audiov1.NormalizeResponse
	gotTranscribe *audiov1.TranscribeSegmentRequest
	transResp     *audiov1.TranscribeSegmentResponse
}

func (s *stubServer) Probe(ctx context.Context, req *audiov1.ProbeRequest) (*audiov1.ProbeResponse, error) {
	s.gotMD, _ = metadata.FromIncomingContext(ctx)
	s.gotProbe = req
	if s.err != nil {
		return nil, s.err
	}
	return s.probeResp, nil
}

func (s *stubServer) Normalize(ctx context.Context, req *audiov1.NormalizeRequest) (*audiov1.NormalizeResponse, error) {
	s.gotMD, _ = metadata.FromIncomingContext(ctx)
	s.gotNormalize = req
	if s.err != nil {
		return nil, s.err
	}
	return s.normalizeResp, nil
}

func (s *stubServer) TranscribeSegment(ctx context.Context, req *audiov1.TranscribeSegmentRequest) (*audiov1.TranscribeSegmentResponse, error) {
	s.gotMD, _ = metadata.FromIncomingContext(ctx)
	s.gotTranscribe = req
	if s.err != nil {
		return nil, s.err
	}
	return s.transResp, nil
}

// serveStub starts the stub on a loopback listener and returns its address.
func serveStub(t *testing.T, stub *stubServer) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := grpc.NewServer()
	audiov1.RegisterAudioServiceServer(srv, stub)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return lis.Addr().String()
}

func newClient(t *testing.T, addr string) *audio.Client {
	t.Helper()
	c, err := audio.New(audio.Config{Addr: addr, Timeout: 5 * time.Second, MaxRecvBytes: 1 << 20})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestProbeSendsRequestAndMapsResult(t *testing.T) {
	stub := &stubServer{probeResp: &audiov1.ProbeResponse{
		DurationMs: 3_600_000, Channels: 2, SampleRate: 44100,
		Container: "mp3", Codec: "mp3", Silent: true,
	}}
	client := newClient(t, serveStub(t, stub))

	res, err := client.Probe(t.Context(), audio.ProbeOptions{SourceURL: "https://s3/get?sig=1", Filename: "ep1.mp3"})
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if got := stub.gotProbe; got.GetSourceUrl() != "https://s3/get?sig=1" || got.GetFilename() != "ep1.mp3" {
		t.Fatalf("probe request not propagated: %+v", got)
	}
	want := audio.ProbeResult{DurationMs: 3_600_000, Channels: 2, SampleRate: 44100, Container: "mp3", Codec: "mp3", Silent: true}
	if *res != want {
		t.Fatalf("probe result = %+v, want %+v", *res, want)
	}
}

func TestNormalizeSendsRequestAndMapsResult(t *testing.T) {
	stub := &stubServer{normalizeResp: &audiov1.NormalizeResponse{
		DurationMs: 61_000, SampleRate: 16000, Channels: 1, BytesWritten: 987_654,
	}}
	client := newClient(t, serveStub(t, stub))

	res, err := client.Normalize(t.Context(), audio.NormalizeOptions{
		SourceURL: "https://s3/get", DestPutURL: "https://s3/put", TargetSampleRate: 16000,
	})
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if got := stub.gotNormalize; got.GetSourceUrl() != "https://s3/get" || got.GetDestPutUrl() != "https://s3/put" ||
		got.GetTargetSampleRate() != 16000 {
		t.Fatalf("normalize request not propagated: %+v", got)
	}
	want := audio.NormalizeResult{DurationMs: 61_000, SampleRate: 16000, Channels: 1, BytesWritten: 987_654}
	if *res != want {
		t.Fatalf("normalize result = %+v, want %+v", *res, want)
	}
}

func TestTranscribeSegmentSendsRequestAndMapsResult(t *testing.T) {
	stub := &stubServer{transResp: &audiov1.TranscribeSegmentResponse{
		DetectedLanguage: "uk",
		InputTokens:      1200,
		OutputTokens:     340,
		Utterances: []*audiov1.Utterance{
			{Text: "Привіт", StartMs: 30_000, EndMs: 31_500, Confidence: 0.5, Language: "uk", IsSpeech: true},
			{Text: "", StartMs: 31_500, EndMs: 40_000, Confidence: -1, Language: "", IsSpeech: false},
		},
	}}
	client := newClient(t, serveStub(t, stub))

	res, err := client.TranscribeSegment(t.Context(), audio.TranscribeSegmentOptions{
		NormalizedURL: "https://s3/norm.opus", StartMs: 30_000, EndMs: 60_000,
		LanguageHint: "uk", Model: "gemini-2.5-flash",
	})
	if err != nil {
		t.Fatalf("TranscribeSegment: %v", err)
	}
	if got := stub.gotTranscribe; got.GetNormalizedUrl() != "https://s3/norm.opus" || got.GetStartMs() != 30_000 ||
		got.GetEndMs() != 60_000 || got.GetLanguageHint() != "uk" || got.GetModel() != "gemini-2.5-flash" {
		t.Fatalf("transcribe request not propagated: %+v", got)
	}
	if res.DetectedLanguage != "uk" || res.InputTokens != 1200 || res.OutputTokens != 340 {
		t.Fatalf("unexpected transcribe result: %+v", res)
	}
	want := []audio.Utterance{
		{Text: "Привіт", StartMs: 30_000, EndMs: 31_500, Confidence: 0.5, Language: "uk", IsSpeech: true},
		{Text: "", StartMs: 31_500, EndMs: 40_000, Confidence: -1, Language: "", IsSpeech: false},
	}
	if len(res.Utterances) != len(want) {
		t.Fatalf("got %d utterances, want %d", len(res.Utterances), len(want))
	}
	for i := range want {
		if res.Utterances[i] != want[i] {
			t.Fatalf("utterance %d = %+v, want %+v", i, res.Utterances[i], want[i])
		}
	}
}

func TestTranscribeSegmentEmptyUtterancesIsNonNil(t *testing.T) {
	stub := &stubServer{transResp: &audiov1.TranscribeSegmentResponse{DetectedLanguage: "en"}}
	client := newClient(t, serveStub(t, stub))

	res, err := client.TranscribeSegment(t.Context(), audio.TranscribeSegmentOptions{NormalizedURL: "u", EndMs: 1})
	if err != nil {
		t.Fatalf("TranscribeSegment: %v", err)
	}
	if res.Utterances == nil || len(res.Utterances) != 0 {
		t.Fatalf("expected empty non-nil utterances, got %#v", res.Utterances)
	}
}

func TestErrorsAreClassifiedAndUnwrappable(t *testing.T) {
	cases := []struct {
		name            string
		code            codes.Code
		wantInvalid     bool
		wantUnsupported bool
	}{
		{"invalid argument", codes.InvalidArgument, true, false},
		{"unimplemented", codes.Unimplemented, false, true},
		{"failed precondition is not terminal", codes.FailedPrecondition, false, false},
		{"unavailable is transient", codes.Unavailable, false, false},
		{"internal is transient", codes.Internal, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stub := &stubServer{err: status.Error(tc.code, "nope")}
			client := newClient(t, serveStub(t, stub))

			calls := map[string]func() error{
				"Probe": func() error {
					_, err := client.Probe(t.Context(), audio.ProbeOptions{SourceURL: "u"})
					return err
				},
				"Normalize": func() error {
					_, err := client.Normalize(t.Context(), audio.NormalizeOptions{SourceURL: "u", DestPutURL: "p"})
					return err
				},
				"TranscribeSegment": func() error {
					_, err := client.TranscribeSegment(t.Context(), audio.TranscribeSegmentOptions{NormalizedURL: "u"})
					return err
				},
			}
			for name, call := range calls {
				err := call()
				if err == nil {
					t.Fatalf("%s: expected an error", name)
				}
				if st, ok := status.FromError(err); !ok || st.Code() != tc.code {
					t.Fatalf("%s: status.FromError(%v) code = %v (ok=%v), want %v", name, err, st.Code(), ok, tc.code)
				}
				if got := audio.IsInvalidAudio(err); got != tc.wantInvalid {
					t.Fatalf("%s: IsInvalidAudio = %v, want %v", name, got, tc.wantInvalid)
				}
				if got := audio.IsUnsupportedAudio(err); got != tc.wantUnsupported {
					t.Fatalf("%s: IsUnsupportedAudio = %v, want %v", name, got, tc.wantUnsupported)
				}
			}
		})
	}
}

func TestClassifiersRejectNonStatusErrors(t *testing.T) {
	err := errors.New("plain")
	if audio.IsInvalidAudio(err) || audio.IsUnsupportedAudio(err) {
		t.Fatalf("plain error must not classify as terminal")
	}
	if audio.IsInvalidAudio(nil) || audio.IsUnsupportedAudio(nil) {
		t.Fatalf("nil error must not classify as terminal")
	}
}

// mdValue returns the single metadata value for key, or "" if absent.
func mdValue(md metadata.MD, key string) string {
	if v := md.Get(key); len(v) > 0 {
		return v[0]
	}
	return ""
}

// TestCallsPropagateCorrelationMetadata asserts the unary interceptor copies
// request_id/tenant_id from the call context into outgoing gRPC metadata under
// the exact header keys audio-service reads (CON-111), on every RPC.
func TestCallsPropagateCorrelationMetadata(t *testing.T) {
	stub := &stubServer{
		probeResp:     &audiov1.ProbeResponse{},
		normalizeResp: &audiov1.NormalizeResponse{},
		transResp:     &audiov1.TranscribeSegmentResponse{},
	}
	client := newClient(t, serveStub(t, stub))

	ctx := logging.WithRequestID(t.Context(), "rid")
	ctx = tenantctx.With(ctx, "ten")

	calls := []struct {
		name string
		call func() error
	}{
		{"Probe", func() error { _, err := client.Probe(ctx, audio.ProbeOptions{}); return err }},
		{"Normalize", func() error { _, err := client.Normalize(ctx, audio.NormalizeOptions{}); return err }},
		{"TranscribeSegment", func() error {
			_, err := client.TranscribeSegment(ctx, audio.TranscribeSegmentOptions{})
			return err
		}},
	}
	for _, c := range calls {
		stub.gotMD = nil
		if err := c.call(); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got := mdValue(stub.gotMD, "x-request-id"); got != "rid" {
			t.Fatalf("%s: x-request-id = %q, want %q", c.name, got, "rid")
		}
		if got := mdValue(stub.gotMD, "x-tenant-id"); got != "ten" {
			t.Fatalf("%s: x-tenant-id = %q, want %q", c.name, got, "ten")
		}
	}
}

// TestCallWithoutCorrelationSendsNoHeaders asserts an id-less context sends no
// correlation headers, so audio-service falls back to generating its own id.
func TestCallWithoutCorrelationSendsNoHeaders(t *testing.T) {
	stub := &stubServer{probeResp: &audiov1.ProbeResponse{}}
	client := newClient(t, serveStub(t, stub))

	if _, err := client.Probe(t.Context(), audio.ProbeOptions{}); err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if got := mdValue(stub.gotMD, "x-request-id"); got != "" {
		t.Fatalf("x-request-id = %q, want none", got)
	}
	if got := mdValue(stub.gotMD, "x-tenant-id"); got != "" {
		t.Fatalf("x-tenant-id = %q, want none", got)
	}
}

func TestDisabledClientReportsErrDisabled(t *testing.T) {
	c, err := audio.New(audio.Config{Addr: ""})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if c != nil {
		t.Fatalf("expected a nil client when Addr is empty")
	}
	if _, err := c.Probe(t.Context(), audio.ProbeOptions{}); !errors.Is(err, audio.ErrDisabled) {
		t.Fatalf("Probe: expected ErrDisabled, got %v", err)
	}
	if _, err := c.Normalize(t.Context(), audio.NormalizeOptions{}); !errors.Is(err, audio.ErrDisabled) {
		t.Fatalf("Normalize: expected ErrDisabled, got %v", err)
	}
	if _, err := c.TranscribeSegment(t.Context(), audio.TranscribeSegmentOptions{}); !errors.Is(err, audio.ErrDisabled) {
		t.Fatalf("TranscribeSegment: expected ErrDisabled, got %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close on nil client: %v", err)
	}
}
