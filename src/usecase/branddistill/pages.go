package branddistill

import (
	"net/url"
	"path"
	"slices"
	"strings"

	"github.com/ogen-app/ogen/src/infra/firecrawl"
	"github.com/ogen-app/ogen/src/usecase/urlimport"
)

// MaxWebsitePages caps the pages a website read imports.
const MaxWebsitePages = 10

// pageKind is what a page is likely for, judged from its path and title.
type pageKind int

const (
	kindOther pageKind = iota
	kindAbout
	kindProduct
	kindPricing
	kindLegal
	kindArticle
)

// kindQuota is how many pages of each kind a read takes before the rest of
// the budget goes to the site's other top-level pages.
var kindQuota = map[pageKind]int{kindAbout: 1, kindProduct: 3, kindPricing: 1, kindLegal: 2, kindArticle: 2}

// kindOrder is the order the kinds are taken in after the homepage.
var kindOrder = []pageKind{kindAbout, kindProduct, kindPricing, kindLegal, kindArticle}

var kindWords = []struct {
	kind  pageKind
	words []string
}{
	{kindLegal, []string{"terms", "legal", "disclaimer", "disclaimers", "compliance", "imprint", "impressum", "terms-of-sale", "terms-of-service", "terms-and-conditions"}},
	{kindPricing, []string{"pricing", "prices", "plans", "price"}},
	{kindAbout, []string{"about", "about-us", "company", "story", "our-story", "mission", "who-we-are", "values", "manifesto"}},
	{kindProduct, []string{"product", "products", "features", "solutions", "services", "platform", "how-it-works", "shop", "use-cases"}},
}

// articleSections are path roots whose children are articles, read for how
// the brand writes at length.
var articleSections = []string{"blog", "news", "insights", "journal", "articles", "stories", "resources", "magazine", "posts"}

// skipWords are path segments of pages with no brand to read: accounts,
// carts, listings and boilerplate. Privacy and cookie policies are legal text
// every site shares.
var skipWords = []string{
	"login", "log-in", "signin", "sign-in", "signup", "sign-up", "register", "logout",
	"cart", "basket", "checkout", "account", "my-account", "careers", "jobs",
	"privacy", "privacy-policy", "cookie", "cookies", "cookie-policy",
	"search", "tag", "tags", "category", "categories", "author", "feed", "rss",
	"wp-admin", "wp-content", "wp-json", "cdn-cgi", "sitemap",
}

// skipExtensions are files, not pages.
var skipExtensions = []string{".xml", ".pdf", ".jpg", ".jpeg", ".png", ".gif", ".svg", ".webp", ".zip", ".mp4", ".mp3", ".css", ".js", ".json", ".txt", ".ico"}

type candidate struct {
	url   string
	kind  pageKind
	depth int
}

// SelectPages picks at most limit pages of a site to read: the homepage, the
// page the user gave when it is not the homepage, then pages by kind (about,
// what is sold, pricing, legal, a couple of articles), then the site's other
// top-level pages. links is the site map in its own order; only pages on the
// given host (www or not) are taken, without query strings, files or
// account pages. The result is canonical URLs in reading order.
func SelectPages(given string, links []firecrawl.Link, limit int) []string {
	root, host, ok := siteRoot(given)
	if !ok || limit <= 0 {
		return nil
	}
	picked := []string{root}
	seen := map[string]bool{root: true}
	take := func(u string) bool {
		if len(picked) >= limit || seen[u] {
			return false
		}
		seen[u] = true
		picked = append(picked, u)
		return true
	}
	// "https://acme.com" is the homepage too.
	if u, _, err := urlimport.Canonical(given); err == nil && u+"/" != root {
		take(u)
	}

	var pool []candidate
	for _, l := range links {
		if c, ok := classify(l, host); ok && !seen[c.url] {
			pool = append(pool, c)
		}
	}
	for _, kind := range kindOrder {
		n := 0
		for _, c := range pool {
			if c.kind == kind && n < kindQuota[kind] && take(c.url) {
				n++
			}
		}
	}
	// The rest of the budget: other top-level pages first, then deeper ones
	// that are not articles.
	for _, depth := range []int{1, 2} {
		for _, c := range pool {
			if c.kind == kindOther && c.depth == depth {
				take(c.url)
			}
		}
	}
	return picked
}

// siteRoot is the homepage of the site the URL is on and its host without
// www.
func siteRoot(given string) (root, host string, ok bool) {
	canon, _, err := urlimport.Canonical(given)
	if err != nil {
		return "", "", false
	}
	u, err := url.Parse(canon)
	if err != nil {
		return "", "", false
	}
	return u.Scheme + "://" + u.Host + "/", bareHost(u.Hostname()), true
}

func bareHost(h string) string { return strings.TrimPrefix(strings.ToLower(h), "www.") }

// classify reads a site-map link; ok is false for a page not worth reading.
func classify(l firecrawl.Link, host string) (candidate, bool) {
	canon, _, err := urlimport.Canonical(l.URL)
	if err != nil {
		return candidate{}, false
	}
	u, err := url.Parse(canon)
	if err != nil || u.RawQuery != "" || bareHost(u.Hostname()) != host {
		return candidate{}, false
	}
	p := strings.ToLower(u.Path)
	if slices.Contains(skipExtensions, path.Ext(p)) {
		return candidate{}, false
	}
	segs := strings.FieldsFunc(p, func(r rune) bool { return r == '/' })
	if len(segs) == 0 {
		return candidate{}, false // the homepage is always taken
	}
	for _, s := range segs {
		if slices.Contains(skipWords, s) {
			return candidate{}, false
		}
	}
	c := candidate{url: canon, depth: len(segs), kind: kindOther}
	if len(segs) >= 2 && slices.Contains(articleSections, segs[0]) {
		c.kind = kindArticle
		return c, true
	}
	if slices.Contains(articleSections, segs[0]) {
		return candidate{}, false // an article index lists, it does not write
	}
	words := append(slices.Clone(segs), strings.Fields(strings.ToLower(l.Title))...)
	for _, kw := range kindWords {
		for _, w := range words {
			if slices.Contains(kw.words, w) {
				c.kind = kw.kind
				return c, true
			}
		}
	}
	return c, true
}
