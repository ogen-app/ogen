package zernio

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// FollowerQuery holds the documented GET /accounts/follower-stats params.
// A zero-value field is omitted so Zernio's defaults apply (all accounts,
// last 30 days, daily granularity). Ogen sets ProfileID to scope to the
// current tenant's profile.
type FollowerQuery struct {
	AccountIDs  []string // joined into accountIds (csv); empty = all accounts
	ProfileID   string
	FromDate    string // YYYY-MM-DD
	ToDate      string // YYYY-MM-DD
	Granularity string // daily | weekly | monthly
}

func (q FollowerQuery) values() url.Values {
	v := url.Values{}
	if len(q.AccountIDs) > 0 {
		v.Set("accountIds", strings.Join(q.AccountIDs, ","))
	}
	set := func(k, val string) {
		if val != "" {
			v.Set(k, val)
		}
	}
	set("profileId", q.ProfileID)
	set("fromDate", q.FromDate)
	set("toDate", q.ToDate)
	set("granularity", q.Granularity)
	return v
}

// FollowerAccount is one account's summary block. ID (`_id`) maps to a
// local social_accounts.id. Growth may be negative when followers drop.
type FollowerAccount struct {
	ID               string  `json:"_id"`
	Platform         string  `json:"platform"`
	Username         string  `json:"username"`
	CurrentFollowers int64   `json:"currentFollowers"`
	Growth           int64   `json:"growth"`
	GrowthPercentage float64 `json:"growthPercentage"`
	DataPoints       int     `json:"dataPoints"`
}

// FollowerPoint is one follower-count datum in the daily series.
type FollowerPoint struct {
	Date      string `json:"date"` // YYYY-MM-DD
	Followers int64  `json:"followers"`
}

// FollowerDateRange echoes the resolved query window.
type FollowerDateRange struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// FollowerStats wraps GET /accounts/follower-stats. Stats is keyed by
// account id (matching FollowerAccount.ID). Raw retains the verbatim body.
type FollowerStats struct {
	Accounts    []FollowerAccount          `json:"accounts"`
	Stats       map[string][]FollowerPoint `json:"stats"`
	DateRange   FollowerDateRange          `json:"dateRange"`
	Granularity string                     `json:"granularity"`
	Raw         json.RawMessage            `json:"-"`
}

// GetFollowerStats returns follower-count history and growth per account
// (GET /accounts/follower-stats). Add-on-gated: a 402/403 becomes
// ErrAnalyticsUnavailable.
func (c *Client) GetFollowerStats(ctx context.Context, q FollowerQuery) (FollowerStats, error) {
	raw, err := c.getAddonGated(ctx, "/accounts/follower-stats", q.values())
	if err != nil {
		return FollowerStats{}, err
	}
	var out FollowerStats
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &out); err != nil {
			return FollowerStats{}, fmt.Errorf("zernio: decode follower-stats: %w", err)
		}
	}
	out.Raw = raw
	return out, nil
}

// SyncExternalRequest is the POST /posts/sync-external body. AccountID is
// required; supply URL or PostID to locate a specific post (omit both to
// just refresh the account's recent posts).
type SyncExternalRequest struct {
	AccountID string `json:"accountId"`
	URL       string `json:"url,omitempty"`
	PostID    string `json:"postId,omitempty"`
}

// ExternalPostAnalytics is the engagement block on a synced external
// post. This endpoint confirms a post exists and returns basic engagement
// (likes/comments), not the delayed platform insights (reach/impressions
// may lag ~24h); those fields are present but may be zero.
type ExternalPostAnalytics struct {
	Likes          int       `json:"likes"`
	Comments       int       `json:"comments"`
	Shares         int       `json:"shares"`
	Saves          int       `json:"saves"`
	Sends          int       `json:"sends"`
	Clicks         int       `json:"clicks"`
	Views          int       `json:"views"`
	Reach          int       `json:"reach"`
	Impressions    int       `json:"impressions"`
	EngagementRate float64   `json:"engagementRate"`
	LastUpdated    *flexTime `json:"lastUpdated,omitempty"`
}

// LastUpdatedTime returns the analytics lastUpdated as a plain *time.Time
// (nil when unset/unparseable), reusing the tolerant flexTime decode.
func (a ExternalPostAnalytics) LastUpdatedTime() *time.Time { return a.LastUpdated.ptr() }

// ExternalPost is one post returned by sync-external. PlatformPostID is
// the platform's own media/video id — the value written to a local
// posts.publisher_post_id on confirmation.
type ExternalPost struct {
	Platform        string                `json:"platform"`
	PlatformPostID  string                `json:"platformPostId"`
	PlatformPostURL string                `json:"platformPostUrl"`
	Content         string                `json:"content"`
	PublishedAt     *flexTime             `json:"publishedAt,omitempty"`
	MediaType       string                `json:"mediaType"`
	MediaURL        string                `json:"mediaUrl"`
	ThumbnailURL    string                `json:"thumbnailUrl"`
	Analytics       ExternalPostAnalytics `json:"analytics"`
}

// PublishedAtTime returns publishedAt as a plain *time.Time (nil when
// unset/unparseable).
func (p ExternalPost) PublishedAtTime() *time.Time { return p.PublishedAt.ptr() }

// SyncExternalSummary counts what the on-demand sync did upstream.
// Skipped is true when the account was just synced (debounced ~15s) and
// the live platform fetch was skipped.
type SyncExternalSummary struct {
	PostsFound  int  `json:"postsFound"`
	PostsSynced int  `json:"postsSynced"`
	Skipped     bool `json:"skipped"`
}

// SyncExternalResult mirrors POST /posts/sync-external. Found reports
// whether the located post (by url/postId) was matched; Post is that
// post. Posts carries the account's recent posts when no locator was
// given. Raw retains the verbatim body.
type SyncExternalResult struct {
	Synced SyncExternalSummary `json:"synced"`
	Found  bool                `json:"found"`
	Post   *ExternalPost       `json:"post,omitempty"`
	Posts  []ExternalPost      `json:"posts,omitempty"`
	Raw    json.RawMessage     `json:"-"`
}

// SyncExternalPost fetches an account's latest external posts on demand
// and, when a url/postId is supplied, returns the matched post. Not
// add-on gated (this endpoint returns 400/404, never the paywall). A
// missing post surfaces as *APIError{Status:404}; callers may prefer to
// treat that as "not found" rather than an error.
func (c *Client) SyncExternalPost(ctx context.Context, req SyncExternalRequest) (SyncExternalResult, error) {
	if c == nil {
		return SyncExternalResult{}, errors.New("zernio: client is disabled")
	}
	if req.AccountID == "" {
		return SyncExternalResult{}, errors.New("zernio: sync-external requires accountId")
	}
	var raw json.RawMessage
	if err := c.do(ctx, http.MethodPost, "/posts/sync-external", nil, req, &raw); err != nil {
		return SyncExternalResult{}, err
	}
	var out SyncExternalResult
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &out); err != nil {
			return SyncExternalResult{}, fmt.Errorf("zernio: decode sync-external: %w", err)
		}
	}
	out.Raw = raw
	return out, nil
}
