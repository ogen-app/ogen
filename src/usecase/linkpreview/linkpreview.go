// Package linkpreview reads a web page's Open Graph tags so the composer can
// show the card a network will build for a link post. The result is an
// approximation: the network fetches and caches the page itself at publish
// time.
package linkpreview

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/html"

	"github.com/ogen-app/ogen/src/domain/platforms"
)

// ErrInvalidURL marks a URL the service refuses to fetch: malformed, not
// http(s), or pointing at a non-public address.
var ErrInvalidURL = errors.New("linkpreview: invalid url")

const (
	maxBodyBytes  = 1 << 20
	successTTL    = 24 * time.Hour
	failureTTL    = 10 * time.Minute
	maxCacheItems = 2048
	userAgent     = "Mozilla/5.0 (compatible; OgenLinkPreview/1.0; +https://getogen.com)"
)

// Preview is the card data for one URL. Only Domain is guaranteed: a page
// that can't be fetched or carries no tags yields a card naming its host.
type Preview struct {
	URL         string `json:"url"`
	FinalURL    string `json:"final_url,omitempty"`
	Domain      string `json:"domain"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	ImageURL    string `json:"image_url,omitempty"`
	SiteName    string `json:"site_name,omitempty"`
}

// HostCheck rejects a host the service must not fetch.
type HostCheck func(ctx context.Context, host string) error

type entry struct {
	preview Preview
	expires time.Time
}

// Service fetches and caches previews. Safe for concurrent use.
type Service struct {
	client    *http.Client
	allowHost HostCheck
	now       func() time.Time

	mu    sync.Mutex
	cache map[string]entry
}

// New builds a Service. client must enforce the outbound network policy
// (netguard.SafeClient in production); allowHost is the cheap pre-flight
// check run before any fetch.
func New(client *http.Client, allowHost HostCheck) *Service {
	return &Service{client: client, allowHost: allowHost, now: time.Now, cache: map[string]entry{}}
}

// Get returns the preview for rawURL. The error is non-nil only for a URL the
// service refuses (ErrInvalidURL); a failed fetch degrades to a domain-only
// preview, cached briefly so a broken page isn't refetched on every keystroke.
func (s *Service) Get(ctx context.Context, rawURL string) (Preview, error) {
	u, err := parse(rawURL)
	if err != nil {
		return Preview{}, err
	}
	if err := s.allowHost(ctx, u.Hostname()); err != nil {
		return Preview{}, fmt.Errorf("%w: %v", ErrInvalidURL, err)
	}
	key := u.String()
	if p, ok := s.cached(key); ok {
		return p, nil
	}

	p := Preview{URL: key, Domain: domainOf(u)}
	ttl := failureTTL
	if fetched, err := s.fetch(ctx, u); err == nil {
		p, ttl = fetched, successTTL
	}
	s.store(key, p, ttl)
	return p, nil
}

func parse(rawURL string) (*url.URL, error) {
	raw := strings.TrimSpace(rawURL)
	if err := platforms.ValidateLink(raw); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidURL, err)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidURL, err)
	}
	u.Fragment = ""
	u.Host = strings.ToLower(u.Host)
	return u, nil
}

func domainOf(u *url.URL) string {
	return strings.TrimPrefix(u.Hostname(), "www.")
}

func (s *Service) cached(key string) (Preview, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.cache[key]
	if !ok || s.now().After(e.expires) {
		return Preview{}, false
	}
	return e.preview, true
}

func (s *Service) store(key string, p Preview, ttl time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.cache) >= maxCacheItems {
		s.evictLocked()
	}
	s.cache[key] = entry{preview: p, expires: s.now().Add(ttl)}
}

// evictLocked drops expired entries, or an arbitrary one when none have
// expired, so the cache stays bounded.
func (s *Service) evictLocked() {
	now := s.now()
	for k, e := range s.cache {
		if now.After(e.expires) {
			delete(s.cache, k)
		}
	}
	if len(s.cache) < maxCacheItems {
		return
	}
	for k := range s.cache {
		delete(s.cache, k)
		return
	}
}

func (s *Service) fetch(ctx context.Context, u *url.URL) (Preview, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return Preview{}, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	resp, err := s.client.Do(req)
	if err != nil {
		return Preview{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return Preview{}, fmt.Errorf("status %d", resp.StatusCode)
	}
	if mt, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type")); mt != "text/html" && mt != "application/xhtml+xml" {
		return Preview{}, fmt.Errorf("content type %q", mt)
	}

	final := resp.Request.URL
	tags := parseHead(io.LimitReader(resp.Body, maxBodyBytes))
	return Preview{
		URL:         u.String(),
		FinalURL:    final.String(),
		Domain:      domainOf(final),
		Title:       tags.first("og:title", "twitter:title", "title"),
		Description: tags.first("og:description", "twitter:description", "description"),
		ImageURL:    absoluteHTTP(final, tags.first("og:image:secure_url", "og:image", "og:image:url", "twitter:image", "twitter:image:src")),
		SiteName:    tags.first("og:site_name"),
	}, nil
}

// absoluteHTTP resolves ref against base, keeping it only when the result is
// an http(s) URL.
func absoluteHTTP(base *url.URL, ref string) string {
	if ref == "" {
		return ""
	}
	r, err := base.Parse(ref)
	if err != nil || (r.Scheme != "http" && r.Scheme != "https") {
		return ""
	}
	return r.String()
}

// headTags maps a lower-cased meta property/name (or "title") to its first
// non-empty value.
type headTags map[string]string

func (t headTags) first(keys ...string) string {
	for _, k := range keys {
		if v := t[k]; v != "" {
			return v
		}
	}
	return ""
}

func (t headTags) set(key, val string) {
	key = strings.ToLower(strings.TrimSpace(key))
	val = strings.Join(strings.Fields(val), " ")
	if key == "" || val == "" || t[key] != "" {
		return
	}
	t[key] = val
}

// parseHead tokenizes the document up to <body> (or EOF), collecting meta
// property/name content and the <title> text.
func parseHead(r io.Reader) headTags {
	tags := headTags{}
	z := html.NewTokenizer(r)
	inTitle := false
	for {
		switch z.Next() {
		case html.ErrorToken:
			return tags
		case html.StartTagToken, html.SelfClosingTagToken:
			tok := z.Token()
			switch tok.Data {
			case "body":
				return tags
			case "title":
				inTitle = true
			case "meta":
				collectMeta(tags, tok.Attr)
			}
		case html.EndTagToken:
			if tok := z.Token(); tok.Data == "head" {
				return tags
			}
			inTitle = false
		case html.TextToken:
			if inTitle {
				tags.set("title", string(z.Text()))
			}
		}
	}
}

func collectMeta(tags headTags, attrs []html.Attribute) {
	var key, content string
	for _, a := range attrs {
		switch strings.ToLower(a.Key) {
		case "property", "name":
			if key == "" {
				key = a.Val
			}
		case "content":
			content = a.Val
		}
	}
	tags.set(key, content)
}
