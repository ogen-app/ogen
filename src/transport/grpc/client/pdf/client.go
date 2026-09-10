// Package pdf is a thin gRPC client for the pdf-service (CON-103). It owns
// the connection, the raised receive limit, and the per-call deadline, and
// presents PDF parsing as a single Parse call that client-streams the bytes.
//
// The link to pdf-service is private-network-only (Railway), so the channel is
// plaintext h2c — no TLS between the two.
package pdf

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	grpcstatus "google.golang.org/grpc/status"

	pdfv1 "github.com/ogen-app/ogen/gen/pdf/v1"
)

// streamFrameSize bounds each PDF byte frame sent over the stream (1 MiB), well
// under gRPC's default 4 MiB message cap, so a 50 MB PDF streams in many frames.
const streamFrameSize = 1 << 20

const defaultTimeout = 2 * time.Minute

// ErrDisabled is returned when Parse is called on a disabled (nil) client —
// i.e. PDF_SERVICE_ADDR was not configured.
var ErrDisabled = errors.New("pdf: disabled (PDF_SERVICE_ADDR not set)")

// IsInvalidPDF reports whether err is the service's terminal "not a usable PDF"
// verdict — gRPC InvalidArgument: the input is corrupt, encrypted, or not a
// PDF, and will never parse, so it must not be retried. Transport/internal
// failures (Unavailable, DeadlineExceeded, Internal) are transient and return
// false, so callers can degrade gracefully rather than reject the upload.
func IsInvalidPDF(err error) bool {
	st, ok := grpcstatus.FromError(err)
	return ok && st.Code() == codes.InvalidArgument
}

// Options controls a single Parse call. Zero values mean "service default".
type Options struct {
	Filename          string
	RenderThumbnail   bool
	ThumbnailDPI      int
	ChunkTargetChars  int
	ChunkOverlapChars int
	ChunkMaxChars     int
}

// Chunk is a page-attributed text chunk returned by the service.
type Chunk struct {
	Index     int
	Text      string
	PageStart int
	PageEnd   int
}

// Result is the parsed output of a PDF.
type Result struct {
	PageCount    int
	Chunks       []Chunk
	ThumbnailPNG []byte
}

// Config wires a Client.
type Config struct {
	Addr         string        // gRPC target (private-network host:port); empty disables
	Timeout      time.Duration // per-Parse deadline; <=0 uses the default
	MaxRecvBytes int           // client max-recv; <=0 leaves the gRPC default
}

// Client is a thin gRPC client for the pdf-service, safe for concurrent use. A
// nil *Client is the "disabled" sentinel (no PDF_SERVICE_ADDR) and every method
// fails cleanly with ErrDisabled.
type Client struct {
	conn    *grpc.ClientConn
	rpc     pdfv1.PdfServiceClient
	timeout time.Duration
}

// New dials the pdf-service. When cfg.Addr is empty it returns (nil, nil): a nil
// client that reports ErrDisabled, mirroring how an absent embedder disables
// semantic features. grpc.NewClient connects lazily, so this never blocks on the
// service being up.
func New(cfg Config) (*Client, error) {
	if cfg.Addr == "" {
		return nil, nil
	}
	// The correlation interceptors copy request_id/tenant_id from the call
	// context into outgoing gRPC metadata so pdf-service's logs join the API's
	// (CON-111).
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
		return nil, fmt.Errorf("pdf: dial %q: %w", cfg.Addr, err)
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	return &Client{conn: conn, rpc: pdfv1.NewPdfServiceClient(conn), timeout: timeout}, nil
}

// Close releases the underlying connection. Safe on a nil client.
func (c *Client) Close() error {
	if c == nil || c.conn == nil {
		return nil
	}
	return c.conn.Close()
}

// Parse streams the PDF in r to the service and returns the parsed result. The
// caller's ctx is further bounded by the configured per-call timeout.
func (c *Client) Parse(ctx context.Context, r io.Reader, opts Options) (*Result, error) {
	if c == nil {
		return nil, ErrDisabled
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	stream, err := c.rpc.Parse(ctx)
	if err != nil {
		return nil, fmt.Errorf("pdf: open stream: %w", err)
	}

	// First frame carries the options.
	if err := stream.Send(&pdfv1.ParseRequest{Payload: &pdfv1.ParseRequest_Options{Options: &pdfv1.ParseOptions{
		Filename:          opts.Filename,
		RenderThumbnail:   opts.RenderThumbnail,
		ThumbnailDpi:      int32(opts.ThumbnailDPI),
		ChunkTargetChars:  int32(opts.ChunkTargetChars),
		ChunkOverlapChars: int32(opts.ChunkOverlapChars),
		ChunkMaxChars:     int32(opts.ChunkMaxChars),
	}}}); err != nil {
		return nil, fmt.Errorf("pdf: send options: %w", err)
	}

	// Subsequent frames carry the PDF bytes.
	buf := make([]byte, streamFrameSize)
	for {
		n, rerr := r.Read(buf)
		if n > 0 {
			if err := stream.Send(&pdfv1.ParseRequest{Payload: &pdfv1.ParseRequest_Chunk{Chunk: buf[:n]}}); err != nil {
				return nil, fmt.Errorf("pdf: send pdf bytes: %w", err)
			}
		}
		if errors.Is(rerr, io.EOF) {
			break
		}
		if rerr != nil {
			return nil, fmt.Errorf("pdf: read pdf: %w", rerr)
		}
	}

	resp, err := stream.CloseAndRecv()
	if err != nil {
		return nil, fmt.Errorf("pdf: parse: %w", err)
	}

	out := &Result{PageCount: int(resp.GetPageCount()), ThumbnailPNG: resp.GetThumbnailPng()}
	out.Chunks = make([]Chunk, 0, len(resp.GetChunks()))
	for _, ch := range resp.GetChunks() {
		out.Chunks = append(out.Chunks, Chunk{
			Index:     int(ch.GetIndex()),
			Text:      ch.GetText(),
			PageStart: int(ch.GetPageStart()),
			PageEnd:   int(ch.GetPageEnd()),
		})
	}
	return out, nil
}

// RenderOptions controls a Render call.
type RenderOptions struct {
	RenderThumbnail bool
	ThumbnailDPI    int
}

// RenderResult is a PDF's page count and optional first-page thumbnail.
type RenderResult struct {
	PageCount    int
	ThumbnailPNG []byte
}

// Render streams the PDF in r to the service and returns its page count and
// (optionally) a first-page thumbnail — no text extraction or chunking. Used
// for post attachments.
func (c *Client) Render(ctx context.Context, r io.Reader, opts RenderOptions) (*RenderResult, error) {
	if c == nil {
		return nil, ErrDisabled
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	stream, err := c.rpc.Render(ctx)
	if err != nil {
		return nil, fmt.Errorf("pdf: open render stream: %w", err)
	}

	if err := stream.Send(&pdfv1.RenderRequest{Payload: &pdfv1.RenderRequest_Options{Options: &pdfv1.RenderOptions{
		RenderThumbnail: opts.RenderThumbnail,
		ThumbnailDpi:    int32(opts.ThumbnailDPI),
	}}}); err != nil {
		return nil, fmt.Errorf("pdf: send render options: %w", err)
	}

	buf := make([]byte, streamFrameSize)
	for {
		n, rerr := r.Read(buf)
		if n > 0 {
			if err := stream.Send(&pdfv1.RenderRequest{Payload: &pdfv1.RenderRequest_Chunk{Chunk: buf[:n]}}); err != nil {
				return nil, fmt.Errorf("pdf: send pdf bytes: %w", err)
			}
		}
		if errors.Is(rerr, io.EOF) {
			break
		}
		if rerr != nil {
			return nil, fmt.Errorf("pdf: read pdf: %w", rerr)
		}
	}

	resp, err := stream.CloseAndRecv()
	if err != nil {
		return nil, fmt.Errorf("pdf: render: %w", err)
	}
	return &RenderResult{PageCount: int(resp.GetPageCount()), ThumbnailPNG: resp.GetThumbnailPng()}, nil
}
