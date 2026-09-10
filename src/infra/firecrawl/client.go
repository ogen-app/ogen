// Package firecrawl is a thin HTTP client for the Firecrawl.dev scrape API
// (https://firecrawl.dev), used to turn a URL into Markdown for URL assets
// (CON-222). It mirrors the Zernio/Resend client conventions: the API key is
// resolved per request via a KeyResolver (so a rotated key is picked up with no
// restart and is never held on the struct), non-2xx responses become a
// classified *APIError (transient vs terminal), and the key never appears in
// log lines.
package firecrawl

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	defaultBaseURL = "https://api.firecrawl.dev"
	defaultTimeout = 90 * time.Second
	// errorBodyLimit caps how much of a non-2xx body we read into the error so
	// an unexpectedly large response can't blow up logs or memory.
	errorBodyLimit = 4096
)

// KeyResolver returns the current Firecrawl API key. It should resolve through
// the secrets store on each call so a rotated key takes effect without a
// restart.
type KeyResolver func(ctx context.Context) (string, error)

// Client is the authenticated Firecrawl HTTP wrapper. The bearer key is fetched
// per request via keyResolver, never cached here.
type Client struct {
	httpClient  *http.Client
	baseURL     string
	keyResolver KeyResolver
}

// New constructs a Client. A nil resolver returns nil so callers can treat a
// nil *Client as "scraping disabled" — mirroring the Zernio/Resend clients.
func New(resolver KeyResolver, baseURL string, timeout time.Duration) *Client {
	if resolver == nil {
		return nil
	}
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	return &Client{
		httpClient:  &http.Client{Timeout: timeout},
		baseURL:     strings.TrimRight(baseURL, "/"),
		keyResolver: resolver,
	}
}

// String redacts everything sensitive (the key isn't stored here, but keep a
// deliberate Stringer so a %v never reflects into the resolver).
func (c *Client) String() string {
	if c == nil {
		return "firecrawl.Client(disabled)"
	}
	return fmt.Sprintf("firecrawl.Client(baseURL=%s)", c.baseURL)
}

// HasKey reports whether a non-empty API key is currently configured. The URL
// endpoint uses it to fail fast with 409 when scraping is unavailable. A
// transient secret-read error is treated as "not configured" for the gate; the
// worker's own retry loop covers a key that appears moments later.
func (c *Client) HasKey(ctx context.Context) bool {
	if c == nil {
		return false
	}
	key, err := c.keyResolver(ctx)
	return err == nil && key != ""
}

// ScrapeRequest is the input to Scrape. Only URL is required; MaxAge, when set,
// overrides Firecrawl's default cache window (pass a pointer to 0 on refresh so
// the upstream cache is bypassed and the page is re-fetched fresh).
type ScrapeRequest struct {
	URL    string
	MaxAge *int // milliseconds; nil = Firecrawl default (2-day cache)
}

// ScrapeResult is the normalised subset of Firecrawl's response we persist.
type ScrapeResult struct {
	Markdown    string
	Title       string
	Description string
	SourceURL   string // the final URL after redirects (metadata.sourceURL)
	StatusCode  int
	ContentType string
}

// scrapeBody is the POST /v2/scrape request body. Field names match Firecrawl's
// API. onlyMainContent + blockAds are on by default upstream but set explicitly
// so behaviour is pinned regardless of upstream default changes.
type scrapeBody struct {
	URL             string   `json:"url"`
	Formats         []string `json:"formats"`
	OnlyMainContent bool     `json:"onlyMainContent"`
	BlockAds        bool     `json:"blockAds"`
	MaxAge          *int     `json:"maxAge,omitempty"`
}

// scrapeResponse is the envelope Firecrawl returns for a single-URL scrape.
type scrapeResponse struct {
	Success bool   `json:"success"`
	Error   string `json:"error"`
	Data    struct {
		Markdown string `json:"markdown"`
		Metadata struct {
			// title/description may arrive as a string or an array of strings
			// depending on the page's meta tags — flexString tolerates both.
			Title       flexString `json:"title"`
			Description flexString `json:"description"`
			SourceURL   string     `json:"sourceURL"`
			StatusCode  int        `json:"statusCode"`
			ContentType string     `json:"contentType"`
		} `json:"metadata"`
	} `json:"data"`
}

// Scrape fetches url and returns its Markdown + page metadata. A missing key,
// network error, 429, or 5xx yields a transient *APIError (the caller retries);
// a 4xx or success:false yields a terminal one (the page won't scrape on a
// re-attempt).
func (c *Client) Scrape(ctx context.Context, in ScrapeRequest) (*ScrapeResult, error) {
	if c == nil {
		return nil, &APIError{Message: "firecrawl: client disabled"}
	}
	key, err := c.keyResolver(ctx)
	if err != nil {
		return nil, &APIError{Transient: true, Message: fmt.Sprintf("firecrawl: resolve api key: %v", err)}
	}
	if key == "" {
		return nil, &APIError{Transient: true, Message: "firecrawl: api key not configured"}
	}

	buf, err := json.Marshal(scrapeBody{
		URL:             in.URL,
		Formats:         []string{"markdown"},
		OnlyMainContent: true,
		BlockAds:        true,
		MaxAge:          in.MaxAge,
	})
	if err != nil {
		return nil, &APIError{Message: fmt.Sprintf("firecrawl: marshal request: %v", err)}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v2/scrape", bytes.NewReader(buf))
	if err != nil {
		return nil, &APIError{Message: err.Error()}
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		// Network / timeout: retryable.
		return nil, &APIError{Transient: true, Message: err.Error()}
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, errorBodyLimit))
		return nil, &APIError{
			// 5xx and 429 are worth retrying; other 4xx (bad url, blocked page,
			// bad key) are terminal.
			Transient: resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests,
			Status:    resp.StatusCode,
			Message:   shortErrorMessage(raw),
		}
	}

	var out scrapeResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(&out); err != nil {
		return nil, &APIError{Transient: true, Message: fmt.Sprintf("firecrawl: decode response: %v", err)}
	}
	if !out.Success {
		msg := out.Error
		if msg == "" {
			msg = "firecrawl: scrape unsuccessful"
		}
		// The request was well-formed but the page couldn't be scraped — terminal.
		return nil, &APIError{Status: resp.StatusCode, Message: msg}
	}

	return &ScrapeResult{
		Markdown:    out.Data.Markdown,
		Title:       out.Data.Metadata.Title.String(),
		Description: out.Data.Metadata.Description.String(),
		SourceURL:   out.Data.Metadata.SourceURL,
		StatusCode:  out.Data.Metadata.StatusCode,
		ContentType: out.Data.Metadata.ContentType,
	}, nil
}

// maxResponseBytes bounds a successful scrape body. A page's Markdown is
// text-only and onlyMainContent-trimmed; 32 MiB is far past any real article
// and still guards against a pathological response.
const maxResponseBytes = 32 << 20

// APIError is a non-2xx (or transport) Firecrawl failure. Transient reports
// whether a retry could plausibly succeed.
type APIError struct {
	Status    int
	Message   string
	Transient bool
}

func (e *APIError) Error() string {
	if e.Status != 0 {
		return fmt.Sprintf("firecrawl: status %d: %s", e.Status, e.Message)
	}
	return e.Message
}

// IsTransient reports whether err is a retryable Firecrawl error. Non-APIError
// values are treated as terminal.
func IsTransient(err error) bool {
	if ae, ok := errors.AsType[*APIError](err); ok {
		return ae.Transient
	}
	return false
}

// flexString decodes a JSON value that may be either a string or an array of
// strings (Firecrawl returns title/description as either). The array form is
// joined with " · " so nothing is silently dropped.
type flexString string

func (f *flexString) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || string(b) == "null" {
		return nil
	}
	if b[0] == '[' {
		var arr []string
		if err := json.Unmarshal(b, &arr); err != nil {
			return err
		}
		*f = flexString(strings.Join(arr, " · "))
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	*f = flexString(s)
	return nil
}

func (f flexString) String() string { return string(f) }

// shortErrorMessage extracts a brief message from a Firecrawl error body,
// falling back to a truncated raw body when the shape is unknown.
func shortErrorMessage(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	var env struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(raw, &env); err == nil {
		switch {
		case env.Error != "":
			return env.Error
		case env.Message != "":
			return env.Message
		}
	}
	if len(raw) > 200 {
		raw = raw[:200]
	}
	return string(raw)
}
