// Package video is a thin gRPC client for the video-service (CON-148).
// It owns the connection, the raised receive limit, and the per-call deadline,
// and presents video probing as a single unary Probe call.
//
// Unlike pdf — which client-streams the file bytes — video hands
// video-service a short-lived presigned GET URL and lets ffprobe range-read
// only what it needs. That keeps the (potentially multi-GB) video out of the
// API process entirely (CON-148 §5.2).
//
// The link to video-service is private-network-only (Railway), so the channel
// is plaintext h2c — no TLS between the two.
package video

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	grpcstatus "google.golang.org/grpc/status"

	videov1 "github.com/ogen-app/ogen/gen/video/v1"
)

const defaultTimeout = 2 * time.Minute

// ErrDisabled is returned when Probe is called on a disabled (nil) client —
// i.e. VIDEO_SERVICE_ADDR was not configured.
var ErrDisabled = errors.New("video: disabled (VIDEO_SERVICE_ADDR not set)")

// IsInvalidVideo reports whether err is the service's terminal "not a usable
// video" verdict — gRPC InvalidArgument: the input is corrupt, truncated, or
// not a decodable video, and will never probe, so it must not be retried.
// Transport/internal failures (Unavailable, DeadlineExceeded, Internal) are
// transient and return false, so callers can degrade gracefully rather than
// reject the upload — mirrors pdf.IsInvalidPDF.
func IsInvalidVideo(err error) bool {
	st, ok := grpcstatus.FromError(err)
	return ok && st.Code() == codes.InvalidArgument
}

// ProbeOptions controls a single Probe call.
type ProbeOptions struct {
	// SourceURL is a short-lived presigned GET URL to the uploaded object.
	SourceURL string
	// RenderPoster also captures a representative frame as PNG.
	RenderPoster bool
	// Filename is the client-declared name, for logging / container hints.
	Filename string
}

// ProbeResult is the metadata video-service extracted from a video, plus an
// optional poster frame. Zero fields mean "unknown" (e.g. a container ffprobe
// couldn't read a duration from).
type ProbeResult struct {
	DurationMs int64
	Codec      string
	Container  string
	Width      int
	Height     int
	Bitrate    int64
	PosterPNG  []byte
}

// Config wires a Client.
type Config struct {
	Addr         string        // gRPC target (private-network host:port); empty disables
	Timeout      time.Duration // per-Probe deadline; <=0 uses the default
	MaxRecvBytes int           // client max-recv; <=0 leaves the gRPC default
}

// Client is a thin gRPC client for the video-service, safe for concurrent use.
// A nil *Client is the "disabled" sentinel (no VIDEO_SERVICE_ADDR) and every
// method fails cleanly with ErrDisabled.
type Client struct {
	conn    *grpc.ClientConn
	rpc     videov1.VideoServiceClient
	timeout time.Duration
}

// New dials the video-service. When cfg.Addr is empty it returns (nil, nil): a
// nil client that reports ErrDisabled, mirroring how an absent pdf
// disables PDF features. grpc.NewClient connects lazily, so this never blocks
// on the service being up.
func New(cfg Config) (*Client, error) {
	if cfg.Addr == "" {
		return nil, nil
	}
	// The correlation interceptors copy request_id/tenant_id from the call
	// context into outgoing gRPC metadata so video-service's logs join the
	// API's, exactly as pdf does (CON-111).
	dialOpts := []grpc.DialOption{
		// CON-303: emit a client span per RPC and propagate the trace to the service.
		grpc.WithStatsHandler(otelgrpc.NewClientHandler()),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithChainStreamInterceptor(correlationStreamInterceptor),
		grpc.WithChainUnaryInterceptor(correlationUnaryInterceptor),
	}
	if cfg.MaxRecvBytes > 0 {
		dialOpts = append(dialOpts, grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(cfg.MaxRecvBytes)))
	}
	conn, err := grpc.NewClient(cfg.Addr, dialOpts...)
	if err != nil {
		return nil, fmt.Errorf("video: dial %q: %w", cfg.Addr, err)
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	return &Client{conn: conn, rpc: videov1.NewVideoServiceClient(conn), timeout: timeout}, nil
}

// Close releases the underlying connection. Safe on a nil client.
func (c *Client) Close() error {
	if c == nil || c.conn == nil {
		return nil
	}
	return c.conn.Close()
}

// Probe asks the service to read the video at opts.SourceURL and return its
// metadata (and optionally a poster frame). The caller's ctx is further
// bounded by the configured per-call timeout.
func (c *Client) Probe(ctx context.Context, opts ProbeOptions) (*ProbeResult, error) {
	if c == nil {
		return nil, ErrDisabled
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	resp, err := c.rpc.Probe(ctx, &videov1.ProbeRequest{
		SourceUrl:    opts.SourceURL,
		RenderPoster: opts.RenderPoster,
		Filename:     opts.Filename,
	})
	if err != nil {
		return nil, fmt.Errorf("video: probe: %w", err)
	}
	return &ProbeResult{
		DurationMs: resp.GetDurationMs(),
		Codec:      resp.GetCodec(),
		Container:  resp.GetContainer(),
		Width:      int(resp.GetWidth()),
		Height:     int(resp.GetHeight()),
		Bitrate:    resp.GetBitrate(),
		PosterPNG:  resp.GetPosterPng(),
	}, nil
}
