package analytics

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ogen-app/ogen/src/infra/publishers/zernio"
	"github.com/ogen-app/ogen/src/infra/repository"
)

// ReasonAddonRequired is a live insight read the workspace's publisher plan
// doesn't include.
const ReasonAddonRequired = "addon_required"

// ErrUpstream is a live insight read the publisher failed.
var ErrUpstream = errors.New("analytics upstream unavailable")

// InsightFilter narrows a live insight read: a publisher platform slug, one
// account, and the source (all, late or external; empty is all).
type InsightFilter struct {
	Platform  string
	AccountID string
	Source    string
}

// BestTimes is the publisher's day-of-week by hour engagement grid, across
// the workspace's connected accounts.
func (s *Service) BestTimes(ctx context.Context, f InsightFilter) ([]zernio.BestTimeSlot, string, error) {
	return insight(ctx, s, "best-times", f, func(ctx context.Context, q zernio.InsightQuery) ([]zernio.BestTimeSlot, error) {
		out, err := s.Zernio.GetBestTimes(ctx, q)
		return out.Slots, err
	})
}

// ContentDecay is the share of a post's final engagement it reaches by each
// age, from the publisher.
func (s *Service) ContentDecay(ctx context.Context, f InsightFilter) ([]zernio.ContentDecayBucket, string, error) {
	return insight(ctx, s, "content-decay", f, func(ctx context.Context, q zernio.InsightQuery) ([]zernio.ContentDecayBucket, error) {
		out, err := s.Zernio.GetContentDecay(ctx, q)
		return out.Buckets, err
	})
}

// PostingFrequency is the engagement rate at each posts-per-week cadence,
// per platform, from the publisher.
func (s *Service) PostingFrequency(ctx context.Context, f InsightFilter) ([]zernio.PostingFrequencyRow, string, error) {
	return insight(ctx, s, "posting-frequency", f, func(ctx context.Context, q zernio.InsightQuery) ([]zernio.PostingFrequencyRow, error) {
		out, err := s.Zernio.GetPostingFrequency(ctx, q)
		return out.Frequency, err
	})
}

// insight runs one live read for the workspace's publishing profile, through
// the cache when there is one. A workspace without the publisher, or without
// a profile, reads ReasonNotConfigured; one without the add-on,
// ReasonAddonRequired. Any other publisher failure is ErrUpstream.
func insight[T any](ctx context.Context, s *Service, endpoint string, f InsightFilter, fetch func(context.Context, zernio.InsightQuery) (T, error)) (T, string, error) {
	var zero T
	if s.Zernio == nil || s.ProfileID == nil {
		return zero, ReasonNotConfigured, nil
	}
	profile, err := s.ProfileID(ctx)
	if err != nil {
		return zero, "", err
	}
	if profile == "" {
		return zero, ReasonNotConfigured, nil
	}
	q := zernio.InsightQuery{ProfileID: profile, Platform: f.Platform, AccountID: f.AccountID, Source: f.Source}
	load := func(ctx context.Context) (any, error) { return fetch(ctx, q) }
	var v any
	if s.InsightCache != nil {
		key := strings.Join([]string{endpoint, q.ProfileID, q.Platform, q.AccountID, q.Source}, "\x00")
		v, err = s.InsightCache(ctx, key, load)
	} else {
		v, err = load(ctx)
	}
	if errors.Is(err, zernio.ErrAnalyticsUnavailable) {
		return zero, ReasonAddonRequired, nil
	}
	if err != nil {
		return zero, "", fmt.Errorf("%w: %w", ErrUpstream, err)
	}
	out, _ := v.(T)
	return out, "", nil
}

// Follower granularities.
const (
	GranularityDaily   = "daily"
	GranularityWeekly  = "weekly"
	GranularityMonthly = "monthly"
)

// ValidGranularity reports whether g is a follower granularity.
func ValidGranularity(g string) bool {
	return g == GranularityDaily || g == GranularityWeekly || g == GranularityMonthly
}

// FollowerHistory is each account's follower count over time, and its
// growth.
type FollowerHistory struct {
	Accounts    []repository.FollowerAccountSummary         `json:"accounts"`
	Series      map[string][]repository.FollowerSeriesPoint `json:"series"`
	Granularity string                                      `json:"granularity"`
	// BiggestDay is the window's day of largest follower gain, nil when
	// none gained.
	BiggestDay *FollowerDay `json:"biggest_day,omitempty"`
}

// FollowerHistoryOptions narrows the history: one account, a date range
// (zero ends are open), and the granularity (empty is daily).
type FollowerHistoryOptions struct {
	AccountID   string
	From, To    time.Time
	Granularity string
}

// FollowerHistory reads the follower history from the daily snapshots, the last
// point of each week or month kept when coarser than daily; nil with
// ReasonNotConfigured when the analytics database isn't.
func (s *Service) FollowerHistory(ctx context.Context, opt FollowerHistoryOptions) (*FollowerHistory, string, error) {
	if s.Followers == nil {
		return nil, ReasonNotConfigured, nil
	}
	if opt.Granularity == "" {
		opt.Granularity = GranularityDaily
	}
	summary, err := s.Followers.Summary(ctx, opt.AccountID)
	if err != nil {
		return nil, "", err
	}
	points, err := s.Followers.Series(ctx, repository.FollowerSeriesOptions{AccountID: opt.AccountID, From: opt.From, To: opt.To})
	if err != nil {
		return nil, "", err
	}
	series := map[string][]repository.FollowerSeriesPoint{}
	for _, p := range DownsampleSeries(points, opt.Granularity) {
		series[p.SocialAccountID] = append(series[p.SocialAccountID], p)
	}
	if summary == nil {
		summary = []repository.FollowerAccountSummary{}
	}
	day := biggestDay(points)
	if err := s.postOfDay(ctx, day); err != nil {
		return nil, "", err
	}
	return &FollowerHistory{Accounts: summary, Series: series, Granularity: opt.Granularity, BiggestDay: day}, "", nil
}

// DownsampleSeries reduces daily points to one point per ISO week or
// calendar month per account, keeping the last point in each. The input is
// ordered by account and date, so a later point in a bucket wins and the
// buckets stay in date order; daily returns the input unchanged.
func DownsampleSeries(points []repository.FollowerSeriesPoint, granularity string) []repository.FollowerSeriesPoint {
	if granularity != GranularityWeekly && granularity != GranularityMonthly {
		return points
	}
	out := make([]repository.FollowerSeriesPoint, 0, len(points))
	idxByBucket := map[string]int{}
	for _, p := range points {
		key := p.SocialAccountID + "|" + periodBucket(p.PointDate, granularity)
		if i, ok := idxByBucket[key]; ok {
			out[i] = p
			continue
		}
		idxByBucket[key] = len(out)
		out = append(out, p)
	}
	return out
}

func periodBucket(d time.Time, granularity string) string {
	if granularity == GranularityWeekly {
		y, w := d.ISOWeek()
		return fmt.Sprintf("%04d-W%02d", y, w)
	}
	return fmt.Sprintf("%04d-%02d", d.Year(), int(d.Month()))
}

// PostsPage is one page of the published posts' analytics, with totals over
// all of them.
type PostsPage struct {
	Items    []repository.PostAnalyticsListItem `json:"items"`
	Page     int                                `json:"page"`
	Limit    int                                `json:"limit"`
	Total    int                                `json:"total"`
	Pages    int                                `json:"pages"`
	Overview repository.PostAnalyticsOverview   `json:"overview"`
	// SortBy and Order are what the page is sorted on.
	SortBy string `json:"sort_by"`
	Order  string `json:"order"`
}

// ListPosts pages the analytics of the posts published through the
// publisher; nil with ReasonNotConfigured when the analytics database isn't.
// opt.Platform is a platform's display name.
func (s *Service) ListPosts(ctx context.Context, opt repository.PostAnalyticsListOptions) (*PostsPage, string, error) {
	if s.Repo == nil {
		return nil, ReasonNotConfigured, nil
	}
	items, overview, err := s.Repo.List(ctx, opt)
	if err != nil {
		return nil, "", err
	}
	page := &PostsPage{Items: items, Page: opt.Page, Limit: opt.Limit, Total: overview.PostCount, Overview: overview,
		SortBy: opt.SortBy, Order: opt.Order}
	if page.Total > 0 && opt.Limit > 0 {
		page.Pages = (page.Total + opt.Limit - 1) / opt.Limit
	}
	return page, "", nil
}
