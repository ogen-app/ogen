// Package zernio is a thin REST client and integration controller for
// the Zernio.com social-account broker. The package owns:
//
//   - The HTTP client, including bearer-token redaction in error paths
//     and log lines (the API key must never leak via %v / Stringer).
//   - The Integration controller (see integration.go) that tracks
//     boot-time state — disabled / degraded / ok — and is shared between
//     handlers and the background sync worker.
//
// Phase 1 ships only the client skeleton plus the controller. Profile
// bootstrap, connect-link issuance, and account sync are layered on top
// in subsequent phases.
package zernio

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	// defaultBaseURL is used when ZERNIO_BASE_URL is unset. Zernio's
	// REST API lives under /api/v1; the prefix is part of the base
	// URL rather than each path so tests can point a stub server at
	// the same paths without re-deriving the prefix.
	defaultBaseURL = "https://zernio.com/api/v1"
	// defaultTimeout matches the ticket's documented 15s default.
	defaultTimeout = 15 * time.Second
	// errorBodyLimit caps how much of a non-2xx body we read into APIError
	// so an unexpectedly large response can't blow up logs or memory.
	errorBodyLimit = 4096
)

// KeyResolver returns the current Zernio API key. Implementations
// should resolve through the SecretStore on each call so a rotated key
// is picked up without a process restart. An error indicates the key
// is unavailable (missing in DB, decrypt failure, etc.) — outbound
// requests fail cleanly rather than authenticate with stale data.
type KeyResolver func(ctx context.Context) (string, error)

// StaticKey returns a KeyResolver that always yields the supplied key.
// Intended for tests; production wiring goes through the SecretStore.
//
// An empty key returns a nil resolver so NewClient(StaticKey("")) is
// equivalent to "no integration configured" — handlers see a nil
// *Client and surface the existing integration-disabled (409) error
// path. Tests rely on this to model the unconfigured state.
func StaticKey(key string) KeyResolver {
	if key == "" {
		return nil
	}
	return func(context.Context) (string, error) { return key, nil }
}

// Client is the authenticated HTTP wrapper for Zernio's REST API.
//
// The bearer key is fetched on every outbound request via keyResolver
// — never cached on the Client itself — so SecretStore rotations land
// on the next call. accidental %v of a *Client in a log line must not
// leak credentials; String() and BaseURL() are the only intended
// debug surfaces. All outbound requests carry the bearer header;
// non-2xx responses are returned as a typed *APIError.
type Client struct {
	httpClient  *http.Client
	baseURL     string
	keyResolver KeyResolver
	// redirectURL is the post-OAuth landing target sent on every
	// connect-link request. Empty means "use Zernio's default success
	// page". See ClientOpts.RedirectURL for semantics.
	redirectURL string
}

// ClientOpts holds optional Client knobs. Zero-value fields fall back
// to package defaults; passing a zero-value ClientOpts is equivalent
// to omitting it.
type ClientOpts struct {
	// Timeout for individual outbound HTTP requests. Defaults to 15s.
	Timeout time.Duration

	// RedirectURL, when non-empty, is forwarded to Zernio as
	// `redirect_url` on every CreateConnectLink call. Zernio appends
	// ?connected=<platform>&profileId=<id>&accountId=<id>&username=<name>
	// to it after a successful authorization. Useful when the SPA
	// owns a "connection complete" page that should consume those
	// parameters directly.
	RedirectURL string
}

// NewClient constructs a Client. resolver == nil returns nil so
// callers can treat a nil receiver as "integration disabled" — the
// only state where Zernio is permanently off is when no resolver is
// wired (the integration was never registered). A resolver that
// returns "key missing" at call time produces a typed error and is
// not the same as "integration disabled".
func NewClient(resolver KeyResolver, baseURL string, opts ClientOpts) *Client {
	if resolver == nil {
		return nil
	}
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	return &Client{
		httpClient:  &http.Client{Timeout: timeout},
		baseURL:     strings.TrimRight(baseURL, "/"),
		keyResolver: resolver,
		redirectURL: opts.RedirectURL,
	}
}

// String redacts the API key. Important: %v / %+v of the struct itself
// would expose apiKey via reflection — callers should print *Client
// (which uses this method) rather than the dereferenced struct.
func (c *Client) String() string {
	if c == nil {
		return "zernio.Client(disabled)"
	}
	return fmt.Sprintf("zernio.Client(baseURL=%s)", c.baseURL)
}

// BaseURL returns the configured base URL. Useful for log lines that
// want to record where requests were sent without exposing the key.
func (c *Client) BaseURL() string {
	if c == nil {
		return ""
	}
	return c.baseURL
}

// APIError carries the HTTP status and a short message extracted from
// the Zernio response. The full body is intentionally truncated and
// stripped of any URL query strings — connect-link tokens must not
// survive into wrapped errors / logs.
type APIError struct {
	Status  int
	Message string
	// RequiresAddon mirrors the `"requiresAddon": true` flag Zernio sets on
	// the analytics add-on paywall body (CON-153). It lets callers tell an
	// add-on 403 apart from any other 403 (proxy/WAF/gateway) rather than
	// treating every 403 the same. False for bodies without the flag.
	RequiresAddon bool
}

func (e *APIError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("zernio: HTTP %d", e.Status)
	}
	return fmt.Sprintf("zernio: HTTP %d: %s", e.Status, e.Message)
}

// IsStatus reports whether err is an *APIError with the given status.
func IsStatus(err error, status int) bool {
	if apiErr, ok := errors.AsType[*APIError](err); ok {
		return apiErr.Status == status
	}
	return false
}

// Ping issues a low-cost authenticated request to validate the API key
// at boot. 200 → nil; 401 → *APIError{Status:401}; transport errors
// propagate as-is.
//
// Zernio's GET /profiles returns the full list with no pagination
// surface in the documented schema; for a tenant with one or two
// profiles it's effectively free. We discard the body — the only
// signal we care about here is the HTTP status.
func (c *Client) Ping(ctx context.Context) error {
	if c == nil {
		return errors.New("zernio: client is disabled")
	}
	return c.do(ctx, http.MethodGet, "/profiles", nil, nil, nil)
}

// do performs an authenticated request and decodes the JSON body into
// out when out != nil. Returns *APIError for non-2xx responses.
func (c *Client) do(ctx context.Context, method, path string, query url.Values, body, out any) error {
	return c.doHeaders(ctx, method, path, query, nil, body, out)
}

// doHeaders is do with extra request headers merged in after the standard
// bearer/accept/content-type set. It backs the headless connect select step
// (CON-217), which authenticates the list/finalize calls with a short-lived
// X-Connect-Token header in addition to the bearer key.
func (c *Client) doHeaders(ctx context.Context, method, path string, query url.Values, headers map[string]string, body, out any) error {
	u := c.baseURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}

	var bodyReader io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("zernio: marshal request: %w", err)
		}
		bodyReader = bytes.NewReader(buf)
	}

	apiKey, err := c.keyResolver(ctx)
	if err != nil {
		return fmt.Errorf("zernio: resolve api key: %w", err)
	}
	if apiKey == "" {
		return errors.New("zernio: api key not configured")
	}

	req, err := http.NewRequestWithContext(ctx, method, u, bodyReader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	// Caller-supplied headers win over the defaults above (none currently
	// collide; X-Connect-Token is orthogonal to the bearer key).
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, errorBodyLimit))
		return &APIError{Status: resp.StatusCode, Message: shortErrorMessage(raw), RequiresAddon: errorRequiresAddon(raw)}
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return fmt.Errorf("zernio: decode response: %w", err)
		}
	}
	return nil
}

// shortErrorMessage extracts a brief message from a Zernio error body.
// Falls back to a truncated raw body when the shape is unknown.
func shortErrorMessage(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	var env struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(raw, &env); err == nil {
		if env.Error != "" {
			return env.Error
		}
		if env.Message != "" {
			return env.Message
		}
	}
	if len(raw) > 200 {
		raw = raw[:200]
	}
	return string(raw)
}

// errorRequiresAddon reports whether a Zernio error body carries the analytics
// add-on paywall flag (`"requiresAddon": true`, CON-153). A malformed or
// flag-less body reads as false.
func errorRequiresAddon(raw []byte) bool {
	var env struct {
		RequiresAddon bool `json:"requiresAddon"`
	}
	return json.Unmarshal(raw, &env) == nil && env.RequiresAddon
}
