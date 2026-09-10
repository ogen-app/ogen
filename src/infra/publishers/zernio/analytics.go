package zernio

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// ErrAnalyticsUnavailable maps Zernio's analytics paywall to a typed
// sentinel: the legacy 402 ("analytics add-on required") on GET
// /analytics, and the current 403 (`{"requiresAddon":true}`) returned by
// the aggregate analytics endpoints (best-time / content-decay /
// posting-frequency / follower-stats, CON-153). Under Zernio's bundled
// pricing on GET /analytics — analytics is on every tier, re-verified
// 2026-06-15 — the 402 is never expected; the refresh queue handles it
// defensively only (CON-93 §2/§9). The 403 IS expected on the CON-153
// endpoints for tenants without the add-on and is surfaced gracefully.
// See IsAddonRequired for the shared status check.
var ErrAnalyticsUnavailable = errors.New("zernio: analytics unavailable (add-on required)")

// Analytics source filters (CON-93). Ogen only ever requests `late` —
// posts Zernio itself published, which is the entire CON-93 scope. The
// `external`/`all` values are listed for completeness; analytics for
// externally-synced/legacy posts is a separate, future issue.
const (
	AnalyticsSourceLate     = "late"
	AnalyticsSourceExternal = "external"
	AnalyticsSourceAll      = "all"
)

// AnalyticsQuery holds the documented GET /analytics query params. A
// zero-value field is omitted from the request so Zernio's own defaults
// apply (source=all, last 90d, limit=50, page=1, sortBy=date,
// order=desc). The refresh queue always sets Source=late.
type AnalyticsQuery struct {
	Source    string // late | external | all
	Platform  string
	ProfileID string
	AccountID string
	PostID    string // single-post filter (?postId=)
	FromDate  string // YYYY-MM-DD
	ToDate    string // YYYY-MM-DD
	SortBy    string // date|engagement|impressions|reach|likes|comments|shares|saves|clicks|views
	Order     string // asc|desc
	Limit     int    // 1–100
	Page      int    // 1-based
}

func (q AnalyticsQuery) values() url.Values {
	v := url.Values{}
	set := func(k, val string) {
		if val != "" {
			v.Set(k, val)
		}
	}
	set("source", q.Source)
	set("platform", q.Platform)
	set("profileId", q.ProfileID)
	set("accountId", q.AccountID)
	set("postId", q.PostID)
	set("fromDate", q.FromDate)
	set("toDate", q.ToDate)
	set("sortBy", q.SortBy)
	set("order", q.Order)
	if q.Limit > 0 {
		v.Set("limit", strconv.Itoa(q.Limit))
	}
	if q.Page > 0 {
		v.Set("page", strconv.Itoa(q.Page))
	}
	return v
}

// flexTime wraps time.Time to tolerate every timestamp shape Zernio's
// /analytics endpoint has shipped. Go's stdlib time.Time.UnmarshalJSON
// accepts only RFC3339, but Zernio has been observed returning a
// space-separated, zone-less timestamp ("2026-07-29 12:47:25") on the
// analytics `lastUpdated` field. Decoding that into a plain *time.Time
// fails the whole item — and because one bad item fails the whole page
// (decodeAnalyticsItems is all-or-nothing), a single such timestamp
// zeroes out that tenant's entire analytics collection. Tolerating the
// format keeps collection resilient, matching the tolerant envelope and
// pagination decode already in this file.
type flexTime struct{ time.Time }

// flexTimeLayouts are tried in order. RFC3339 first (the documented and
// common shape); the space-separated variants cover the observed
// deviation and its plausible zoned/fractional cousins.
var flexTimeLayouts = []string{
	time.RFC3339Nano,
	time.RFC3339,
	"2006-01-02T15:04:05",
	"2006-01-02 15:04:05.999999999Z07:00",
	"2006-01-02 15:04:05Z07:00",
	"2006-01-02 15:04:05",
	"2006-01-02",
}

// UnmarshalJSON parses the JSON string against each known layout. An
// unparseable (or non-string) value leaves the zero time rather than
// erroring: the raw item is preserved verbatim on the snapshot
// (RawJSON), the numeric metrics are what matter, and failing here would
// re-introduce the very whole-page wipeout this type exists to prevent.
func (t *flexTime) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return nil // null / number / object — treat as absent
	}
	if s == "" {
		return nil
	}
	for _, layout := range flexTimeLayouts {
		if parsed, err := time.Parse(layout, s); err == nil {
			t.Time = parsed
			return nil
		}
	}
	return nil
}

// ptr returns a *time.Time for assigning onto the analytics model, nil
// when unset/unparseable so the column stays NULL rather than epoch-zero.
func (t *flexTime) ptr() *time.Time {
	if t == nil || t.Time.IsZero() {
		return nil
	}
	tt := t.Time
	return &tt
}

// LastUpdatedTime returns the metrics' lastUpdated as a plain *time.Time
// (nil when unset or unparseable), so callers outside this package assign
// onto the analytics model without touching the tolerant wire type.
func (m Metrics) LastUpdatedTime() *time.Time { return m.LastUpdated.ptr() }

// Metrics is Zernio's aggregate engagement block (`analytics{}`), shared
// by the post-level rollup and each per-platform breakdown. Field names
// mirror Zernio's exactly. IGReels* are Instagram-only and zero
// elsewhere. LastUpdated is when Zernio last refreshed these numbers.
type Metrics struct {
	Impressions               int       `json:"impressions"`
	Reach                     int       `json:"reach"`
	Likes                     int       `json:"likes"`
	Comments                  int       `json:"comments"`
	Shares                    int       `json:"shares"`
	Saves                     int       `json:"saves"`
	Clicks                    int       `json:"clicks"`
	Views                     int       `json:"views"`
	Follows                   int       `json:"follows"`
	IGReelsAvgWatchTime       float64   `json:"igReelsAvgWatchTime,omitzero"`
	IGReelsVideoViewTotalTime float64   `json:"igReelsVideoViewTotalTime,omitzero"`
	EngagementRate            float64   `json:"engagementRate"`
	LastUpdated               *flexTime `json:"lastUpdated,omitempty"`
}

// PlatformAnalytics is one per-platform row inside an AnalyticsItem.
// SyncStatus / ErrorMessage / ReauthorizeURL carry the per-platform
// scope-gap nuance (412/reauthorizeUrl, CON-93 §9/§10): a platform
// connected before analytics scopes were granted reports an error here
// without failing the whole item; platforms that do report still carry
// metrics.
type PlatformAnalytics struct {
	Platform        string  `json:"platform"`
	Status          string  `json:"status,omitempty"`
	PlatformPostID  string  `json:"platformPostId,omitempty"`
	AccountID       string  `json:"accountId,omitempty"`
	AccountUsername string  `json:"accountUsername,omitempty"`
	PlatformPostURL string  `json:"platformPostUrl,omitempty"`
	SyncStatus      string  `json:"syncStatus,omitempty"`
	ErrorMessage    string  `json:"errorMessage,omitempty"`
	ReauthorizeURL  string  `json:"reauthorizeUrl,omitempty"`
	Analytics       Metrics `json:"analytics"`
}

// AnalyticsItem is one post's analytics as returned by GET /analytics.
// PostID / LatePostID are the join keys back to a local
// posts.publisher_post_id. Raw retains the verbatim item for
// forward-compat, exactly as Account/Profile do.
type AnalyticsItem struct {
	PostID            string              `json:"postId"`
	LatePostID        string              `json:"latePostId,omitempty"`
	Status            string              `json:"status,omitempty"`
	Content           string              `json:"content,omitempty"`
	ScheduledFor      *flexTime           `json:"scheduledFor,omitempty"`
	PublishedAt       *flexTime           `json:"publishedAt,omitempty"`
	Platform          string              `json:"platform,omitempty"`
	PlatformPostURL   string              `json:"platformPostUrl,omitempty"`
	IsExternal        bool                `json:"isExternal"`
	SyncStatus        string              `json:"syncStatus,omitempty"`
	Message           string              `json:"message,omitempty"`
	ThumbnailURL      string              `json:"thumbnailUrl,omitempty"`
	MediaType         string              `json:"mediaType,omitempty"`
	Analytics         Metrics             `json:"analytics"`
	PlatformAnalytics []PlatformAnalytics `json:"platformAnalytics,omitempty"`

	// Raw is the verbatim Zernio item, preserved so a future field can
	// be read without a client change. Not part of the JSON contract.
	Raw json.RawMessage `json:"-"`
}

// AnalyticsPagination mirrors Zernio's pagination block. Both field spellings
// Zernio has shipped for the last-page count are tolerated (`pages` and
// `totalPages`), plus the cursor-style `hasMore` flag, so the refresh loop can
// page correctly regardless of which the endpoint returns. See LastPage.
type AnalyticsPagination struct {
	Page       int  `json:"page"`
	Limit      int  `json:"limit"`
	Total      int  `json:"total"`
	Pages      int  `json:"pages"`
	TotalPages int  `json:"totalPages"`
	HasMore    bool `json:"hasMore"`
}

// LastPage returns the highest known last-page number across the two field
// names Zernio has used (`pages`, `totalPages`), or 0 when neither is present
// (e.g. a bare-array or cursor-style response) — callers then fall back to the
// returned item count / HasMore to decide whether to keep paging.
func (p AnalyticsPagination) LastPage() int {
	if p.Pages > p.TotalPages {
		return p.Pages
	}
	return p.TotalPages
}

// analyticsListKeys are the wrapper keys under which Zernio has returned (or
// might return) the analytics array. The endpoint is documented as returning
// post objects and has shipped both as a bare top-level array and wrapped in a
// single-key envelope; accepting all of these means an upstream shape change
// can't silently zero out collection (CON-93 follow-up). `analytics` first
// because it is the historically-assumed key.
var analyticsListKeys = []string{"analytics", "posts", "data", "results", "items"}

// ListAnalytics fetches one page of post analytics from GET /analytics,
// mirroring Status. The refresh queue pages through with Source=late.
//
// The add-on paywall (402/403) is translated to ErrAnalyticsUnavailable;
// all other non-2xx responses surface as the usual *APIError so the worker
// can reuse IsStatus(err, 401|429). The body is decoded tolerantly — see
// decodeAnalyticsList — so either the bare-array or enveloped shape works.
func (c *Client) ListAnalytics(ctx context.Context, q AnalyticsQuery) ([]AnalyticsItem, AnalyticsPagination, error) {
	if c == nil {
		return nil, AnalyticsPagination{}, errors.New("zernio: client is disabled")
	}
	var raw json.RawMessage
	if err := c.do(ctx, http.MethodGet, "/analytics", q.values(), nil, &raw); err != nil {
		if IsAddonRequired(err) {
			return nil, AnalyticsPagination{}, ErrAnalyticsUnavailable
		}
		return nil, AnalyticsPagination{}, err
	}
	return decodeAnalyticsList(raw)
}

// decodeAnalyticsList tolerates every top-level shape GET /analytics has been
// observed or documented to return:
//   - a bare array of post objects:            [ {item}, {item} ]
//   - an object envelope wrapping the array:   { "<key>": [ {item} ], "pagination": {...} }
//   - a single post object (the ?postId shape): { "postId": ..., "analytics": ... }
//
// Pagination is returned as its zero value when absent (bare array / single
// object). An object with none of the known array keys and no post id decodes
// to an empty list rather than an error, so a genuinely empty page is a no-op.
func decodeAnalyticsList(raw json.RawMessage) ([]AnalyticsItem, AnalyticsPagination, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return nil, AnalyticsPagination{}, nil
	}
	switch trimmed[0] {
	case '[':
		var rawItems []json.RawMessage
		if err := json.Unmarshal(trimmed, &rawItems); err != nil {
			return nil, AnalyticsPagination{}, fmt.Errorf("zernio: decode analytics array: %w", err)
		}
		items, err := decodeAnalyticsItems(rawItems)
		return items, AnalyticsPagination{}, err
	case '{':
		var env map[string]json.RawMessage
		if err := json.Unmarshal(trimmed, &env); err != nil {
			return nil, AnalyticsPagination{}, fmt.Errorf("zernio: decode analytics envelope: %w", err)
		}
		var pag AnalyticsPagination
		if p, ok := env["pagination"]; ok && len(p) > 0 {
			// Pagination is advisory; a decode failure must not sink the page.
			_ = json.Unmarshal(p, &pag)
		}
		for _, key := range analyticsListKeys {
			arr, ok := env[key]
			if !ok || len(arr) == 0 || string(arr) == "null" {
				continue
			}
			if bytes.TrimSpace(arr)[0] != '[' {
				continue // e.g. the metrics object at the "analytics" key of a single post
			}
			var rawItems []json.RawMessage
			if err := json.Unmarshal(arr, &rawItems); err != nil {
				return nil, pag, fmt.Errorf("zernio: decode analytics %q array: %w", key, err)
			}
			items, err := decodeAnalyticsItems(rawItems)
			return items, pag, err
		}
		// No wrapper array — but the object may itself be a single post (the
		// documented ?postId shape). Decode it as a one-item list when it
		// carries a post id; otherwise it's an empty page.
		if _, ok := env["postId"]; ok {
			item, err := decodeAnalyticsItem(trimmed)
			if err != nil {
				return nil, pag, err
			}
			return []AnalyticsItem{*item}, pag, nil
		}
		if _, ok := env["latePostId"]; ok {
			item, err := decodeAnalyticsItem(trimmed)
			if err != nil {
				return nil, pag, err
			}
			return []AnalyticsItem{*item}, pag, nil
		}
		return nil, pag, nil
	default:
		return nil, AnalyticsPagination{}, fmt.Errorf("zernio: analytics response is neither array nor object (starts with %q)", trimmed[0])
	}
}

func decodeAnalyticsItems(rawItems []json.RawMessage) ([]AnalyticsItem, error) {
	out := make([]AnalyticsItem, 0, len(rawItems))
	for _, raw := range rawItems {
		item, err := decodeAnalyticsItem(raw)
		if err != nil {
			return nil, err
		}
		out = append(out, *item)
	}
	return out, nil
}

func decodeAnalyticsItem(raw json.RawMessage) (*AnalyticsItem, error) {
	var item AnalyticsItem
	if err := json.Unmarshal(raw, &item); err != nil {
		return nil, err
	}
	item.Raw = append(json.RawMessage(nil), raw...)
	return &item, nil
}
