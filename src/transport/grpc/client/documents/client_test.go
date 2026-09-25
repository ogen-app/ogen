package documents_test

import (
	"bytes"
	"errors"
	"io"
	"net"
	"slices"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	documentsv1 "github.com/ogen-app/ogen/gen/documents/v1"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
	"github.com/ogen-app/ogen/src/transport/grpc/client/documents"
)

// stubServer records the streamed request and returns a canned response, or
// fails the call with err (without draining the stream) when err is set.
type stubServer struct {
	documentsv1.UnimplementedDocumentsServiceServer
	gotOptions *documentsv1.ParseOptions
	gotBytes   []byte
	gotFrames  int         // number of byte frames received
	gotMD      metadata.MD // inbound gRPC metadata seen on the Parse stream
	resp       *documentsv1.ParseResponse
	err        error
}

func (s *stubServer) Parse(stream grpc.ClientStreamingServer[documentsv1.ParseRequest, documentsv1.ParseResponse]) error {
	s.gotMD, _ = metadata.FromIncomingContext(stream.Context())
	if s.err != nil {
		return s.err
	}
	for {
		req, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		switch p := req.Payload.(type) {
		case *documentsv1.ParseRequest_Options:
			s.gotOptions = p.Options
		case *documentsv1.ParseRequest_Chunk:
			s.gotFrames++
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
	documentsv1.RegisterDocumentsServiceServer(srv, stub)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return lis.Addr().String()
}

func newClient(t *testing.T, addr string) *documents.Client {
	t.Helper()
	c, err := documents.New(documents.Config{Addr: addr, Timeout: 5 * time.Second, MaxRecvBytes: 1 << 20})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestParseStreamsBytesAndReturnsResult(t *testing.T) {
	stub := &stubServer{resp: &documentsv1.ParseResponse{
		Format: "xlsx",
		Chunks: []*documentsv1.Chunk{{
			Index:       0,
			Text:        "Sheet: Q1\nrevenue,100",
			SourceLabel: "Q1!A1:B2",
			TokenCount:  12,
			Anchor: &documentsv1.Anchor{
				Kind:      documentsv1.AnchorKind_ANCHOR_KIND_SHEET,
				Sheet:     "Q1",
				CellRange: "A1:B2",
			},
		}, {
			Index: 1,
			Text:  "no anchor",
		}},
	}}
	addr := serveStub(t, stub)
	client := newClient(t, addr)

	// ~2.5 MiB forces multiple stream frames (frame size is 1 MiB).
	docBytes := bytes.Repeat([]byte("PK\x03\x04 data "), 250_000)
	res, err := client.Parse(t.Context(), bytes.NewReader(docBytes), documents.Options{
		Filename:          "report.xlsx",
		ContentType:       "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
		ChunkTargetChars:  1500,
		ChunkOverlapChars: 150,
		ChunkMaxChars:     3000,
	})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	// All bytes arrived, reassembled in order, across several frames.
	if !bytes.Equal(stub.gotBytes, docBytes) {
		t.Fatalf("server got %d bytes, want %d", len(stub.gotBytes), len(docBytes))
	}
	if stub.gotFrames < 3 {
		t.Fatalf("expected >=3 byte frames for %d bytes, got %d", len(docBytes), stub.gotFrames)
	}
	// Options were carried in the first frame.
	got := stub.gotOptions
	if got == nil || got.GetFilename() != "report.xlsx" ||
		got.GetContentType() != "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet" ||
		got.GetChunkTargetChars() != 1500 || got.GetChunkOverlapChars() != 150 || got.GetChunkMaxChars() != 3000 {
		t.Fatalf("options not propagated: %+v", got)
	}

	// Response was mapped onto the typed Result.
	if res.Format != "xlsx" || len(res.Chunks) != 2 {
		t.Fatalf("unexpected result: %+v", res)
	}
	c0 := res.Chunks[0]
	if c0.Index != 0 || c0.Text != "Sheet: Q1\nrevenue,100" || c0.SourceLabel != "Q1!A1:B2" || c0.TokenCount != 12 {
		t.Fatalf("unexpected chunk 0: %+v", c0)
	}
	if c0.Anchor.Kind != "sheet" || c0.Anchor.Sheet != "Q1" || c0.Anchor.CellRange != "A1:B2" {
		t.Fatalf("unexpected chunk 0 anchor: %+v", c0.Anchor)
	}
	// A chunk with no anchor maps to the zero Anchor.
	if c1 := res.Chunks[1]; c1.Index != 1 || c1.Anchor.Kind != "" || c1.Anchor.HeadingPath != nil {
		t.Fatalf("unexpected chunk 1: %+v", c1)
	}
}

func TestParseMapsEveryAnchorKind(t *testing.T) {
	stub := &stubServer{resp: &documentsv1.ParseResponse{
		Format: "mixed",
		Chunks: []*documentsv1.Chunk{
			{Index: 0, Anchor: &documentsv1.Anchor{Kind: documentsv1.AnchorKind_ANCHOR_KIND_PAGE, PageStart: 2, PageEnd: 4}},
			{Index: 1, Anchor: &documentsv1.Anchor{Kind: documentsv1.AnchorKind_ANCHOR_KIND_SLIDE, Slide: 7}},
			{Index: 2, Anchor: &documentsv1.Anchor{Kind: documentsv1.AnchorKind_ANCHOR_KIND_SHEET, Sheet: "Data", CellRange: "A10:F24"}},
			{Index: 3, Anchor: &documentsv1.Anchor{Kind: documentsv1.AnchorKind_ANCHOR_KIND_SECTION, HeadingPath: []string{"Onboarding", "Auth", "SSO"}}},
			{Index: 4, Anchor: &documentsv1.Anchor{Kind: documentsv1.AnchorKind_ANCHOR_KIND_EMAIL}},
			{Index: 5, Anchor: &documentsv1.Anchor{Kind: documentsv1.AnchorKind_ANCHOR_KIND_UNSPECIFIED}},
		},
	}}
	addr := serveStub(t, stub)
	client := newClient(t, addr)

	res, err := client.Parse(t.Context(), bytes.NewReader([]byte("hello")), documents.Options{})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(res.Chunks) != 6 {
		t.Fatalf("got %d chunks, want 6", len(res.Chunks))
	}

	wantKinds := []string{"page", "slide", "sheet", "section", "email", ""}
	for i, want := range wantKinds {
		if got := res.Chunks[i].Anchor.Kind; got != want {
			t.Fatalf("chunk %d kind = %q, want %q", i, got, want)
		}
	}
	if a := res.Chunks[0].Anchor; a.PageStart != 2 || a.PageEnd != 4 {
		t.Fatalf("page range not mapped: %+v", a)
	}
	if a := res.Chunks[1].Anchor; a.Slide != 7 {
		t.Fatalf("slide not mapped: %+v", a)
	}
	if a := res.Chunks[2].Anchor; a.Sheet != "Data" || a.CellRange != "A10:F24" {
		t.Fatalf("sheet/cell range not mapped: %+v", a)
	}
	if a := res.Chunks[3].Anchor; !slices.Equal(a.HeadingPath, []string{"Onboarding", "Auth", "SSO"}) {
		t.Fatalf("heading path not mapped: %+v", a)
	}
}

func TestParseClassifiesTerminalErrors(t *testing.T) {
	cases := []struct {
		name            string
		code            codes.Code
		wantInvalid     bool
		wantUnsupported bool
	}{
		{"invalid argument", codes.InvalidArgument, true, false},
		{"failed precondition", codes.FailedPrecondition, true, false},
		{"unimplemented", codes.Unimplemented, false, true},
		{"unavailable is transient", codes.Unavailable, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stub := &stubServer{err: status.Error(tc.code, "nope")}
			addr := serveStub(t, stub)
			client := newClient(t, addr)

			// Multi-frame payload so the client may hit an aborted stream mid-send
			// and must still surface the server's real status.
			docBytes := bytes.Repeat([]byte("x"), 3<<20)
			_, err := client.Parse(t.Context(), bytes.NewReader(docBytes), documents.Options{Filename: "bad.docx"})
			if err == nil {
				t.Fatalf("expected an error")
			}
			st, ok := status.FromError(err)
			if !ok || st.Code() != tc.code {
				t.Fatalf("status.FromError(%v) = (%v, %v), want code %v", err, st.Code(), ok, tc.code)
			}
			if got := documents.IsInvalidDocument(err); got != tc.wantInvalid {
				t.Fatalf("IsInvalidDocument = %v, want %v (err %v)", got, tc.wantInvalid, err)
			}
			if got := documents.IsUnsupportedDocument(err); got != tc.wantUnsupported {
				t.Fatalf("IsUnsupportedDocument = %v, want %v (err %v)", got, tc.wantUnsupported, err)
			}
		})
	}
}

func TestClassifiersRejectNonStatusErrors(t *testing.T) {
	err := errors.New("plain")
	if documents.IsInvalidDocument(err) || documents.IsUnsupportedDocument(err) {
		t.Fatalf("plain error must not classify as terminal")
	}
	if documents.IsInvalidDocument(nil) || documents.IsUnsupportedDocument(nil) {
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

// TestParsePropagatesCorrelationMetadata asserts the client interceptor copies
// request_id/tenant_id from the call context into outgoing gRPC metadata under
// the exact header keys document-service reads (CON-111).
func TestParsePropagatesCorrelationMetadata(t *testing.T) {
	stub := &stubServer{resp: &documentsv1.ParseResponse{Format: "txt"}}
	addr := serveStub(t, stub)
	client := newClient(t, addr)

	ctx := logging.WithRequestID(t.Context(), "rid")
	ctx = tenantctx.With(ctx, "ten")
	if _, err := client.Parse(ctx, bytes.NewReader([]byte("hi")), documents.Options{}); err != nil {
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
// correlation headers, so document-service falls back to generating its own id.
func TestParseWithoutCorrelationSendsNoHeaders(t *testing.T) {
	stub := &stubServer{resp: &documentsv1.ParseResponse{Format: "txt"}}
	addr := serveStub(t, stub)
	client := newClient(t, addr)

	if _, err := client.Parse(t.Context(), bytes.NewReader([]byte("hi")), documents.Options{}); err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := mdValue(stub.gotMD, "x-request-id"); got != "" {
		t.Fatalf("x-request-id = %q, want none", got)
	}
	if got := mdValue(stub.gotMD, "x-tenant-id"); got != "" {
		t.Fatalf("x-tenant-id = %q, want none", got)
	}
}

func TestDisabledClientReportsErrDisabled(t *testing.T) {
	c, err := documents.New(documents.Config{Addr: ""})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if c != nil {
		t.Fatalf("expected a nil client when Addr is empty")
	}
	if _, err := c.Parse(t.Context(), bytes.NewReader(nil), documents.Options{}); !errors.Is(err, documents.ErrDisabled) {
		t.Fatalf("expected ErrDisabled, got %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close on nil client: %v", err)
	}
}
