package queues

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/firecrawl"
	"github.com/ogen-app/ogen/src/infra/storage"
)

// maxFaviconBytes caps a copied site icon.
const maxFaviconBytes = 1 << 20

// siteMeta is what a site root's page says about the site: its name and where
// its icon is. Any other page says nothing about the site, so it is nil. The
// root is the page asked for; the icon resolves against where the page landed
// (a homepage may redirect to /en/).
func siteMeta(pageURL string, res *firecrawl.ScrapeResult) *models.AssetSiteMeta {
	u, err := url.Parse(pageURL)
	if err != nil || (u.Path != "" && u.Path != "/") || u.Host == "" {
		return nil
	}
	if final, err := url.Parse(res.SourceURL); err == nil && final.Host != "" {
		u = final
	}
	meta := &models.AssetSiteMeta{Name: siteNameOf(res.SiteName, res.Title)}
	icon := res.Favicon
	if icon == "" {
		icon = "/favicon.ico"
	}
	if ref, err := url.Parse(icon); err == nil {
		if abs := u.ResolveReference(ref); abs.Scheme == "http" || abs.Scheme == "https" {
			meta.FaviconSource = abs.String()
		}
	}
	return meta
}

// titleSeparators split a page title into its parts: "Acme | Coffee",
// "Acme – Coffee", "Home - Acme".
var titleSeparators = regexp.MustCompile(`[|–—·:]|\s-\s`)

// genericTitleParts are homepage title parts that are not the site's name.
var genericTitleParts = []string{"home", "homepage", "home page", "welcome", "official site", "official website"}

// siteNameOf is the site's stated name, else the shortest part of the
// homepage's title that is not "Home": "Acme | Specialty coffee roasters" is
// Acme.
func siteNameOf(stated, title string) string {
	if stated = strings.TrimSpace(stated); stated != "" {
		return stated
	}
	best := ""
	for _, p := range titleSeparators.Split(title, -1) {
		p = strings.TrimSpace(p)
		if p == "" || isGenericTitlePart(p) {
			continue
		}
		if best == "" || len([]rune(p)) < len([]rune(best)) {
			best = p
		}
	}
	return best
}

func isGenericTitlePart(p string) bool {
	for _, g := range genericTitleParts {
		if strings.EqualFold(p, g) {
			return true
		}
	}
	return false
}

// fetchFavicon asks for the site's icon, after the page images.
func (p *ProcessURLProcessor) fetchFavicon(st *urlState) (StepOutput, error) {
	st.Phase = urlPhaseFavicon
	raw, err := encodeState(st)
	if err != nil {
		return StepOutput{}, err
	}
	src, fetcher := st.Site.FaviconSource, p.fetcher()
	return StepOutput{State: raw, HTTP: &HTTPCall{
		Method: http.MethodGet, URL: src,
		local: func(ctx context.Context) (any, error) {
			data, ct, err := fetcher.Fetch(ctx, src)
			return fetchedImage{data: data, contentType: ct}, err
		},
	}}, nil
}

// faviconFetched copies the icon just downloaded into storage. It is best
// effort: a site whose icon is missing, too big or an SVG simply has none.
func (p *ProcessURLProcessor) faviconFetched(ctx context.Context, in ProcessURLTask, st *urlState, r *StepResult) {
	st.FaviconDone = true
	src := st.Site.FaviconSource
	img, err := localOr(r, func(r *StepResult) (fetchedImage, error) {
		switch {
		case r.Err != "":
			return fetchedImage{}, errors.New(r.Err)
		case r.Status != http.StatusOK:
			return fetchedImage{}, fmt.Errorf("favicon fetch %s: status %d", src, r.Status)
		}
		return fetchedImage{data: r.Body, contentType: r.ContentType}, nil
	})
	ct := strings.ToLower(strings.TrimSpace(strings.SplitN(img.contentType, ";", 2)[0]))
	if err != nil || len(img.data) == 0 || len(img.data) > maxFaviconBytes || !strings.HasPrefix(ct, "image/") || isSVGContentType(ct) {
		return
	}
	key := storage.TenantKey(ctx, fmt.Sprintf("assets/%s/favicon%s", in.AssetID, extForImage(ct, src)))
	if _, err := p.Deps.Storage.Upload(ctx, key, bytes.NewReader(img.data), int64(len(img.data)), ct); err != nil {
		return
	}
	st.Site.FaviconKey = key
}
