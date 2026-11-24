package queues

import (
	"context"
	"strings"
	"testing"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/firecrawl"
)

func TestSiteMetaOnlyForASiteRoot(t *testing.T) {
	cases := []struct {
		page, siteName, title, favicon string
		wantName, wantIcon             string
		none                           bool
	}{
		{page: "https://acme.com/", siteName: "Acme", favicon: "https://cdn.acme.com/i.png", wantName: "Acme", wantIcon: "https://cdn.acme.com/i.png"},
		{page: "https://acme.com", title: "Home | Acme", favicon: "/static/icon.ico", wantName: "Acme", wantIcon: "https://acme.com/static/icon.ico"},
		{page: "https://acme.com/", title: "Acme Roasters – Light coffee, roasted to order", wantName: "Acme Roasters", wantIcon: "https://acme.com/favicon.ico"},
		{page: "https://acme.com/about", siteName: "Acme", none: true},
		{page: "https://acme.com/", favicon: "data:image/png;base64,AAAA", wantIcon: ""},
	}
	for _, tc := range cases {
		meta := siteMeta(tc.page, &firecrawl.ScrapeResult{SiteName: tc.siteName, Title: tc.title, Favicon: tc.favicon})
		if tc.none {
			if meta != nil {
				t.Errorf("%s: meta = %+v, want none", tc.page, meta)
			}
			continue
		}
		if meta == nil || meta.Name != tc.wantName || meta.FaviconSource != tc.wantIcon {
			t.Errorf("%s %q: meta = %+v, want name %q icon %q", tc.page, tc.title, meta, tc.wantName, tc.wantIcon)
		}
	}
}

type fakeSites struct {
	id   string
	meta *models.AssetSiteMeta
}

func (f *fakeSites) SetSiteMeta(_ context.Context, id string, meta *models.AssetSiteMeta) error {
	f.id, f.meta = id, meta
	return nil
}

func TestProcessURL_SiteRootRecordsNameAndCopiesIcon(t *testing.T) {
	scraper := &fakeScraper{res: &firecrawl.ScrapeResult{
		Markdown: "We roast light. Enough words here to embed.", Title: "Acme | Coffee", SiteName: "Acme", Favicon: "/icon.ico",
	}}
	blob, sites := &fakeBlob{}, &fakeSites{}
	p := newURLProc(URLDeps{
		Scraper: scraper, Embedder: &fakeEmbedder{}, Storage: blob, Assets: &fakeURLAssets{}, Chunks: &fakeChunks{},
		Images: &fakeImages{}, Sites: sites,
		Fetcher: &fakeFetcher{data: map[string]fakeImg{"https://acme.com/icon.ico": {body: []byte("ICO"), ct: "image/x-icon"}}},
	})
	if err := p.process(t.Context(), ProcessURLTask{AssetID: "home", TenantID: "t1", SourceURL: "https://acme.com/"}, false); err != nil {
		t.Fatal(err)
	}
	if sites.id != "home" || sites.meta == nil || sites.meta.Name != "Acme" {
		t.Fatalf("site meta = %q %+v", sites.id, sites.meta)
	}
	key := sites.meta.FaviconKey
	if !strings.HasSuffix(key, "assets/home/favicon.ico") || string(blob.uploads[key]) != "ICO" {
		t.Fatalf("favicon key = %q, uploads = %v", key, blob.uploads)
	}

	t.Run("an SVG icon is not copied, and another page says nothing", func(t *testing.T) {
		sites := &fakeSites{}
		p.Deps.Sites = sites
		p.Deps.Fetcher = &fakeFetcher{data: map[string]fakeImg{"https://acme.com/icon.ico": {body: []byte("<svg/>"), ct: "image/svg+xml"}}}
		if err := p.process(t.Context(), ProcessURLTask{AssetID: "home2", TenantID: "t1", SourceURL: "https://acme.com"}, false); err != nil {
			t.Fatal(err)
		}
		if sites.meta == nil || sites.meta.FaviconKey != "" || sites.meta.Name != "Acme" {
			t.Fatalf("site meta = %+v", sites.meta)
		}
		sites.meta = nil
		if err := p.process(t.Context(), ProcessURLTask{AssetID: "about", TenantID: "t1", SourceURL: "https://acme.com/about"}, false); err != nil {
			t.Fatal(err)
		}
		if sites.meta != nil {
			t.Fatalf("a page that is not the root recorded %+v", sites.meta)
		}
	})
}

func TestSiteNameOfTitle(t *testing.T) {
	for title, want := range map[string]string{
		"Acme | Specialty coffee":     "Acme",
		"Welcome - Acme":              "Acme",
		"Homepage":                    "",
		"Acme":                        "Acme",
		"Acme: roasted in Bristol":    "Acme",
		"Specialty coffee — Acme Ltd": "Acme Ltd",
	} {
		if got := siteNameOf("", title); got != want {
			t.Errorf("siteNameOf(%q) = %q, want %q", title, got, want)
		}
	}
}
