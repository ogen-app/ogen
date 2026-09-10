// Package documents is a thin gRPC client for the document-service (CON-280). It
// owns the connection, the raised receive limit, and the per-call deadline, and
// presents office/text-document parsing as a single Parse call that
// client-streams the bytes and returns embedding-ready, source-anchored chunks.
//
// It is the direct sibling of the pdf client, minus rendering (no thumbnails).
// The link to document-service is private-network-only (Railway), so the channel
// is plaintext h2c — no TLS between the two.
package documents

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

	documentsv1 "github.com/ogen-app/ogen/gen/documents/v1"
)

// streamFrameSize bounds each document byte frame sent over the stream (1 MiB),
// well under gRPC's default 4 MiB message cap, so a large document streams in
// many frames.
const streamFrameSize = 1 << 20

// defaultTimeout matches the DOCUMENTS_SERVICE_TIMEOUT default; a large
// spreadsheet can explode into many labelled row chunks, so it is longer than
// the pdf client's.
const defaultTimeout = 5 * time.Minute

// ErrDisabled is returned when Parse is called on a disabled (nil) client —
// i.e. DOCUMENTS_SERVICE_ADDR was not configured.
var ErrDisabled = errors.New("documents: disabled (DOCUMENTS_SERVICE_ADDR not set)")

// IsInvalidDocument reports whether err is the service's terminal "not a usable
// document" verdict — gRPC InvalidArgument or FailedPrecondition: the input is
// corrupt, encrypted, empty, or over the size cap, and will never parse, so it
// must not be retried.
func IsInvalidDocument(err error) bool {
	st, ok := grpcstatus.FromError(err)
	if !ok {
		return false
	}
	switch st.Code() {
	case codes.InvalidArgument, codes.FailedPrecondition:
		return true
	default:
		return false
	}
}

// IsUnsupportedDocument reports whether err is the service's terminal "format
// not supported" verdict — gRPC Unimplemented: a legacy OLE2 binary
// (.doc/.xls/.ppt), an Apple iWork file, or an unrecognised magic number. The
// file must be converted before it can be ingested, so this must not be retried.
func IsUnsupportedDocument(err error) bool {
	st, ok := grpcstatus.FromError(err)
	return ok && st.Code() == codes.Unimplemented
}

// Options controls a single Parse call. Zero values mean "service default".
type Options struct {
	Filename          string
	ContentType       string
	ChunkTargetChars  int
	ChunkOverlapChars int
	ChunkMaxChars     int
}

// Anchor is the structured source location of a chunk, mirroring the proto
// Anchor. Kind is the lowercase citation kind ("page"/"slide"/"sheet"/
// "section"/"email", or "" when unspecified); the remaining fields are populated
// per kind (zero/empty when not applicable).
type Anchor struct {
	Kind        string
	PageStart   int
	PageEnd     int
	Slide       int
	Sheet       string
	CellRange   string
	HeadingPath []string
}

// Chunk is a source-anchored, embed-ready text chunk returned by the service.
// Text already carries any breadcrumb / sheet-label prefix.
type Chunk struct {
	Index       int
	Text        string
	SourceLabel string
	Anchor      Anchor
	TokenCount  int
}

// Result is the parsed output of a document.
type Result struct {
	Format string
	Chunks []Chunk
}

// Config wires a Client.
type Config struct {
	Addr         string        // gRPC target (private-network host:port); empty disables
	Timeout      time.Duration // per-Parse deadline; <=0 uses the default
	MaxRecvBytes int           // client max-recv; <=0 leaves the gRPC default
}

// Client is a thin gRPC client for the document-service, safe for concurrent
// use. A nil *Client is the "disabled" sentinel (no DOCUMENTS_SERVICE_ADDR) and
// every method fails cleanly with ErrDisabled.
type Client struct {
	conn    *grpc.ClientConn
	rpc     documentsv1.DocumentsServiceClient
	timeout time.Duration
}

// New dials the document-service. When cfg.Addr is empty it returns (nil, nil):
// a nil client that reports ErrDisabled, mirroring the pdf client and how an
// absent embedder disables semantic features. grpc.NewClient connects lazily, so
// this never blocks on the service being up.
func New(cfg Config) (*Client, error) {
	if cfg.Addr == "" {
		return nil, nil
	}
	// The correlation interceptors copy request_id/tenant_id from the call
	// context into outgoing gRPC metadata so document-service's logs join the
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
		return nil, fmt.Errorf("documents: dial %q: %w", cfg.Addr, err)
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	return &Client{conn: conn, rpc: documentsv1.NewDocumentsServiceClient(conn), timeout: timeout}, nil
}

// Close releases the underlying connection. Safe on a nil client.
func (c *Client) Close() error {
	if c == nil || c.conn == nil {
		return nil
	}
	return c.conn.Close()
}

// Parse streams the document in r to the service and returns the extracted,
// source-anchored chunks. The caller's ctx is further bounded by the configured
// per-call timeout.
func (c *Client) Parse(ctx context.Context, r io.Reader, opts Options) (*Result, error) {
	if c == nil {
		return nil, ErrDisabled
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	stream, err := c.rpc.Parse(ctx)
	if err != nil {
		return nil, fmt.Errorf("documents: open stream: %w", err)
	}

	// First frame carries the options.
	if err := stream.Send(&documentsv1.ParseRequest{Payload: &documentsv1.ParseRequest_Options{Options: &documentsv1.ParseOptions{
		Filename:          opts.Filename,
		ContentType:       opts.ContentType,
		ChunkTargetChars:  int32(opts.ChunkTargetChars),
		ChunkOverlapChars: int32(opts.ChunkOverlapChars),
		ChunkMaxChars:     int32(opts.ChunkMaxChars),
	}}}); err != nil {
		return nil, fmt.Errorf("documents: send options: %w", err)
	}

	// Subsequent frames carry the document bytes.
	buf := make([]byte, streamFrameSize)
	for {
		n, rerr := r.Read(buf)
		if n > 0 {
			if err := stream.Send(&documentsv1.ParseRequest{Payload: &documentsv1.ParseRequest_Chunk{Chunk: buf[:n]}}); err != nil {
				return nil, fmt.Errorf("documents: send document bytes: %w", err)
			}
		}
		if errors.Is(rerr, io.EOF) {
			break
		}
		if rerr != nil {
			return nil, fmt.Errorf("documents: read document: %w", rerr)
		}
	}

	resp, err := stream.CloseAndRecv()
	if err != nil {
		return nil, fmt.Errorf("documents: parse: %w", err)
	}

	out := &Result{Format: resp.GetFormat()}
	out.Chunks = make([]Chunk, 0, len(resp.GetChunks()))
	for _, ch := range resp.GetChunks() {
		out.Chunks = append(out.Chunks, Chunk{
			Index:       int(ch.GetIndex()),
			Text:        ch.GetText(),
			SourceLabel: ch.GetSourceLabel(),
			Anchor:      toAnchor(ch.GetAnchor()),
			TokenCount:  int(ch.GetTokenCount()),
		})
	}
	return out, nil
}

// toAnchor maps the proto Anchor to the client's Anchor, collapsing the enum to
// a lowercase kind string the persistence layer stores verbatim.
func toAnchor(a *documentsv1.Anchor) Anchor {
	if a == nil {
		return Anchor{}
	}
	return Anchor{
		Kind:        anchorKind(a.GetKind()),
		PageStart:   int(a.GetPageStart()),
		PageEnd:     int(a.GetPageEnd()),
		Slide:       int(a.GetSlide()),
		Sheet:       a.GetSheet(),
		CellRange:   a.GetCellRange(),
		HeadingPath: a.GetHeadingPath(),
	}
}

// anchorKind renders the proto AnchorKind as the lowercase token persisted in
// assets_chunks.source_anchor (models.SourceAnchor.Kind). Unspecified -> "".
func anchorKind(k documentsv1.AnchorKind) string {
	switch k {
	case documentsv1.AnchorKind_ANCHOR_KIND_PAGE:
		return "page"
	case documentsv1.AnchorKind_ANCHOR_KIND_SLIDE:
		return "slide"
	case documentsv1.AnchorKind_ANCHOR_KIND_SHEET:
		return "sheet"
	case documentsv1.AnchorKind_ANCHOR_KIND_SECTION:
		return "section"
	case documentsv1.AnchorKind_ANCHOR_KIND_EMAIL:
		return "email"
	default:
		return ""
	}
}
