// Package anthropicadmin reads Anthropic's Admin API cost report
// (GET /v1/organizations/cost_report): what Anthropic actually charged per
// day, which Harbor reconciles against Ogen's token-pipeline estimate.
//
// The Admin key (sk-ant-admin…) is not the API key the flows use. It is
// resolved per call through a KeyResolver, so a rotated key applies without a
// restart and is never held on the struct.
package anthropicadmin

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	defaultBaseURL = "https://api.anthropic.com"
	defaultTimeout = 30 * time.Second
	apiVersion     = "2023-06-01"
	// pageLimit is the report's maximum buckets per page.
	pageLimit = 31
	// maxPages bounds paging should next_page never run out.
	maxPages = 32
	// maxAttempts is how often one page is tried on 429, 5xx or a network
	// error before the call gives up with ErrUnavailable.
	maxAttempts = 4
	baseBackoff = time.Second
	// maxBackoff caps one wait. A Retry-After longer than that gives up at
	// once rather than hold the caller's RPC open; the caller retries later.
	maxBackoff    = 30 * time.Second
	errorBodySize = 4096
	dateLayout    = time.DateOnly
)

var (
	// ErrNotConfigured means no Admin key is set.
	ErrNotConfigured = errors.New("anthropicadmin: admin key not configured")
	// ErrUnauthorized means Anthropic rejected the key (401), usually
	// because an API key was stored instead of an Admin key.
	ErrUnauthorized = errors.New("anthropicadmin: admin key rejected")
	// ErrForbidden means the key authenticated but may not read the cost
	// report (403): an organization or role permission, not a wrong key.
	ErrForbidden = errors.New("anthropicadmin: admin key may not read the cost report")
	// ErrUnavailable means Anthropic stayed down or rate-limited through
	// every retry; the caller retries later.
	ErrUnavailable = errors.New("anthropicadmin: cost report unavailable")
)

// KeyResolver returns the current Admin key, or "" when none is set.
type KeyResolver func(ctx context.Context) (string, error)

// DailyCost is Anthropic's charge for one model and cost type on one UTC day,
// summed over token types, service tiers and workspaces.
type DailyCost struct {
	// Date is the UTC day, YYYY-MM-DD.
	Date string
	// Model is "" for costs not tied to a model, such as web search.
	Model    string
	CostType string
	// USDMicros is the exact sum of the report's fractional cents, rounded
	// half away from zero once.
	USDMicros int64
}

// Client calls the cost report.
type Client struct {
	httpClient  *http.Client
	baseURL     string
	keyResolver KeyResolver
	// sleep waits between retries; tests replace it.
	sleep func(ctx context.Context, d time.Duration) error
}

// New builds a Client. An empty baseURL means api.anthropic.com.
func New(resolver KeyResolver, baseURL string, timeout time.Duration) *Client {
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	return &Client{
		httpClient:  &http.Client{Timeout: timeout},
		baseURL:     strings.TrimRight(cmp.Or(baseURL, defaultBaseURL), "/"),
		keyResolver: resolver,
		sleep:       sleepCtx,
	}
}

// DailyCosts returns the costs for the UTC days from through to, both
// inclusive, ordered by date, model and cost type. Entries that sum to zero
// are left out.
func (c *Client) DailyCosts(ctx context.Context, from, to time.Time) ([]DailyCost, error) {
	key, err := c.keyResolver(ctx)
	if err != nil {
		return nil, fmt.Errorf("anthropicadmin: resolve admin key: %w", err)
	}
	if key == "" {
		return nil, ErrNotConfigured
	}
	from, to = utcDay(from), utcDay(to)
	sums := map[costKey]*big.Rat{}
	page := ""
	for range maxPages {
		rep, err := c.fetchPage(ctx, key, from, to.AddDate(0, 0, 1), page)
		if err != nil {
			return nil, err
		}
		if err := addBuckets(sums, rep.Data, from, to); err != nil {
			return nil, err
		}
		if !rep.HasMore || rep.NextPage == "" {
			return toDailyCosts(sums)
		}
		page = rep.NextPage
	}
	return nil, fmt.Errorf("anthropicadmin: cost report still paging after %d pages", maxPages)
}

type costKey struct{ date, model, costType string }

type report struct {
	Data     []bucket `json:"data"`
	HasMore  bool     `json:"has_more"`
	NextPage string   `json:"next_page"`
}

type bucket struct {
	StartingAt time.Time `json:"starting_at"`
	Results    []result  `json:"results"`
}

type result struct {
	Amount   string  `json:"amount"`
	Currency string  `json:"currency"`
	Model    *string `json:"model"`
	CostType *string `json:"cost_type"`
}

// addBuckets adds each result's cents to its day, model and cost type.
// Buckets outside [from, to] are ignored, should the report widen the range.
func addBuckets(sums map[costKey]*big.Rat, buckets []bucket, from, to time.Time) error {
	for _, b := range buckets {
		day := utcDay(b.StartingAt)
		if day.Before(from) || day.After(to) {
			continue
		}
		for _, r := range b.Results {
			if r.Currency != "" && r.Currency != "USD" {
				return fmt.Errorf("anthropicadmin: unexpected currency %q", r.Currency)
			}
			cents, ok := new(big.Rat).SetString(r.Amount)
			if !ok {
				return fmt.Errorf("anthropicadmin: bad amount %q", r.Amount)
			}
			k := costKey{date: day.Format(dateLayout), model: deref(r.Model), costType: deref(r.CostType)}
			if sums[k] == nil {
				sums[k] = new(big.Rat)
			}
			sums[k].Add(sums[k], cents)
		}
	}
	return nil
}

func toDailyCosts(sums map[costKey]*big.Rat) ([]DailyCost, error) {
	out := make([]DailyCost, 0, len(sums))
	for k, cents := range sums {
		micros, err := centsToMicros(cents)
		if err != nil {
			return nil, fmt.Errorf("anthropicadmin: %s %q %q: %w", k.date, k.model, k.costType, err)
		}
		if micros == 0 {
			continue
		}
		out = append(out, DailyCost{Date: k.date, Model: k.model, CostType: k.costType, USDMicros: micros})
	}
	slices.SortFunc(out, func(a, b DailyCost) int {
		return cmp.Or(cmp.Compare(a.Date, b.Date), cmp.Compare(a.Model, b.Model), cmp.Compare(a.CostType, b.CostType))
	})
	return out, nil
}

// centsToMicros converts cents to USD micros (1 cent = 10,000 micros),
// rounding half away from zero. A sum outside int64 is an error.
func centsToMicros(cents *big.Rat) (int64, error) {
	micros := new(big.Rat).Mul(cents, big.NewRat(10_000, 1))
	q, r := new(big.Int).QuoRem(micros.Num(), micros.Denom(), new(big.Int))
	if new(big.Int).Mul(new(big.Int).Abs(r), big.NewInt(2)).Cmp(micros.Denom()) >= 0 {
		q.Add(q, big.NewInt(int64(micros.Sign())))
	}
	if !q.IsInt64() {
		return 0, fmt.Errorf("%s cents overflows int64 micros", cents.FloatString(5))
	}
	return q.Int64(), nil
}

// fetchPage gets one page, retrying 429, 5xx and network errors with
// exponential backoff, or after the server's Retry-After when that is at most
// maxBackoff; a longer Retry-After ends the call with ErrUnavailable.
func (c *Client) fetchPage(ctx context.Context, key string, start, end time.Time, page string) (*report, error) {
	q := url.Values{}
	q.Set("starting_at", start.Format(time.RFC3339))
	q.Set("ending_at", end.Format(time.RFC3339))
	q.Set("bucket_width", "1d")
	q.Add("group_by[]", "description")
	q.Set("limit", strconv.Itoa(pageLimit))
	if page != "" {
		q.Set("page", page)
	}
	endpoint := c.baseURL + "/v1/organizations/cost_report?" + q.Encode()

	var lastErr error
	for attempt := range maxAttempts {
		if attempt > 0 {
			wait, ok := backoff(attempt, lastErr)
			if !ok {
				break
			}
			if err := c.sleep(ctx, wait); err != nil {
				return nil, err
			}
		}
		rep, err := c.get(ctx, key, endpoint)
		if err == nil {
			return rep, nil
		}
		if ctx.Err() != nil || !retryable(err) {
			return nil, err
		}
		lastErr = err
	}
	return nil, fmt.Errorf("%w: %w", ErrUnavailable, lastErr)
}

// statusError is a non-2xx response worth classifying.
type statusError struct {
	code       int
	retryAfter time.Duration
	msg        string
}

func (e *statusError) Error() string {
	return fmt.Sprintf("anthropicadmin: cost report: status %d: %s", e.code, e.msg)
}

func (c *Client) get(ctx context.Context, key, endpoint string) (*report, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("x-api-key", key)
	req.Header.Set("anthropic-version", apiVersion)
	req.Header.Set("Accept", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("anthropicadmin: cost report: %w", err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusUnauthorized:
		return nil, ErrUnauthorized
	case http.StatusForbidden:
		return nil, ErrForbidden
	}
	if resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, errorBodySize))
		return nil, &statusError{code: resp.StatusCode, retryAfter: parseRetryAfter(resp.Header.Get("Retry-After")), msg: errorMessage(raw)}
	}
	var rep report
	if err := json.NewDecoder(resp.Body).Decode(&rep); err != nil {
		return nil, fmt.Errorf("anthropicadmin: decode cost report: %w", err)
	}
	return &rep, nil
}

func retryable(err error) bool {
	if se, ok := errors.AsType[*statusError](err); ok {
		return se.code == http.StatusTooManyRequests || se.code >= 500
	}
	// Transport errors, the client timeout among them, are retried; decode
	// errors, ErrUnauthorized and ErrForbidden are not.
	_, isURL := errors.AsType[*url.Error](err)
	return isURL
}

// backoff is the wait before the given retry; false means the server asked
// for a longer wait than maxBackoff.
func backoff(attempt int, lastErr error) (time.Duration, bool) {
	if se, ok := errors.AsType[*statusError](lastErr); ok && se.retryAfter > 0 {
		return se.retryAfter, se.retryAfter <= maxBackoff
	}
	return min(baseBackoff<<(attempt-1), maxBackoff), true
}

func parseRetryAfter(v string) time.Duration {
	if secs, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second
	}
	return 0
}

// errorMessage pulls error.message out of Anthropic's error body, falling
// back to the trimmed body.
func errorMessage(raw []byte) string {
	var body struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &body) == nil && body.Error.Message != "" {
		return body.Error.Message
	}
	return strings.TrimSpace(string(raw))
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func utcDay(t time.Time) time.Time {
	y, m, d := t.UTC().Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
