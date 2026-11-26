// Package polar is the Polar adapter behind billing.Provider.
//
// It is a hand-written client over net/http: Polar's Go SDK is archived and
// the API is plain JSON. Every request is pinned to one API version
// (Polar-Version), and the sandbox and production APIs are separate hosts
// with separate organizations and tokens. The access token and webhook
// secret are resolved per call from the secret store, so a Harbor rotation
// applies without a restart; neither is ever logged. Decoding is tolerant:
// unknown fields are ignored.
//
// Points marked "spike" in comments rest on Polar's documentation and are
// confirmed against the sandbox before this adapter carries real money.
package polar

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/ogen-app/ogen/src/domain/billing"
)

const (
	// ProductionBaseURL and SandboxBaseURL are Polar's two APIs.
	ProductionBaseURL = "https://api.polar.sh/v1"
	SandboxBaseURL    = "https://sandbox-api.polar.sh/v1"

	defaultTimeout = 15 * time.Second
	errorBodyLimit = 4 << 10
	// pageLimit is Polar's largest page.
	pageLimit = 100
	// versionHeader pins the API version of a request.
	versionHeader = "Polar-Version"
)

// SecretResolver returns a secret's current value; "" means not set.
type SecretResolver func(ctx context.Context) (string, error)

// APIError is a non-2xx answer from the API.
type APIError struct {
	Status int
	// Type is Polar's error name, e.g. ResourceNotFound or SubscriptionLocked.
	Type   string
	Detail string
}

func (e *APIError) Error() string {
	msg := fmt.Sprintf("polar: HTTP %d", e.Status)
	if e.Type != "" {
		msg += " " + e.Type
	}
	if e.Detail != "" {
		msg += ": " + e.Detail
	}
	return msg
}

// Retryable reports whether a retry may succeed: throttling, server errors,
// and a subscription locked by a change in flight.
func (e *APIError) Retryable() bool {
	return e.Status == http.StatusTooManyRequests || e.Status >= 500 ||
		(e.Status == http.StatusConflict && e.Type == "SubscriptionLocked")
}

// HTTPStatus is the answer's status, for metrics.
func (e *APIError) HTTPStatus() int { return e.Status }

// Locked reports whether the subscription is locked by another change.
func (e *APIError) Locked() bool {
	return e.Status == http.StatusConflict && e.Type == "SubscriptionLocked"
}

// Client implements billing.Provider and billing.Remote for Polar.
type Client struct {
	http          *http.Client
	baseURL       string // overrides the per-mode hosts when set (tests, plans)
	testMode      bool
	apiVersion    string
	orgID         string
	liveOrgID     string
	accessToken   SecretResolver
	liveToken     SecretResolver
	webhookSecret SecretResolver
	now           func() time.Time
	opts          Options
}

var (
	_ billing.Provider = (*Client)(nil)
	_ billing.Remote   = (*Client)(nil)
)

// Options configures a client.
type Options struct {
	// AccessToken is the Organization Access Token of the mode the server
	// runs in (TestMode).
	AccessToken SecretResolver
	// LiveAccessToken reads production products while the server still runs
	// in the sandbox, so the live product map can be prepared before the
	// switch. Optional; it needs only products:read.
	LiveAccessToken SecretResolver
	WebhookSecret   SecretResolver
	// OrganizationID is the organization of the running mode;
	// LiveOrganizationID the production one, read with LiveAccessToken.
	OrganizationID     string
	LiveOrganizationID string
	TestMode           bool
	APIVersion         string
	// BaseURL replaces both hosts (tests). Timeout falls back to 15s.
	BaseURL string
	Timeout time.Duration
	// Now defaults to time.Now.
	Now func() time.Time
}

// New builds a client.
func New(o Options) *Client {
	timeout := o.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	now := o.Now
	if now == nil {
		now = time.Now
	}
	return &Client{
		http:          &http.Client{Timeout: timeout},
		baseURL:       strings.TrimRight(o.BaseURL, "/"),
		testMode:      o.TestMode,
		apiVersion:    cmp.Or(o.APIVersion, billing.DefaultPolarAPIVersion),
		orgID:         o.OrganizationID,
		liveOrgID:     o.LiveOrganizationID,
		accessToken:   o.AccessToken,
		liveToken:     o.LiveAccessToken,
		webhookSecret: o.WebhookSecret,
		now:           now,
		opts:          o,
	}
}

// String never includes credentials.
func (c *Client) String() string {
	return fmt.Sprintf("polar.Client(test_mode=%t, api_version=%s)", c.testMode, c.apiVersion)
}

// base is the API root of a mode.
func (c *Client) base(testMode bool) string {
	switch {
	case c.baseURL != "":
		return c.baseURL
	case testMode:
		return SandboxBaseURL
	default:
		return ProductionBaseURL
	}
}

// credentials returns the token and organization that read a mode, or a nil
// token when there is none: the server's own token reads its mode, and the
// live token reads production during the sandbox period. The sandbox cannot
// be read once the server runs live.
func (c *Client) credentials(testMode bool) (SecretResolver, string) {
	switch {
	case testMode == c.testMode:
		return c.accessToken, c.orgID
	case !testMode:
		return c.liveToken, c.liveOrgID
	}
	return nil, ""
}

// request is one API call.
type request struct {
	testMode bool
	method   string
	path     string
	query    url.Values
	body     any
}

func (c *Client) do(ctx context.Context, r request, out any) error {
	token, _ := c.credentials(r.testMode)
	if token == nil {
		return billing.ErrNotConfigured
	}
	key, err := token(ctx)
	if err != nil {
		return fmt.Errorf("polar: resolve access token: %w", err)
	}
	if key == "" {
		return billing.ErrNotConfigured
	}
	var reader io.Reader
	if r.body != nil {
		buf, err := json.Marshal(r.body)
		if err != nil {
			return fmt.Errorf("polar: marshal request: %w", err)
		}
		reader = bytes.NewReader(buf)
	}
	target := c.base(r.testMode) + r.path
	if len(r.query) > 0 {
		target += "?" + r.query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, r.method, target, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Accept", "application/json")
	req.Header.Set(versionHeader, c.apiVersion)
	if r.body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		// The URL holds no secret; the token is only in a header.
		return fmt.Errorf("polar: %s %s: %w", r.method, r.path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, errorBodyLimit))
		return statusError(r, resp.StatusCode, raw)
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return fmt.Errorf("polar: decode response: %w", err)
		}
	}
	return nil
}

// statusError turns an error answer into the neutral errors callers test
// for, keeping the API error for the log.
func statusError(r request, status int, raw []byte) error {
	apiErr := decodeAPIError(status, raw)
	switch {
	case status == http.StatusNotFound:
		return fmt.Errorf("%w: %s %s: %w", billing.ErrNotFound, r.method, r.path, apiErr)
	case status == http.StatusPaymentRequired:
		return fmt.Errorf("%w: %w", billing.ErrPaymentFailed, apiErr)
	case status == http.StatusForbidden && apiErr.Type == "AlreadyCanceled":
		return fmt.Errorf("%w: %w", billing.ErrAlreadyCanceled, apiErr)
	}
	return apiErr
}

// decodeAPIError reads Polar's error body: {"error": "...", "detail": "..."},
// or a validation error whose detail is a list.
func decodeAPIError(status int, raw []byte) *APIError {
	var env struct {
		Error  string          `json:"error"`
		Detail json.RawMessage `json:"detail"`
	}
	e := &APIError{Status: status}
	if json.Unmarshal(raw, &env) != nil {
		e.Detail = truncate(string(raw))
		return e
	}
	e.Type = env.Error
	var detail string
	if json.Unmarshal(env.Detail, &detail) == nil {
		e.Detail = detail
		return e
	}
	var list []struct {
		Loc []any  `json:"loc"`
		Msg string `json:"msg"`
	}
	if json.Unmarshal(env.Detail, &list) == nil && len(list) > 0 {
		e.Detail = fmt.Sprintf("%s (at %v)", list[0].Msg, list[0].Loc)
		return e
	}
	e.Detail = truncate(string(env.Detail))
	return e
}

func truncate(s string) string {
	if len(s) > 200 {
		return s[:200]
	}
	return s
}

// page is Polar's list envelope.
type page[T any] struct {
	Items      []T `json:"items"`
	Pagination struct {
		TotalCount int `json:"total_count"`
		MaxPage    int `json:"max_page"`
	} `json:"pagination"`
}

func listQuery(orgID string, pageNo int, extra url.Values) url.Values {
	q := url.Values{}
	maps.Copy(q, extra)
	if orgID != "" {
		q.Set("organization_id", orgID)
	}
	q.Set("page", strconv.Itoa(pageNo))
	q.Set("limit", strconv.Itoa(pageLimit))
	return q
}

// errNoID guards a decoded object without an id.
var errNoID = errors.New("polar: object has no id")
