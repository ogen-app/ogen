package zernio

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
)

// InsightQuery holds the query params shared by Zernio's aggregate
// analytics endpoints (best-time, content-decay, posting-frequency). A
// zero-value field is omitted so Zernio's own defaults apply (source=all,
// all platforms/profiles/accounts). Ogen always sets ProfileID so the
// aggregate is scoped to the current tenant's profile (CON-153).
type InsightQuery struct {
	Platform  string
	ProfileID string
	AccountID string
	Source    string // all | late | external (see AnalyticsSource* constants)
}

func (q InsightQuery) values() url.Values {
	v := url.Values{}
	set := func(k, val string) {
		if val != "" {
			v.Set(k, val)
		}
	}
	set("platform", q.Platform)
	set("profileId", q.ProfileID)
	set("accountId", q.AccountID)
	set("source", q.Source)
	return v
}

// BestTimeSlot is one day-of-week × hour engagement cell. DayOfWeek is
// 0-6 (Sunday=0, UTC); Hour is 0-23 (UTC). Field names mirror Zernio's
// snake_case wire shape exactly.
type BestTimeSlot struct {
	DayOfWeek     int     `json:"day_of_week"`
	Hour          int     `json:"hour"`
	AvgEngagement float64 `json:"avg_engagement"`
	PostCount     int     `json:"post_count"`
}

// BestTimes wraps GET /analytics/best-time. Raw retains the verbatim
// body for forward-compat, as AnalyticsItem does.
type BestTimes struct {
	Slots []BestTimeSlot  `json:"slots"`
	Raw   json.RawMessage `json:"-"`
}

// ContentDecayBucket is one time window in the engagement-accrual curve.
// AvgPctOfFinal is the mean percentage of a post's total engagement
// reached by the end of that window.
type ContentDecayBucket struct {
	BucketOrder   int     `json:"bucket_order"`
	BucketLabel   string  `json:"bucket_label"`
	AvgPctOfFinal float64 `json:"avg_pct_of_final"`
	PostCount     int     `json:"post_count"`
}

// ContentDecay wraps GET /analytics/content-decay.
type ContentDecay struct {
	Buckets []ContentDecayBucket `json:"buckets"`
	Raw     json.RawMessage      `json:"-"`
}

// PostingFrequencyRow is one (platform, posts-per-week) frequency bucket
// with the average engagement observed across all weeks at that cadence.
type PostingFrequencyRow struct {
	Platform          string  `json:"platform"`
	PostsPerWeek      int     `json:"posts_per_week"`
	AvgEngagementRate float64 `json:"avg_engagement_rate"`
	AvgEngagement     float64 `json:"avg_engagement"`
	WeeksCount        int     `json:"weeks_count"`
}

// PostingFrequency wraps GET /analytics/posting-frequency.
type PostingFrequency struct {
	Frequency []PostingFrequencyRow `json:"frequency"`
	Raw       json.RawMessage       `json:"-"`
}

// IsAddonRequired reports whether err is the analytics add-on paywall: the
// legacy 402 (always), or a 403 that carries Zernio's `"requiresAddon": true`
// flag. A 403 WITHOUT that flag (a proxy/WAF/gateway block, a revoked token,
// etc.) is deliberately NOT classified here, so it stays a plain *APIError the
// caller can surface rather than being masked as "add-on required". Callers map
// a true result to ErrAnalyticsUnavailable.
func IsAddonRequired(err error) bool {
	if IsStatus(err, http.StatusPaymentRequired) {
		return true
	}
	apiErr, ok := errors.AsType[*APIError](err)
	return ok && apiErr.Status == http.StatusForbidden && apiErr.RequiresAddon
}

// getAddonGated GETs an add-on-gated analytics endpoint, translating the
// add-on paywall (402/403) to ErrAnalyticsUnavailable so every caller
// branches on the one sentinel; all other non-2xx surface as the usual
// *APIError (so IsStatus(err, 401|429) still works). The body is captured
// raw so the typed wrapper can retain it verbatim.
func (c *Client) getAddonGated(ctx context.Context, path string, q url.Values) (json.RawMessage, error) {
	if c == nil {
		return nil, errors.New("zernio: client is disabled")
	}
	var raw json.RawMessage
	if err := c.do(ctx, http.MethodGet, path, q, nil, &raw); err != nil {
		if IsAddonRequired(err) {
			return nil, ErrAnalyticsUnavailable
		}
		return nil, err
	}
	return raw, nil
}

// GetBestTimes returns the day-of-week × hour engagement grid
// (GET /analytics/best-time). An empty/absent `slots` array decodes to a
// nil slice rather than an error, matching the tolerant decode elsewhere
// in this package.
func (c *Client) GetBestTimes(ctx context.Context, q InsightQuery) (BestTimes, error) {
	raw, err := c.getAddonGated(ctx, "/analytics/best-time", q.values())
	if err != nil {
		return BestTimes{}, err
	}
	var out BestTimes
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &out); err != nil {
			return BestTimes{}, fmt.Errorf("zernio: decode best-time: %w", err)
		}
	}
	out.Raw = raw
	return out, nil
}

// GetContentDecay returns the engagement-accrual buckets
// (GET /analytics/content-decay).
func (c *Client) GetContentDecay(ctx context.Context, q InsightQuery) (ContentDecay, error) {
	raw, err := c.getAddonGated(ctx, "/analytics/content-decay", q.values())
	if err != nil {
		return ContentDecay{}, err
	}
	var out ContentDecay
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &out); err != nil {
			return ContentDecay{}, fmt.Errorf("zernio: decode content-decay: %w", err)
		}
	}
	out.Raw = raw
	return out, nil
}

// GetPostingFrequency returns the per-platform cadence↔engagement rows
// (GET /analytics/posting-frequency).
func (c *Client) GetPostingFrequency(ctx context.Context, q InsightQuery) (PostingFrequency, error) {
	raw, err := c.getAddonGated(ctx, "/analytics/posting-frequency", q.values())
	if err != nil {
		return PostingFrequency{}, err
	}
	var out PostingFrequency
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &out); err != nil {
			return PostingFrequency{}, fmt.Errorf("zernio: decode posting-frequency: %w", err)
		}
	}
	out.Raw = raw
	return out, nil
}
