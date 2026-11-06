// Package analytics reads what happened to a workspace's posts: the overview
// over a window and the best and worst performers, narrowed to platforms
// and/or a campaign, the lessons of its whole history, and one post's
// statistics. The HTTP handlers and the assistant read through it, so
// a number on the dashboard and one in the chat are the same number.
package analytics

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/ogen-app/ogen/src/analytics/insights"
	"github.com/ogen-app/ogen/src/analytics/overview"
	"github.com/ogen-app/ogen/src/analytics/performers"
	"github.com/ogen-app/ogen/src/analytics/timeframe"
	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
)

// Reasons a read has no data, as the API reports them.
const (
	ReasonNotConfigured = "not_configured"
	ReasonNoData        = "no_data"
)

var (
	// ErrInvalidPlatform is a platform slug that matches no platform.
	ErrInvalidPlatform = errors.New("invalid_platform")
	// ErrCampaignNotFound is a campaign that is unknown or another tenant's.
	ErrCampaignNotFound = errors.New("campaign not found")
)

// Service reads analytics. Repo nil means the analytics database is not
// configured for this deployment; every read then reports
// ReasonNotConfigured.
type Service struct {
	Repo      repository.PostAnalyticsRepository
	Followers repository.FollowerStatsRepository
	Posts     repository.PostRepository
	Platforms repository.PlatformRepository
	Accounts  repository.SocialAccountRepository
	Campaigns repository.CampaignRepository
	// ReachSamples returns the workspace's reach-at-age baseline; nil reads
	// Repo each time. The HTTP handler passes its cached read.
	ReachSamples func(ctx context.Context) ([]repository.ReachAgeSample, error)
	// LifespanSamples returns the workspace's lifespan samples since a day
	// (zero for all time); nil reads Repo each time.
	LifespanSamples func(ctx context.Context, since time.Time) ([]repository.LifespanSample, error)
}

// Scope is what a read is narrowed to: platforms and/or one campaign. A nil
// scope is the whole workspace, a nil platform set every platform, an empty
// campaign every campaign. Analytics rows carry only the platform's display
// name, which operators can rename, and no campaign, so rows are matched
// through their post's platform and campaign, and follower snapshots through
// the Zernio slug.
type Scope struct {
	ids        map[string]bool
	slugs      map[string]bool
	campaignID string
}

// Scope resolves platform slugs (Zernio ids) and a campaign. A slug matching
// no platform is ErrInvalidPlatform, so a typo fails loudly instead of
// widening to every platform; an unknown campaign is ErrCampaignNotFound.
func (s *Service) Scope(ctx context.Context, slugs []string, campaignID string) (*Scope, error) {
	var sc *Scope
	if len(slugs) > 0 {
		bySlug, err := s.platformsBySlug(ctx)
		if err != nil {
			return nil, err
		}
		sc = &Scope{ids: map[string]bool{}, slugs: map[string]bool{}}
		for _, slug := range slugs {
			slug = strings.ToLower(strings.TrimSpace(slug))
			p, ok := bySlug[slug]
			if !ok {
				return nil, ErrInvalidPlatform
			}
			sc.ids[p.ID] = true
			sc.slugs[slug] = true
		}
	}
	if campaignID = strings.TrimSpace(campaignID); campaignID == "" {
		return sc, nil
	}
	if s.Campaigns == nil {
		return nil, ErrCampaignNotFound
	}
	_, err := s.Campaigns.GetByID(ctx, campaignID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrCampaignNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("load campaign: %w", err)
	}
	if sc == nil {
		sc = &Scope{}
	}
	sc.campaignID = campaignID
	return sc, nil
}

// platformsBySlug indexes every platform, disabled ones included (their
// history is still real), by its lower-cased Zernio slug.
func (s *Service) platformsBySlug(ctx context.Context) (map[string]models.Platform, error) {
	out := map[string]models.Platform{}
	if s.Platforms == nil {
		return out, nil
	}
	plats, err := s.Platforms.List(ctx)
	if err != nil {
		return nil, err
	}
	for _, p := range plats {
		if p.ZernioID != "" {
			out[strings.ToLower(p.ZernioID)] = p
		}
	}
	return out, nil
}

// PlatformIDs are the scope's platforms; nil for every platform.
func (sc *Scope) PlatformIDs() []string {
	if sc == nil {
		return nil
	}
	out := make([]string, 0, len(sc.ids))
	for id := range sc.ids {
		out = append(out, id)
	}
	return out
}

// Campaign is the scope's campaign; empty for every campaign.
func (sc *Scope) Campaign() string {
	if sc == nil {
		return ""
	}
	return sc.campaignID
}

// NarrowsPlatforms reports whether the scope excludes any platform.
func (sc *Scope) NarrowsPlatforms() bool { return sc != nil && sc.ids != nil }

// AllowsPlatformID reports whether a post's platform is in scope.
func (sc *Scope) AllowsPlatformID(id string) bool { return !sc.NarrowsPlatforms() || sc.ids[id] }

// AllowsSlug reports whether a Zernio platform slug is in scope.
func (sc *Scope) AllowsSlug(slug string) bool {
	return !sc.NarrowsPlatforms() || sc.slugs[strings.ToLower(slug)]
}

// AllowsCampaign reports whether a post's campaign is in scope.
func (sc *Scope) AllowsCampaign(id string) bool { return sc.Campaign() == "" || sc.campaignID == id }

// FilterRows keeps the analytics rows whose post is in scope.
func (s *Service) FilterRows(ctx context.Context, sc *Scope, sets ...*[]models.PostAnalytics) error {
	if sc == nil {
		return nil
	}
	var ids []string
	for _, set := range sets {
		for i := range *set {
			ids = append(ids, (*set)[i].PostID)
		}
	}
	keys := map[string]repository.PostScopeKey{}
	if s.Posts != nil {
		var err error
		if keys, err = s.Posts.ScopeKeysByID(ctx, ids); err != nil {
			return err
		}
	}
	for _, set := range sets {
		kept := (*set)[:0:0]
		for _, r := range *set {
			if k, ok := keys[r.PostID]; ok && sc.AllowsPlatformID(k.PlatformID) && sc.AllowsCampaign(k.CampaignID) {
				kept = append(kept, r)
			}
		}
		*set = kept
	}
	return nil
}

// Overview is the KPI cards, the per-metric series and the insights for a
// window. A nil response comes with the reason there is none.
func (s *Service) Overview(ctx context.Context, rng timeframe.Range, sc *Scope) (*overview.Response, string, error) {
	if s.Repo == nil {
		return nil, ReasonNotConfigured, nil
	}
	prev := rng.Previous()
	// The five reads (analytics for both windows, published times for both,
	// followers) are independent, so they run concurrently.
	var (
		curPosts, prevPosts []models.PostAnalytics
		curPub, prevPub     []time.Time
		folTotals           []overview.FollowerDayTotal
		folNow              int
	)
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() (err error) { curPosts, err = s.Repo.PublishedBetween(gctx, rng.From, rng.To); return err })
	g.Go(func() (err error) { prevPosts, err = s.Repo.PublishedBetween(gctx, prev.From, prev.To); return err })
	if s.Posts != nil {
		g.Go(func() (err error) {
			curPub, err = s.Posts.PublishedAtsBetween(gctx, rng.From, rng.To, sc.PlatformIDs(), sc.Campaign())
			return err
		})
		g.Go(func() (err error) {
			prevPub, err = s.Posts.PublishedAtsBetween(gctx, prev.From, prev.To, sc.PlatformIDs(), sc.Campaign())
			return err
		})
	}
	g.Go(func() (err error) {
		folTotals, folNow, err = s.followers(gctx, sc, prev.From, rng.To)
		return err
	})
	if err := g.Wait(); err != nil {
		return nil, "", err
	}
	if err := s.FilterRows(ctx, sc, &curPosts, &prevPosts); err != nil {
		return nil, "", err
	}
	// A truly empty tenant (no analytics, no posts, no followers).
	if len(curPosts) == 0 && len(prevPosts) == 0 && len(curPub) == 0 && len(prevPub) == 0 && folNow == 0 && len(folTotals) == 0 {
		return nil, ReasonNoData, nil
	}
	resp := overview.Build(overview.Inputs{
		Rng:            rng,
		Prev:           prev,
		CurPosts:       toPostPoints(curPosts),
		PrevPosts:      toPostPoints(prevPosts),
		CurPublished:   curPub,
		PrevPublished:  prevPub,
		FollowerTotals: folTotals,
		FollowersNow:   folNow,
		UpdatedAt:      maxLastChecked(curPosts, prevPosts),
	})
	if sc.Campaign() != "" {
		resp.FollowersScope = "workspace"
	}
	return &resp, "", nil
}

// followers returns the daily follower totals over [from, to] and the current
// level, summed over the in-scope accounts. Only the platform part of the
// scope applies: followers have no campaign.
func (s *Service) followers(ctx context.Context, sc *Scope, from, to time.Time) ([]overview.FollowerDayTotal, int, error) {
	if s.Followers == nil {
		return nil, 0, nil
	}
	summary, err := s.Followers.Summary(ctx, "")
	if err != nil {
		return nil, 0, err
	}
	accounts := map[string]bool{}
	var now int
	for _, a := range summary {
		if sc.AllowsSlug(a.Platform) {
			accounts[a.SocialAccountID] = true
			now += int(a.CurrentFollowers)
		}
	}
	// One series spanning both windows lets Build derive current and previous
	// follower levels without a second fetch.
	pts, err := s.Followers.Series(ctx, repository.FollowerSeriesOptions{From: from, To: to})
	if err != nil {
		return nil, 0, err
	}
	if sc.NarrowsPlatforms() {
		pts = slices.DeleteFunc(pts, func(p repository.FollowerSeriesPoint) bool { return !accounts[p.SocialAccountID] })
	}
	return followerDailyTotals(pts), now, nil
}

// Window is a read's resolved window.
type Window struct {
	From string `json:"from"`
	To   string `json:"to"`
	Days int    `json:"days"`
}

// PerformersResult is the best and worst posts of a window.
type PerformersResult struct {
	Window     Window             `json:"window"`
	UpdatedAt  time.Time          `json:"updated_at"`
	By         string             `json:"by"`
	TotalPosts int                `json:"total_posts"`
	Best       []performers.Row   `json:"best"`
	Worst      []performers.Row   `json:"worst"`
	Insights   []insights.Insight `json:"insights"`
}

// Performers ranks a window's posts by, best and worst limit of each, scored
// against the workspace's typical post at the same age.
func (s *Service) Performers(ctx context.Context, rng timeframe.Range, sc *Scope, by string, limit int) (*PerformersResult, string, error) {
	if s.Repo == nil {
		return nil, ReasonNotConfigured, nil
	}
	cur, err := s.Repo.PublishedBetween(ctx, rng.From, rng.To)
	if err != nil {
		return nil, "", err
	}
	if err := s.FilterRows(ctx, sc, &cur); err != nil {
		return nil, "", err
	}
	if len(cur) == 0 {
		return nil, ReasonNoData, nil
	}
	// The baseline stays workspace-wide under any scope, so a campaign's posts
	// are scored against the workspace's usual post, not against each other.
	samples, err := s.reachSamples(ctx)
	if err != nil {
		return nil, "", err
	}
	ids := make([]string, 0, len(cur))
	seen := map[string]bool{}
	for i := range cur {
		if id, _ := AccountRef(&cur[i]); id != "" && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	known := s.LoadAccounts(ctx, ids)
	now := time.Now().UTC()
	cands := make([]performers.Candidate, 0, len(cur))
	for i := range cur {
		r := cur[i]
		if r.PublishedAt == nil {
			continue
		}
		id, username := AccountRef(&r)
		l := LabelAccount(id, username, known)
		cands = append(cands, performers.Candidate{
			PostID:          r.PostID,
			PublisherPostID: r.PublisherPostID,
			Title:           r.Title,
			Platform:        r.Platform,
			Account:         performers.Account{ID: l.ID, Username: l.Username, DisplayName: l.DisplayName, AvatarURL: l.AvatarURL},
			Reach:           r.Reach,
			Impressions:     r.Impressions,
			Likes:           r.Likes,
			Comments:        r.Comments,
			Shares:          r.Shares,
			EngagementRate:  r.EngagementRate,
			PublishedAt:     *r.PublishedAt,
			AgeDays:         daysBetween(*r.PublishedAt, now),
		})
	}
	res := performers.Build(cands, toPerfSamples(samples), performers.Options{By: by, Limit: limit})
	return &PerformersResult{
		Window:     Window{From: rng.FromISO(), To: rng.ToISO(), Days: rng.Days},
		UpdatedAt:  maxLastChecked(cur),
		By:         res.By,
		TotalPosts: res.TotalPosts,
		Best:       res.Best,
		Worst:      res.Worst,
		Insights:   res.Insights,
	}, "", nil
}

func (s *Service) reachSamples(ctx context.Context) ([]repository.ReachAgeSample, error) {
	if s.ReachSamples != nil {
		return s.ReachSamples(ctx)
	}
	return s.Repo.ReachByAgeSamples(ctx)
}

// AccountLabel is how a post's account is shown.
type AccountLabel struct {
	ID          string
	Username    string
	DisplayName string
	AvatarURL   string
}

// AccountRef pulls the owning account's id and username from a row's
// per-platform breakdown, preferring the entry of the row's platform.
func AccountRef(r *models.PostAnalytics) (id, username string) {
	for _, pa := range r.PlatformAnalytics {
		if strings.EqualFold(pa.Platform, r.Platform) {
			return pa.AccountID, pa.AccountUsername
		}
	}
	if len(r.PlatformAnalytics) > 0 {
		return r.PlatformAnalytics[0].AccountID, r.PlatformAnalytics[0].AccountUsername
	}
	return "", ""
}

// LoadAccounts fetches the social accounts of ids, keyed by id. Best effort:
// without them labels fall back to usernames.
func (s *Service) LoadAccounts(ctx context.Context, ids []string) map[string]models.SocialAccount {
	out := map[string]models.SocialAccount{}
	if s.Accounts == nil || len(ids) == 0 {
		return out
	}
	rows, err := s.Accounts.ListByIDs(ctx, ids)
	if err != nil {
		slog.WarnContext(ctx, "analytics account labels unavailable; falling back to usernames", "error", err, "accounts", len(ids))
		return out
	}
	for _, a := range rows {
		out[a.ID] = a
	}
	return out
}

// LabelAccount composes one account's display: the display name and avatar
// from its social account when there is one, else the username.
func LabelAccount(id, username string, known map[string]models.SocialAccount) AccountLabel {
	l := AccountLabel{ID: id, Username: username}
	if a, ok := known[id]; ok && id != "" {
		if l.Username == "" {
			l.Username = a.Username
		}
		l.DisplayName = a.DisplayName
		l.AvatarURL = a.AvatarURL
	}
	if l.DisplayName == "" {
		l.DisplayName = l.Username
	}
	return l
}

func toPostPoints(rows []models.PostAnalytics) []overview.PostPoint {
	out := make([]overview.PostPoint, 0, len(rows))
	for i := range rows {
		r := rows[i]
		if r.PublishedAt == nil {
			continue
		}
		out = append(out, overview.PostPoint{At: *r.PublishedAt, Reach: r.Reach, Interactions: r.Likes + r.Comments + r.Shares})
	}
	return out
}

func followerDailyTotals(pts []repository.FollowerSeriesPoint) []overview.FollowerDayTotal {
	byDay := map[time.Time]int{}
	for _, p := range pts {
		byDay[truncDay(p.PointDate)] += int(p.Followers)
	}
	out := make([]overview.FollowerDayTotal, 0, len(byDay))
	for d, t := range byDay {
		out = append(out, overview.FollowerDayTotal{Date: d, Total: t})
	}
	slices.SortFunc(out, func(a, b overview.FollowerDayTotal) int { return a.Date.Compare(b.Date) })
	return out
}

// MaxLastChecked is the latest refresh of the rows.
func MaxLastChecked(sets ...[]models.PostAnalytics) time.Time { return maxLastChecked(sets...) }

func maxLastChecked(sets ...[]models.PostAnalytics) time.Time {
	var latest time.Time
	for _, s := range sets {
		for i := range s {
			if s[i].LastCheckedAt.After(latest) {
				latest = s[i].LastCheckedAt
			}
		}
	}
	return latest
}

func toPerfSamples(in []repository.ReachAgeSample) []performers.Sample {
	out := make([]performers.Sample, len(in))
	for i, s := range in {
		out[i] = performers.Sample{Platform: s.Platform, PostID: s.PostID, AgeDay: s.AgeDay, Reach: s.Reach, Interactions: s.Interactions}
	}
	return out
}

func daysBetween(from, to time.Time) int {
	return max(int(to.Sub(from)/(24*time.Hour)), 0)
}

func truncDay(t time.Time) time.Time {
	y, m, d := t.UTC().Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}
