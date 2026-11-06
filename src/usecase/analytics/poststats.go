package analytics

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/ogen-app/ogen/src/analytics/learnings"
	"github.com/ogen-app/ogen/src/analytics/poststat"
	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
)

var (
	// ErrPostNotFound is a post that is unknown or another tenant's.
	ErrPostNotFound = errors.New("post not found")
	// ErrNotPublishedViaPublisher is a post that never went out through a
	// publisher, so it has no analytics.
	ErrNotPublishedViaPublisher = errors.New("post has not been published through a publisher")
)

// PostStats is one published post's statistics: its header, six metric
// cards scored against the workspace's typical post at the same age, the
// running-total series since publishing and a narrative wired to the
// workspace's lifespan. A post the refresh hasn't recorded yet reads as
// awaiting the platform. A nil response comes with the reason there is none.
func (s *Service) PostStats(ctx context.Context, postID string) (*poststat.Response, string, error) {
	if s.Repo == nil {
		return nil, ReasonNotConfigured, nil
	}
	if s.Posts == nil {
		return nil, "", ErrPostNotFound
	}
	post, err := s.Posts.GetByID(ctx, postID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "", ErrPostNotFound
	}
	if err != nil {
		return nil, "", err
	}
	if post.PublisherPostID == "" {
		return nil, "", ErrNotPublishedViaPublisher
	}
	cur, err := s.Repo.GetByPostID(ctx, post.ID)
	if err != nil {
		return nil, "", err
	}
	now := time.Now().UTC()
	header := s.postHeader(ctx, post, cur)
	in := poststat.Inputs{
		Now:      now,
		Header:   header,
		Age:      max(now.Sub(header.PublishedAt), 0),
		Platform: header.Platform,
	}
	// No current row yet: the background refresh hasn't covered this post.
	if cur == nil {
		in.UpdatedAt = now
		resp := poststat.Build(in)
		return &resp, "", nil
	}
	in.UpdatedAt = cur.LastCheckedAt
	in.Current = &poststat.Metrics{
		Reach:          cur.Reach,
		Impressions:    cur.Impressions,
		Interactions:   cur.Likes + cur.Comments + cur.Shares,
		Saves:          cur.Saves,
		Clicks:         cur.Clicks,
		EngagementRate: cur.EngagementRate,
	}
	if err := s.postHistory(ctx, post.ID, header.PublishedAt, &in); err != nil {
		return nil, "", err
	}
	resp := poststat.Build(in)
	return &resp, "", nil
}

// postHistory fills the baselines and the post's own trajectory. The
// workspace baselines and the post's snapshots are independent reads, so
// they run concurrently. The all-time workspace lifespan drives the
// half-life narrative and the still-counting cutoff.
func (s *Service) postHistory(ctx context.Context, postID string, publishedAt time.Time, in *poststat.Inputs) error {
	var (
		samples []repository.ReachAgeSample
		snaps   []models.PostAnalyticsSnapshot
		life    []repository.LifespanSample
	)
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() (err error) { samples, err = s.reachSamples(gctx); return err })
	g.Go(func() (err error) { snaps, err = s.Repo.SnapshotsByPostID(gctx, postID); return err })
	g.Go(func() (err error) { life, err = s.lifespanSamples(gctx, time.Time{}); return err })
	if err := g.Wait(); err != nil {
		return err
	}
	in.Samples = toBandSamples(samples)
	in.Snapshots = toPoststatSnapshots(snaps, publishedAt)
	in.Lifespan = lifespanInfo(life)
	return nil
}

// postHeader is the post's identity block, preferring what the refresh last
// recorded (platform, permalink, publish time, title) over the post's own.
func (s *Service) postHeader(ctx context.Context, post *models.Post, cur *models.PostAnalytics) poststat.PostHeader {
	header := poststat.PostHeader{
		PostID:          post.ID,
		PublisherPostID: post.PublisherPostID,
		Title:           post.Title,
		MediaFormat:     MediaFormat(*post),
		Campaign:        campaignRef(post),
		Account:         s.postAccount(ctx, post, cur),
	}
	published := post.PublishedAt
	if cur != nil {
		header.Platform = cur.Platform
		header.OpenURL = openURL(cur)
		if cur.PublishedAt != nil {
			published = cur.PublishedAt
		}
		if cur.Title != "" {
			header.Title = cur.Title
		}
	}
	if header.Platform == "" && post.Platform != nil {
		header.Platform = post.Platform.Name
	}
	if published != nil {
		header.PublishedAt = *published
	}
	return header
}

// postAccount labels the post's owning account: id and username from the
// current row's platform breakdown, falling back to the post's own social
// account when the refresh hasn't recorded one.
func (s *Service) postAccount(ctx context.Context, post *models.Post, cur *models.PostAnalytics) poststat.Account {
	var id, username string
	if cur != nil {
		id, username = AccountRef(cur)
	}
	if id == "" {
		id = post.SocialAccountID
	}
	var known map[string]models.SocialAccount
	if id != "" {
		known = s.LoadAccounts(ctx, []string{id})
	}
	l := LabelAccount(id, username, known)
	return poststat.Account{ID: l.ID, Username: l.Username, DisplayName: l.DisplayName, AvatarURL: l.AvatarURL}
}

// MediaFormat classifies a post's format from its media and platform post
// type, the same dimension the lessons mine.
func MediaFormat(p models.Post) string {
	if IsVideoPost(p.PlatformPostType) {
		return "video"
	}
	switch {
	case len(p.MediaURLs) > 1:
		return "carousel"
	case len(p.MediaURLs) == 1:
		return "single_image"
	case p.IsLinkPost():
		return "link"
	default:
		return "text"
	}
}

func campaignRef(p *models.Post) *poststat.CampaignRef {
	if p.Campaign == nil {
		return nil
	}
	return &poststat.CampaignRef{ID: p.Campaign.ID, Name: p.Campaign.Name}
}

// openURL is the "Open on {platform}" permalink, preferring the entry of the
// row's platform, then any entry that carries one.
func openURL(a *models.PostAnalytics) string {
	for _, pa := range a.PlatformAnalytics {
		if strings.EqualFold(pa.Platform, a.Platform) && pa.PlatformPostURL != "" {
			return pa.PlatformPostURL
		}
	}
	for _, pa := range a.PlatformAnalytics {
		if pa.PlatformPostURL != "" {
			return pa.PlatformPostURL
		}
	}
	return ""
}

func toBandSamples(in []repository.ReachAgeSample) []poststat.AgeSample {
	out := make([]poststat.AgeSample, len(in))
	for i, s := range in {
		out[i] = poststat.AgeSample{
			Platform:     s.Platform,
			PostID:       s.PostID,
			AgeDay:       s.AgeDay,
			Reach:        s.Reach,
			Interactions: s.Interactions,
			Impressions:  s.Impressions,
			Saves:        s.Saves,
			Clicks:       s.Clicks,
		}
	}
	return out
}

func toPoststatSnapshots(in []models.PostAnalyticsSnapshot, publishedAt time.Time) []poststat.Snapshot {
	out := make([]poststat.Snapshot, 0, len(in))
	for _, s := range in {
		out = append(out, poststat.Snapshot{
			AgeHours: max(int(s.OccurredAt.Sub(publishedAt).Hours()), 0),
			Metrics: poststat.Metrics{
				Reach:          s.Reach,
				Impressions:    s.Impressions,
				Interactions:   s.Likes + s.Comments + s.Shares,
				Saves:          s.Saves,
				Clicks:         s.Clicks,
				EngagementRate: s.EngagementRate,
			},
		})
	}
	return out
}

func lifespanInfo(samples []repository.LifespanSample) *poststat.Lifespan {
	life := learnings.BuildLifespan(toLifespanPoints(samples))
	if life == nil || life.InsufficientHistory {
		return nil
	}
	return &poststat.Lifespan{T50Hours: life.T50Hours, T95Hours: life.T95Hours, SettledPosts: life.SettledPosts}
}
