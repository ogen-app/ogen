package queues

import (
	"context"
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/infra/firecrawl"
	"github.com/ogen-app/ogen/src/transport/grpc/client/pdf"
)

// fakePublished records SetPublishedAt calls by asset id.
type fakePublished struct{ got map[string]time.Time }

func (f *fakePublished) SetPublishedAt(_ context.Context, id string, at time.Time) error {
	if f.got == nil {
		f.got = map[string]time.Time{}
	}
	f.got[id] = at
	return nil
}

func TestProcessPDF_RecordsCreationDate(t *testing.T) {
	parser := &fakeParser{res: &pdf.Result{PageCount: 1, Chunks: []pdf.Chunk{{Index: 0, Text: "hello world", PageStart: 1, PageEnd: 1}}}}
	blob := &fakeBlob{data: []byte("%PDF-1.7 body trailer << /Info << /CreationDate (D:20240315103000Z) >> >>")}
	published := &fakePublished{}
	p := newProc(PDFDeps{Client: parser, Embedder: &fakeEmbedder{}, Storage: blob, Assets: &fakeStatus{},
		Chunks: &fakeChunks{}, Files: &fakeFiles{}, Published: published})

	if err := p.process(t.Context(), ProcessPDFTask{AssetID: "a1", OriginalName: "x.pdf"}, false); err != nil {
		t.Fatalf("process: %v", err)
	}
	if want := time.Date(2024, 3, 15, 10, 30, 0, 0, time.UTC); !published.got["a1"].Equal(want) {
		t.Errorf("published_at = %v, want %v", published.got["a1"], want)
	}
	if string(parser.gotBytes) != string(blob.data) {
		t.Errorf("the parser got %q, want the whole file through the sniffer", parser.gotBytes)
	}
}

func TestProcessURL_RecordsPublishedTime(t *testing.T) {
	scraper := &fakeScraper{res: &firecrawl.ScrapeResult{Markdown: "hello world words", Title: "T",
		PublishedTime: []string{"", "2025-08-14T09:30:00+02:00"}}}
	published := &fakePublished{}
	p := newURLProc(URLDeps{Scraper: scraper, Embedder: &fakeEmbedder{}, Assets: &fakeURLAssets{}, Chunks: &fakeChunks{}, Published: published})

	if err := p.process(t.Context(), ProcessURLTask{AssetID: "u1", SourceURL: "https://example.com/"}, false); err != nil {
		t.Fatalf("process: %v", err)
	}
	if want := time.Date(2025, 8, 14, 7, 30, 0, 0, time.UTC); !published.got["u1"].Equal(want) {
		t.Errorf("published_at = %v, want %v", published.got["u1"], want)
	}

	// No usable tag, nothing recorded.
	scraper.res.PublishedTime = []string{"soon"}
	if err := p.process(t.Context(), ProcessURLTask{AssetID: "u2", SourceURL: "https://example.com/2"}, false); err != nil {
		t.Fatalf("process: %v", err)
	}
	if _, ok := published.got["u2"]; ok {
		t.Error("an unreadable published time was recorded")
	}
}
