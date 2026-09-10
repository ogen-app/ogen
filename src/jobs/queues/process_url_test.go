package queues

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/eventhub"
	"github.com/ogen-app/ogen/src/infra/firecrawl"
)

// ---- fakes for the process_url worker (reusing fakeEmbedder/fakeBlob/fakeChunks
// from process_pdf_test.go, same package) ----

type fakeScraper struct {
	res    *firecrawl.ScrapeResult
	err    error
	gotReq firecrawl.ScrapeRequest
}

func (f *fakeScraper) Scrape(_ context.Context, in firecrawl.ScrapeRequest) (*firecrawl.ScrapeResult, error) {
	f.gotReq = in
	return f.res, f.err
}

type fakeURLAssets struct {
	title, content string
	contentCalls   int
	statuses       []string
}

func (f *fakeURLAssets) UpdateContent(_ context.Context, _, title, content string) error {
	f.title, f.content = title, content
	f.contentCalls++
	return nil
}

func (f *fakeURLAssets) UpdateStatus(_ context.Context, _, status string) error {
	f.statuses = append(f.statuses, status)
	return nil
}

func (f *fakeURLAssets) CreatorOf(context.Context, string) (string, error) { return "", nil }

func (f *fakeURLAssets) last() string {
	if len(f.statuses) == 0 {
		return ""
	}
	return f.statuses[len(f.statuses)-1]
}

type fakeImages struct {
	got   []models.AssetImage
	calls int
}

func (f *fakeImages) ReplaceForAsset(_ context.Context, _ string, imgs []models.AssetImage) error {
	f.got = imgs
	f.calls++
	return nil
}

type fakeImg struct {
	body []byte
	ct   string
}

type fakeFetcher struct {
	data map[string]fakeImg
	err  error
}

func (f *fakeFetcher) Fetch(_ context.Context, rawURL string) ([]byte, string, error) {
	if f.err != nil {
		return nil, "", f.err
	}
	if im, ok := f.data[rawURL]; ok {
		return im.body, im.ct, nil
	}
	return nil, "", errors.New("image not found")
}

type fakeHub struct {
	events []eventhub.Event
}

func (f *fakeHub) Publish(_ context.Context, ev eventhub.Event) error {
	f.events = append(f.events, ev)
	return nil
}

func (f *fakeHub) lastPayload() assetEventPayload {
	if len(f.events) == 0 {
		return assetEventPayload{}
	}
	p, _ := f.events[len(f.events)-1].Payload.(assetEventPayload)
	return p
}

func newURLProc(d URLDeps) *ProcessURLProcessor { return &ProcessURLProcessor{Deps: d} }

func TestProcessURL_Success_MirrorsImageAndEmbeds(t *testing.T) {
	md := "# Heading\n\nIntro paragraph with enough words to embed.\n\n" +
		"![logo](https://example.com/a.png)\n\nClosing paragraph, also wordy."
	scraper := &fakeScraper{res: &firecrawl.ScrapeResult{Markdown: md, Title: "Example Page"}}
	assets := &fakeURLAssets{}
	images := &fakeImages{}
	chunks := &fakeChunks{}
	hub := &fakeHub{}
	blob := &fakeBlob{}
	fetcher := &fakeFetcher{data: map[string]fakeImg{
		"https://example.com/a.png": {body: []byte("PNGBYTES"), ct: "image/png"},
	}}

	p := newURLProc(URLDeps{
		Scraper: scraper, Embedder: &fakeEmbedder{}, Storage: blob, Fetcher: fetcher,
		Assets: assets, Chunks: chunks, Images: images, Hub: hub,
	})

	if err := p.process(t.Context(), ProcessURLTask{AssetID: "u1", TenantID: "t1", SourceURL: "https://example.com/x"}, false); err != nil {
		t.Fatalf("process: %v", err)
	}

	if assets.last() != models.AssetStatusReady {
		t.Fatalf("final status = %q, want ready (saw %v)", assets.last(), assets.statuses)
	}
	if assets.title != "Example Page" {
		t.Fatalf("title = %q, want the page title", assets.title)
	}
	// The image link must be rewritten to the stored object and the original
	// external URL gone from the persisted Markdown.
	if strings.Contains(assets.content, "https://example.com/a.png") {
		t.Fatalf("original image URL should be rewritten, content=%q", assets.content)
	}
	if !strings.Contains(assets.content, "https://stub/") {
		t.Fatalf("content should reference the mirrored object, content=%q", assets.content)
	}
	if images.calls != 1 || len(images.got) != 1 {
		t.Fatalf("expected 1 mirrored image row, got calls=%d n=%d", images.calls, len(images.got))
	}
	if images.got[0].SourceURL != "https://example.com/a.png" || images.got[0].MimeType != "image/png" || images.got[0].S3Key == "" {
		t.Fatalf("unexpected image row: %+v", images.got[0])
	}
	if len(chunks.got) == 0 {
		t.Fatalf("expected embedded chunks to be upserted")
	}
	// Progress + terminal SSE frames.
	if len(hub.events) < 2 {
		t.Fatalf("expected >=2 hub events (processing + ready), got %d", len(hub.events))
	}
	if got := hub.lastPayload(); got.Status != models.AssetStatusReady || got.Type != models.AssetTypeURL || got.ImageCount != 1 {
		t.Fatalf("unexpected terminal event payload: %+v", got)
	}
}

func TestProcessURL_RefreshBypassesCache(t *testing.T) {
	scraper := &fakeScraper{res: &firecrawl.ScrapeResult{Markdown: "hello world words", Title: "T"}}
	p := newURLProc(URLDeps{Scraper: scraper, Embedder: &fakeEmbedder{}, Assets: &fakeURLAssets{}, Chunks: &fakeChunks{}})

	if err := p.process(t.Context(), ProcessURLTask{AssetID: "u2", SourceURL: "https://example.com/", Refresh: true}, false); err != nil {
		t.Fatalf("process: %v", err)
	}
	if scraper.gotReq.MaxAge == nil || *scraper.gotReq.MaxAge != 0 {
		t.Fatalf("refresh should set MaxAge=0 to bypass cache, got %v", scraper.gotReq.MaxAge)
	}
}

func TestProcessURL_TerminalScrapeErrorFailsNoRetry(t *testing.T) {
	assets := &fakeURLAssets{}
	p := newURLProc(URLDeps{
		Scraper:  &fakeScraper{err: &firecrawl.APIError{Status: 404, Message: "not found"}},
		Embedder: &fakeEmbedder{}, Assets: assets, Chunks: &fakeChunks{},
	})
	if err := p.process(t.Context(), ProcessURLTask{AssetID: "u3", SourceURL: "https://example.com/x"}, false); err != nil {
		t.Fatalf("terminal scrape error must not be retried (want nil err): %v", err)
	}
	if assets.last() != models.AssetStatusFailed {
		t.Fatalf("status = %q, want failed", assets.last())
	}
}

func TestProcessURL_TransientScrapeErrorRetries(t *testing.T) {
	p := newURLProc(URLDeps{
		Scraper:  &fakeScraper{err: &firecrawl.APIError{Status: 503, Message: "down", Transient: true}},
		Embedder: &fakeEmbedder{}, Assets: &fakeURLAssets{}, Chunks: &fakeChunks{},
	})
	if err := p.process(t.Context(), ProcessURLTask{AssetID: "u4", SourceURL: "https://example.com/x"}, false); err == nil {
		t.Fatal("transient scrape error should be retried (want non-nil err)")
	}
}

func TestProcessURL_ImageFetchFailureIsPartial(t *testing.T) {
	md := "words words words ![x](https://example.com/broken.png) more words here"
	assets := &fakeURLAssets{}
	p := newURLProc(URLDeps{
		Scraper:  &fakeScraper{res: &firecrawl.ScrapeResult{Markdown: md, Title: "T"}},
		Embedder: &fakeEmbedder{}, Storage: &fakeBlob{}, Fetcher: &fakeFetcher{err: errors.New("404")},
		Assets: assets, Chunks: &fakeChunks{}, Images: &fakeImages{},
	})
	if err := p.process(t.Context(), ProcessURLTask{AssetID: "u5", SourceURL: "https://example.com/x"}, false); err != nil {
		t.Fatalf("process: %v", err)
	}
	if assets.last() != models.AssetStatusPartial {
		t.Fatalf("status = %q, want partial (an image failed to mirror)", assets.last())
	}
	// The failed image keeps its original external link in the Markdown.
	if !strings.Contains(assets.content, "https://example.com/broken.png") {
		t.Fatalf("failed image should keep its external link, content=%q", assets.content)
	}
}

func TestProcessURL_SVGIsNotMirrored(t *testing.T) {
	md := "words words words ![x](https://example.com/evil.svg) more words here"
	assets := &fakeURLAssets{}
	images := &fakeImages{}
	blob := &fakeBlob{}
	p := newURLProc(URLDeps{
		Scraper:  &fakeScraper{res: &firecrawl.ScrapeResult{Markdown: md, Title: "T"}},
		Embedder: &fakeEmbedder{}, Storage: blob, Images: images,
		Fetcher: &fakeFetcher{data: map[string]fakeImg{
			"https://example.com/evil.svg": {body: []byte("<svg onload=alert(1)>"), ct: "image/svg+xml"},
		}},
		Assets: assets, Chunks: &fakeChunks{},
	})
	if err := p.process(t.Context(), ProcessURLTask{AssetID: "u7", SourceURL: "https://example.com/x"}, false); err != nil {
		t.Fatalf("process: %v", err)
	}
	// SVG is never mirrored: no image row, nothing uploaded, external link kept,
	// and it's a deliberate skip (not a failure) so the asset is still ready.
	if len(images.got) != 0 || len(blob.uploads) != 0 {
		t.Fatalf("SVG must not be mirrored: images=%d uploads=%d", len(images.got), len(blob.uploads))
	}
	if !strings.Contains(assets.content, "https://example.com/evil.svg") {
		t.Fatalf("SVG external link should be kept, content=%q", assets.content)
	}
	if assets.last() != models.AssetStatusReady {
		t.Fatalf("status = %q, want ready (SVG skip is not a failure)", assets.last())
	}
}

func TestProcessURL_DisabledScraperNoOp(t *testing.T) {
	assets := &fakeURLAssets{}
	p := newURLProc(URLDeps{Scraper: nil, Assets: assets})
	if err := p.process(t.Context(), ProcessURLTask{AssetID: "u6"}, false); err != nil {
		t.Fatalf("disabled scraper should no-op: %v", err)
	}
	if len(assets.statuses) != 0 {
		t.Fatalf("disabled scraper must not touch status, saw %v", assets.statuses)
	}
}
