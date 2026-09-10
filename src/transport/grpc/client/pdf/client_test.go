package pdf_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	pdfv1 "github.com/ogen-app/ogen/gen/pdf/v1"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
	"github.com/ogen-app/ogen/src/transport/grpc/client/pdf"
)

// stubServer records the streamed request and returns a canned response.
type stubServer struct {
	pdfv1.UnimplementedPdfServiceServer
	gotOptions *pdfv1.ParseOptions
	gotBytes   []byte
	gotMD      metadata.MD // inbound gRPC metadata seen on the Parse stream
	resp       *pdfv1.ParseResponse

	gotRenderOpts  *pdfv1.RenderOptions
	gotRenderBytes []byte
	renderResp     *pdfv1.RenderResponse
}

func (s *stubServer) Render(stream grpc.ClientStreamingServer[pdfv1.RenderRequest, pdfv1.RenderResponse]) error {
	for {
		req, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		switch p := req.Payload.(type) {
		case *pdfv1.RenderRequest_Options:
			s.gotRenderOpts = p.Options
		case *pdfv1.RenderRequest_Chunk:
			s.gotRenderBytes = append(s.gotRenderBytes, p.Chunk...)
		}
	}
	return stream.SendAndClose(s.renderResp)
}

func (s *stubServer) Parse(stream grpc.ClientStreamingServer[pdfv1.ParseRequest, pdfv1.ParseResponse]) error {
	s.gotMD, _ = metadata.FromIncomingContext(stream.Context())
	for {
		req, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		switch p := req.Payload.(type) {
		case *pdfv1.ParseRequest_Options:
			s.gotOptions = p.Options
		case *pdfv1.ParseRequest_Chunk:
			s.gotBytes = append(s.gotBytes, p.Chunk...)
		}
	}
	return stream.SendAndClose(s.resp)
}

// serveStub starts the stub on a loopback listener and returns its address.
func serveStub(t *testing.T, stub *stubServer) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := grpc.NewServer()
	pdfv1.RegisterPdfServiceServer(srv, stub)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return lis.Addr().String()
}

func TestParseStreamsBytesAndReturnsResult(t *testing.T) {
	stub := &stubServer{resp: &pdfv1.ParseResponse{
		PageCount:    3,
		Chunks:       []*pdfv1.Chunk{{Index: 0, Text: "hello", PageStart: 1, PageEnd: 2}},
		ThumbnailPng: []byte{0x89, 'P', 'N', 'G'},
	}}
	addr := serveStub(t, stub)

	client, err := pdf.New(pdf.Config{Addr: addr, Timeout: 5 * time.Second, MaxRecvBytes: 1 << 20})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer client.Close()

	// ~1.5 MiB forces multiple stream frames (frame size is 1 MiB).
	pdfBytes := bytes.Repeat([]byte("%PDF-1.7 data "), 110_000)
	res, err := client.Parse(context.Background(), bytes.NewReader(pdfBytes),
		pdf.Options{Filename: "doc.pdf", RenderThumbnail: true, ThumbnailDPI: 96})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	// All bytes arrived, reassembled in order.
	if !bytes.Equal(stub.gotBytes, pdfBytes) {
		t.Fatalf("server got %d bytes, want %d", len(stub.gotBytes), len(pdfBytes))
	}
	// Options were carried in the first frame.
	if got := stub.gotOptions; got == nil || got.GetFilename() != "doc.pdf" ||
		!got.GetRenderThumbnail() || got.GetThumbnailDpi() != 96 {
		t.Fatalf("options not propagated: %+v", stub.gotOptions)
	}
	// Response was mapped onto the typed Result.
	if res.PageCount != 3 || len(res.Chunks) != 1 || res.Chunks[0].Text != "hello" ||
		res.Chunks[0].PageStart != 1 || res.Chunks[0].PageEnd != 2 {
		t.Fatalf("unexpected result: %+v", res)
	}
	if !bytes.Equal(res.ThumbnailPNG, []byte{0x89, 'P', 'N', 'G'}) {
		t.Fatalf("thumbnail mismatch: %v", res.ThumbnailPNG)
	}
}

// mdValue returns the single metadata value for key, or "" if absent.
func mdValue(md metadata.MD, key string) string {
	if v := md.Get(key); len(v) > 0 {
		return v[0]
	}
	return ""
}

// TestParsePropagatesCorrelationMetadata asserts the client interceptor copies
// request_id/tenant_id from the call context into outgoing gRPC metadata under
// the exact header keys pdf-service reads (CON-111).
func TestParsePropagatesCorrelationMetadata(t *testing.T) {
	stub := &stubServer{resp: &pdfv1.ParseResponse{PageCount: 1}}
	addr := serveStub(t, stub)

	client, err := pdf.New(pdf.Config{Addr: addr, Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer client.Close()

	ctx := logging.WithRequestID(context.Background(), "rid")
	ctx = tenantctx.With(ctx, "ten")
	if _, err := client.Parse(ctx, bytes.NewReader([]byte("%PDF-1.7")), pdf.Options{}); err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if got := mdValue(stub.gotMD, "x-request-id"); got != "rid" {
		t.Fatalf("x-request-id = %q, want %q", got, "rid")
	}
	if got := mdValue(stub.gotMD, "x-tenant-id"); got != "ten" {
		t.Fatalf("x-tenant-id = %q, want %q", got, "ten")
	}
}

// TestParseWithoutCorrelationSendsNoHeaders asserts an id-less context sends no
// correlation headers, so pdf-service falls back to generating its own id.
func TestParseWithoutCorrelationSendsNoHeaders(t *testing.T) {
	stub := &stubServer{resp: &pdfv1.ParseResponse{PageCount: 1}}
	addr := serveStub(t, stub)

	client, err := pdf.New(pdf.Config{Addr: addr, Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer client.Close()

	if _, err := client.Parse(context.Background(), bytes.NewReader([]byte("%PDF-1.7")), pdf.Options{}); err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if got := mdValue(stub.gotMD, "x-request-id"); got != "" {
		t.Fatalf("x-request-id = %q, want none", got)
	}
	if got := mdValue(stub.gotMD, "x-tenant-id"); got != "" {
		t.Fatalf("x-tenant-id = %q, want none", got)
	}
}

func TestRenderStreamsBytesAndReturnsPageCountAndThumbnail(t *testing.T) {
	stub := &stubServer{renderResp: &pdfv1.RenderResponse{
		PageCount:    7,
		ThumbnailPng: []byte{0x89, 'P', 'N', 'G'},
	}}
	addr := serveStub(t, stub)

	client, err := pdf.New(pdf.Config{Addr: addr, Timeout: 5 * time.Second, MaxRecvBytes: 1 << 20})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer client.Close()

	pdfBytes := bytes.Repeat([]byte("%PDF-1.7 data "), 110_000) // multi-frame
	res, err := client.Render(context.Background(), bytes.NewReader(pdfBytes),
		pdf.RenderOptions{RenderThumbnail: true, ThumbnailDPI: 96})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !bytes.Equal(stub.gotRenderBytes, pdfBytes) {
		t.Fatalf("server got %d bytes, want %d", len(stub.gotRenderBytes), len(pdfBytes))
	}
	if got := stub.gotRenderOpts; got == nil || !got.GetRenderThumbnail() || got.GetThumbnailDpi() != 96 {
		t.Fatalf("render options not propagated: %+v", stub.gotRenderOpts)
	}
	if res.PageCount != 7 || !bytes.Equal(res.ThumbnailPNG, []byte{0x89, 'P', 'N', 'G'}) {
		t.Fatalf("unexpected render result: %+v", res)
	}
}

func TestDisabledClientReportsErrDisabled(t *testing.T) {
	c, err := pdf.New(pdf.Config{Addr: ""})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if c != nil {
		t.Fatalf("expected a nil client when Addr is empty")
	}
	if _, err := c.Parse(context.Background(), bytes.NewReader(nil), pdf.Options{}); !errors.Is(err, pdf.ErrDisabled) {
		t.Fatalf("expected ErrDisabled, got %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close on nil client: %v", err)
	}
}
