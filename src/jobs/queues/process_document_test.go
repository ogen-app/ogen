package queues

import (
	"context"
	"io"
	"testing"

	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/transport/grpc/client/documents"
)

// fakeDocParser satisfies the documentsParser interface. The embedder/storage/
// status/chunk/file fakes are shared with process_pdf_test.go (same package).
type fakeDocParser struct {
	res      *documents.Result
	err      error
	gotOpts  documents.Options
	gotBytes []byte
}

func (f *fakeDocParser) Parse(_ context.Context, r io.Reader, opts documents.Options) (*documents.Result, error) {
	f.gotOpts = opts
	f.gotBytes, _ = io.ReadAll(r)
	return f.res, f.err
}

func newDocProc(d DocumentDeps) *ProcessDocumentProcessor { return &ProcessDocumentProcessor{Deps: d} }

// TestProcessDocument_Success_AnchorsMapped is the heart of the job: every chunk
// kind must persist the right source_label + source_anchor, and only page-flow
// chunks fill the retained page_start/page_end columns.
func TestProcessDocument_Success_AnchorsMapped(t *testing.T) {
	parser := &fakeDocParser{res: &documents.Result{
		Format: "pptx",
		Chunks: []documents.Chunk{
			{Index: 0, Text: "deck slide body", SourceLabel: "Slide 4",
				Anchor: documents.Anchor{Kind: "slide", Slide: 4}},
			{Index: 1, Text: "Region: EMEA | ACV: 41200", SourceLabel: "Sheet 'Q3' rows 1-2",
				Anchor: documents.Anchor{Kind: "sheet", Sheet: "Q3", CellRange: "A1:B2"}},
			{Index: 2, Text: "prose flow paragraph", SourceLabel: "Page 2",
				Anchor: documents.Anchor{Kind: "page", PageStart: 2, PageEnd: 2}, TokenCount: 123},
		},
	}}
	blob := &fakeBlob{data: []byte("PK\x03\x04 docx body")}
	status := &fakeStatus{}
	chunks := &fakeChunks{}
	files := &fakeFiles{}
	p := newDocProc(DocumentDeps{Client: parser, Embedder: &fakeEmbedder{}, Storage: blob, Assets: status, Chunks: chunks, Files: files})

	task := ProcessDocumentTask{AssetID: "d1", OriginalName: "deck.pptx", MimeType: "application/vnd.openxmlformats-officedocument.presentationml.presentation", StorageKey: "assets/d1/original.pptx"}
	if err := p.process(t.Context(), task, false); err != nil {
		t.Fatalf("process: %v", err)
	}

	if parser.gotOpts.Filename != "deck.pptx" || parser.gotOpts.ContentType != task.MimeType {
		t.Fatalf("parse options not propagated: %+v", parser.gotOpts)
	}
	if len(chunks.got) != 3 {
		t.Fatalf("want 3 stored chunks, got %d", len(chunks.got))
	}

	// Slide chunk: label + anchor set; page columns stay nil.
	slide := chunks.got[0]
	if slide.ID != "d1:0" || slide.SourceLabel == nil || *slide.SourceLabel != "Slide 4" {
		t.Fatalf("slide chunk label wrong: %+v", slide)
	}
	if slide.SourceAnchor == nil || slide.SourceAnchor.Kind != "slide" || slide.SourceAnchor.Slide != 4 {
		t.Fatalf("slide anchor wrong: %+v", slide.SourceAnchor)
	}
	if slide.PageStart != nil || slide.PageEnd != nil {
		t.Fatalf("non-page chunk must leave page columns nil: %+v", slide)
	}

	// Sheet chunk: sheet + cell range persisted.
	sheet := chunks.got[1]
	if sheet.SourceAnchor == nil || sheet.SourceAnchor.Kind != "sheet" || sheet.SourceAnchor.Sheet != "Q3" || sheet.SourceAnchor.CellRange != "A1:B2" {
		t.Fatalf("sheet anchor wrong: %+v", sheet.SourceAnchor)
	}
	if sheet.PageStart != nil || sheet.PageEnd != nil {
		t.Fatalf("sheet chunk must leave page columns nil: %+v", sheet)
	}

	// Page chunk: fills BOTH the jsonb Page and the retained page_start/page_end
	// columns, and takes the service-provided token count verbatim.
	page := chunks.got[2]
	if page.SourceAnchor == nil || page.SourceAnchor.Kind != "page" || page.SourceAnchor.Page != 2 {
		t.Fatalf("page anchor wrong: %+v", page.SourceAnchor)
	}
	if page.PageStart == nil || *page.PageStart != 2 || page.PageEnd == nil || *page.PageEnd != 2 {
		t.Fatalf("page columns not populated: %+v", page)
	}
	if page.TokenCount != 123 {
		t.Fatalf("service token count not used: got %d want 123", page.TokenCount)
	}

	// File row: no thumbnail, no page count for documents in v1.
	if files.got == nil || files.got.OriginalName != "deck.pptx" || files.got.PageCount != nil || files.got.ThumbnailS3Key != nil {
		t.Fatalf("asset file persisted wrong: %+v", files.got)
	}
	if status.last() != models.AssetStatusReady {
		t.Fatalf("final status = %q, want ready (saw %v)", status.last(), status.all)
	}
}

func TestProcessDocument_UnsupportedIsTerminal(t *testing.T) {
	status := &fakeStatus{}
	p := newDocProc(DocumentDeps{
		Client:   &fakeDocParser{err: grpcstatus.Error(codes.Unimplemented, "legacy OLE2 unsupported")},
		Embedder: &fakeEmbedder{}, Storage: &fakeBlob{data: []byte("doc")},
		Assets: status, Chunks: &fakeChunks{}, Files: &fakeFiles{},
	})
	if err := p.process(t.Context(), ProcessDocumentTask{AssetID: "d2", StorageKey: "assets/d2/original.doc"}, false); err != nil {
		t.Fatalf("unsupported document must not be retried (want nil err): %v", err)
	}
	if status.last() != models.AssetStatusFailed {
		t.Fatalf("status = %q, want failed", status.last())
	}
}

func TestProcessDocument_InvalidIsTerminal(t *testing.T) {
	status := &fakeStatus{}
	p := newDocProc(DocumentDeps{
		Client:   &fakeDocParser{err: grpcstatus.Error(codes.InvalidArgument, "corrupt zip")},
		Embedder: &fakeEmbedder{}, Storage: &fakeBlob{data: []byte("doc")},
		Assets: status, Chunks: &fakeChunks{}, Files: &fakeFiles{},
	})
	if err := p.process(t.Context(), ProcessDocumentTask{AssetID: "d3", StorageKey: "assets/d3/original.docx"}, false); err != nil {
		t.Fatalf("corrupt document must not be retried (want nil err): %v", err)
	}
	if status.last() != models.AssetStatusFailed {
		t.Fatalf("status = %q, want failed", status.last())
	}
}

func TestProcessDocument_TransientParseErrorRetries(t *testing.T) {
	p := newDocProc(DocumentDeps{
		Client:   &fakeDocParser{err: grpcstatus.Error(codes.Unavailable, "service down")},
		Embedder: &fakeEmbedder{}, Storage: &fakeBlob{data: []byte("doc")},
		Assets: &fakeStatus{}, Chunks: &fakeChunks{}, Files: &fakeFiles{},
	})
	if err := p.process(t.Context(), ProcessDocumentTask{AssetID: "d4", StorageKey: "assets/d4/original.docx"}, false); err == nil {
		t.Fatal("transient parse error should be retried (want non-nil err)")
	}
}

func TestProcessDocument_EmptyReadyNoChunks(t *testing.T) {
	status := &fakeStatus{}
	chunks := &fakeChunks{}
	p := newDocProc(DocumentDeps{
		Client:   &fakeDocParser{res: &documents.Result{Format: "txt"}},
		Embedder: &fakeEmbedder{}, Storage: &fakeBlob{data: []byte("doc")},
		Assets: status, Chunks: chunks, Files: &fakeFiles{},
	})
	if err := p.process(t.Context(), ProcessDocumentTask{AssetID: "d5", StorageKey: "assets/d5/original.txt"}, false); err != nil {
		t.Fatalf("process: %v", err)
	}
	if status.last() != models.AssetStatusReady {
		t.Fatalf("status = %q, want ready", status.last())
	}
	if chunks.calls != 0 {
		t.Fatalf("no chunks should be upserted for an empty document")
	}
}

func TestProcessDocument_DisabledClientNoOp(t *testing.T) {
	status := &fakeStatus{}
	p := newDocProc(DocumentDeps{Client: nil, Assets: status})
	if err := p.process(t.Context(), ProcessDocumentTask{AssetID: "d6"}, false); err != nil {
		t.Fatalf("disabled client should no-op: %v", err)
	}
	if len(status.all) != 0 {
		t.Fatalf("disabled client must not touch status, saw %v", status.all)
	}
}
