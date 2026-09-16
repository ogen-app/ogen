// Package image is a thin gRPC client for the image-service (CON-281): the
// single authority for every image in the platform. It owns the connection, the
// raised receive limit, and per-call deadlines, and presents image compute as
// three narrow unary calls: Extract (full content-bank pipeline), PrepareAttachment
// (light post-attachment path), and GenerateAltText.
//
// Like audio/video — and unlike pdf/documents which client-stream the file bytes
// — image hands the service short-lived presigned URLs (GET for the source, PUT
// for a normalized/cleaned derivative) so the (potentially large) image bytes
// never traverse the API process. The service is stateless; ogen owns the DB
// rows, the River job, and the HTTP surface.
//
// The link to image-service is private-network-only (Railway), so the channel is
// plaintext h2c — no TLS between the two. A nil *Client is the "disabled"
// sentinel (no IMAGE_SERVICE_ADDR); every method fails cleanly with ErrDisabled.
// Because imageprobe was deleted (D6), that disabled state means image uploads
// are rejected rather than degraded — there is no pure-Go fallback.
package image

import (
	"context"
	"errors"
	"fmt"
	"time"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	grpcstatus "google.golang.org/grpc/status"

	documentsv1 "github.com/ogen-app/ogen/gen/documents/v1"
	imagev1 "github.com/ogen-app/ogen/gen/image/v1"
)

// rejectInfoDomain is the google.rpc.ErrorInfo Domain image-service stamps on a
// terminal reject; it MUST match the service's constant (image.v1). The Reason
// beside it is an image.v1.RejectedCode enum name.
const rejectInfoDomain = "image.v1"

const defaultTimeout = 3 * time.Minute

// ErrDisabled is returned when a call is made on a disabled (nil) client — i.e.
// IMAGE_SERVICE_ADDR was not configured.
var ErrDisabled = errors.New("image: disabled (IMAGE_SERVICE_ADDR not set)")

// IsInvalidImage reports whether err is the service's terminal "not a usable
// image" verdict — gRPC InvalidArgument or FailedPrecondition: the input is
// corrupt, truncated, zero-length, a vector (SVG), or over the pixel-area/size
// ceiling, and will never process, so it must not be retried. Transport/internal
// failures (Unavailable, DeadlineExceeded, Internal) are transient and return
// false so the job/handler retries them.
func IsInvalidImage(err error) bool {
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

// IsUnsupportedImage reports whether err is the service's terminal "format not
// supported" verdict — gRPC Unimplemented: a container/codec the service can't
// decode. Must not be retried.
func IsUnsupportedImage(err error) bool {
	st, ok := grpcstatus.FromError(err)
	return ok && st.Code() == codes.Unimplemented
}

// RejectedReason extracts the machine-readable reject reason image-service
// attaches to a terminal reject as a google.rpc.ErrorInfo detail (CON-281): the
// image.v1.RejectedCode enum NAME, e.g. "REJECTED_CODE_VECTOR". It returns "" when
// err carries no such detail — an older service, or a non-reject error — so the
// caller keeps its coarse IsInvalid/IsUnsupported fallback. The full human
// sentence remains available as the status message.
func RejectedReason(err error) string {
	st, ok := grpcstatus.FromError(err)
	// FromError(nil) returns (nil, true), so guard st explicitly: a nil error (and
	// a non-status error) carries no reject reason.
	if !ok || st == nil {
		return ""
	}
	for _, d := range st.Details() {
		if info, ok := d.(*errdetails.ErrorInfo); ok && info.GetDomain() == rejectInfoDomain {
			return info.GetReason()
		}
	}
	return ""
}

// TokenUsage is one Gemini vision call's token count, priced by ogen via the
// existing gemini vendor (CON-86). Step is the pipeline stage.
type TokenUsage struct {
	Model  string
	Step   string
	Input  int64
	Output int64
}

// Bbox is a normalized [0,1] rectangle on the source image.
type Bbox struct{ X, Y, W, H float64 }

// Anchor is an image block's location: Kind == "image", Bbox the region.
type Anchor struct {
	Kind string
	Bbox *Bbox
}

// Cell is one table-block grid cell.
type Cell struct {
	Row, Col int
	Text     string
}

// Block mirrors the documents-service Block shape (kind/level/text/cells/anchor)
// plus image provenance.
type Block struct {
	Kind          string
	Level         int
	Text          string
	Cells         []Cell
	Anchor        Anchor
	Provenance    string
	LowConfidence bool
}

// NormalizedMeta is the normalized derivative's metadata.
type NormalizedMeta struct {
	Mime           string
	Width          int
	Height         int
	IsAnimated     bool
	ChecksumSHA256 string
}

// ExtractOptions controls a single Extract call (full content-bank pipeline).
type ExtractOptions struct {
	SourceURL           string // presigned GET (original or a prior normalized derivative)
	DestPutURL          string // presigned PUT (normalized.png)
	SourceNormalized    bool
	Filename            string
	ClassifyModel       string
	ExtractModel        string
	EscalateModel       string
	AltTextMaxChars     int
	ConfidenceThreshold float64
}

// ExtractResult is the full pipeline output: the classified shape, structured
// blocks, description + alt text, the normalized derivative's metadata, quality
// signals, and per-call token usage. RejectedReason (non-empty) accompanies a
// terminal InvalidArgument.
type ExtractResult struct {
	Shape              string // "prose"|"conversation"|"social_post"|"tabular"|"creative"|""
	ClassifyConfidence float64
	Blocks             []Block
	Description        string
	AltText            string
	Normalized         NormalizedMeta
	Escalated          bool
	EscalationImproved bool
	DescriptionOK      bool
	ExtractionOK       bool
	Truncated          bool
	Usage              []TokenUsage
	RejectedReason     string
}

// PrepareAttachmentOptions controls a single PrepareAttachment call (light path:
// validate + metadata + EXIF-strip with pixels preserved + optional alt text).
type PrepareAttachmentOptions struct {
	SourceURL       string // presigned GET (uploaded original)
	DestPutURL      string // presigned PUT (EXIF-stripped, pixel-identical copy)
	StripMetadata   bool
	WantAltText     bool
	AltTextMaxChars int
	AltTextModel    string
	Filename        string
}

// PrepareAttachmentResult carries the cleaned derivative's metadata (of the
// EXIF-stripped copy), the ORIGINAL bytes' checksum (dedupe), optional alt text,
// and token usage for any alt-text call.
type PrepareAttachmentResult struct {
	Mime           string
	SizeBytes      int64
	Width          int
	Height         int
	IsAnimated     bool
	FrameCount     int
	ChecksumSHA256 string
	AltText        string
	Usage          []TokenUsage
	RejectedReason string
}

// GenerateAltTextOptions controls a single GenerateAltText call.
type GenerateAltTextOptions struct {
	SourceURL string
	MaxChars  int
	Model     string
}

// GenerateAltTextResult carries the generated alt text + token usage.
type GenerateAltTextResult struct {
	AltText string
	Usage   []TokenUsage
}

// Config wires a Client.
type Config struct {
	Addr         string        // gRPC target (private-network host:port); empty disables
	Timeout      time.Duration // per-call deadline; <=0 uses the default
	MaxRecvBytes int           // client max-recv; <=0 leaves the gRPC default
}

// Client is a thin gRPC client for the image-service, safe for concurrent use.
type Client struct {
	conn    *grpc.ClientConn
	rpc     imagev1.ImageServiceClient
	timeout time.Duration
}

// New dials the image-service. When cfg.Addr is empty it returns (nil, nil): a
// nil client that reports ErrDisabled, mirroring the audio/documents clients.
// grpc.NewClient connects lazily, so this never blocks on the service being up.
func New(cfg Config) (*Client, error) {
	if cfg.Addr == "" {
		return nil, nil
	}
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
		return nil, fmt.Errorf("image: dial %q: %w", cfg.Addr, err)
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	return &Client{conn: conn, rpc: imagev1.NewImageServiceClient(conn), timeout: timeout}, nil
}

// Close releases the underlying connection. Safe on a nil client.
func (c *Client) Close() error {
	if c == nil || c.conn == nil {
		return nil
	}
	return c.conn.Close()
}

// Extract runs the full content-bank pipeline on the image at opts.SourceURL,
// writing the normalized derivative to opts.DestPutURL.
func (c *Client) Extract(ctx context.Context, opts ExtractOptions) (*ExtractResult, error) {
	if c == nil {
		return nil, ErrDisabled
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	resp, err := c.rpc.Extract(ctx, &imagev1.ExtractRequest{
		SourceUrl:           opts.SourceURL,
		DestPutUrl:          opts.DestPutURL,
		SourceNormalized:    opts.SourceNormalized,
		Filename:            opts.Filename,
		ClassifyModel:       opts.ClassifyModel,
		ExtractModel:        opts.ExtractModel,
		EscalateModel:       opts.EscalateModel,
		AltTextMaxChars:     int32(opts.AltTextMaxChars),
		ConfidenceThreshold: float32(opts.ConfidenceThreshold),
	})
	if err != nil {
		return nil, fmt.Errorf("image: extract: %w", err)
	}
	out := &ExtractResult{
		Shape:              shapeString(resp.GetShape()),
		ClassifyConfidence: float64(resp.GetClassifyConfidence()),
		Description:        resp.GetDescription(),
		AltText:            resp.GetAltText(),
		Escalated:          resp.GetEscalated(),
		EscalationImproved: resp.GetEscalationImproved(),
		DescriptionOK:      resp.GetDescriptionOk(),
		ExtractionOK:       resp.GetExtractionOk(),
		Truncated:          resp.GetTruncated(),
		RejectedReason:     resp.GetRejectedReason(),
		Usage:              toUsage(resp.GetUsage()),
	}
	if n := resp.GetNormalized(); n != nil {
		out.Normalized = NormalizedMeta{
			Mime:           n.GetMime(),
			Width:          int(n.GetWidth()),
			Height:         int(n.GetHeight()),
			IsAnimated:     n.GetIsAnimated(),
			ChecksumSHA256: n.GetChecksumSha256(),
		}
	}
	out.Blocks = make([]Block, 0, len(resp.GetBlocks()))
	for _, b := range resp.GetBlocks() {
		out.Blocks = append(out.Blocks, toBlock(b))
	}
	return out, nil
}

// PrepareAttachment runs the light path on the image at opts.SourceURL, writing
// the EXIF-stripped (pixel-identical) copy to opts.DestPutURL.
func (c *Client) PrepareAttachment(ctx context.Context, opts PrepareAttachmentOptions) (*PrepareAttachmentResult, error) {
	if c == nil {
		return nil, ErrDisabled
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	resp, err := c.rpc.PrepareAttachment(ctx, &imagev1.PrepareAttachmentRequest{
		SourceUrl:       opts.SourceURL,
		DestPutUrl:      opts.DestPutURL,
		StripMetadata:   opts.StripMetadata,
		WantAltText:     opts.WantAltText,
		AltTextMaxChars: int32(opts.AltTextMaxChars),
		AltTextModel:    opts.AltTextModel,
		Filename:        opts.Filename,
	})
	if err != nil {
		return nil, fmt.Errorf("image: prepare attachment: %w", err)
	}
	return &PrepareAttachmentResult{
		Mime:           resp.GetMime(),
		SizeBytes:      resp.GetSizeBytes(),
		Width:          int(resp.GetWidth()),
		Height:         int(resp.GetHeight()),
		IsAnimated:     resp.GetIsAnimated(),
		FrameCount:     int(resp.GetFrameCount()),
		ChecksumSHA256: resp.GetChecksumSha256(),
		AltText:        resp.GetAltText(),
		Usage:          toUsage(resp.GetUsage()),
		RejectedReason: resp.GetRejectedReason(),
	}, nil
}

// GenerateAltText re-runs only alt-text generation for the image at
// opts.SourceURL.
func (c *Client) GenerateAltText(ctx context.Context, opts GenerateAltTextOptions) (*GenerateAltTextResult, error) {
	if c == nil {
		return nil, ErrDisabled
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	resp, err := c.rpc.GenerateAltText(ctx, &imagev1.GenerateAltTextRequest{
		SourceUrl: opts.SourceURL,
		MaxChars:  int32(opts.MaxChars),
		Model:     opts.Model,
	})
	if err != nil {
		return nil, fmt.Errorf("image: generate alt text: %w", err)
	}
	return &GenerateAltTextResult{AltText: resp.GetAltText(), Usage: toUsage(resp.GetUsage())}, nil
}

func toUsage(in []*imagev1.TokenUsage) []TokenUsage {
	if len(in) == 0 {
		return nil
	}
	out := make([]TokenUsage, 0, len(in))
	for _, u := range in {
		out = append(out, TokenUsage{
			Model:  u.GetModel(),
			Step:   u.GetStep(),
			Input:  u.GetInput(),
			Output: u.GetOutput(),
		})
	}
	return out
}

func toBlock(b *imagev1.Block) Block {
	out := Block{
		Kind:          b.GetKind(),
		Level:         int(b.GetLevel()),
		Text:          b.GetText(),
		Provenance:    b.GetProvenance(),
		LowConfidence: b.GetLowConfidence(),
	}
	if cells := b.GetCells(); len(cells) > 0 {
		out.Cells = make([]Cell, 0, len(cells))
		for _, c := range cells {
			out.Cells = append(out.Cells, Cell{Row: int(c.GetRow()), Col: int(c.GetCol()), Text: c.GetText()})
		}
	}
	// Block carries the SHARED documents.v1.Anchor (CON-280/281): collapse its
	// kind enum to the lowercase token the persistence layer stores, and lift the
	// normalized bbox.
	if a := b.GetAnchor(); a != nil {
		out.Anchor.Kind = anchorKind(a.GetKind())
		if bb := a.GetBbox(); bb != nil {
			out.Anchor.Bbox = &Bbox{X: float64(bb.GetX()), Y: float64(bb.GetY()), W: float64(bb.GetW()), H: float64(bb.GetH())}
		}
	}
	return out
}

// anchorKind renders the shared proto AnchorKind as the lowercase token persisted
// in assets_chunks.source_anchor / image_blocks.anchor. Image blocks are always
// ANCHOR_KIND_IMAGE_REGION -> "image"; anything else collapses to "".
func anchorKind(k documentsv1.AnchorKind) string {
	if k == documentsv1.AnchorKind_ANCHOR_KIND_IMAGE_REGION {
		return "image"
	}
	return ""
}

// shapeString renders the proto Shape enum as the lowercase token persisted in
// image_extractions.shape (models.ImageShape*). Unspecified -> "".
func shapeString(s imagev1.Shape) string {
	switch s {
	case imagev1.Shape_SHAPE_PROSE:
		return "prose"
	case imagev1.Shape_SHAPE_CONVERSATION:
		return "conversation"
	case imagev1.Shape_SHAPE_SOCIAL_POST:
		return "social_post"
	case imagev1.Shape_SHAPE_TABULAR:
		return "tabular"
	case imagev1.Shape_SHAPE_CREATIVE:
		return "creative"
	default:
		return ""
	}
}
