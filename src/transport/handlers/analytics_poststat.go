package handlers

import (
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/ogen-app/ogen/src/analytics/learnings"
	"github.com/ogen-app/ogen/src/analytics/poststat"
	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
)

// PostDetail serves the CON-250 per-post statistics drill-down: the post header,
// six metric cards scored against the account's typical post at the same age
// (a per-metric p25/p50/p75 usual-range band plus an against-typical multiplier),
// the per-metric running-total series since publishing, and a deterministic
// narrative wired to the CON-239 lifespan. It reads one post's current row +
// snapshot trajectory and the shared per-platform baseline / lifespan curves;
// there is no window parameter (the series spans [published_at, now]).
//
// It is distinct from GET /api/posts/:id/analytics (CON-93), which stays the lean
// raw-snapshot/pending endpoint. A post the refresh hasn't recorded yet returns
// available:true with an awaiting_platform overview rather than a pending stub.
//
// PostDetail godoc
// @Summary      Per-post statistics
// @Description  Header + six metric cards (against-typical multiplier + usual-range band) + per-metric running-total series + narrative for one published post. Distinct from the lean /api/posts/{id}/analytics snapshot.
// @Tags         analytics
// @Produce      json
// @Security     CookieAuth
// @Param        post_id  path  string  true  "Post id"
// @Success      200  {object}  handlers.insightEnvelope
// @Failure      401  {object}  map[string]string
// @Failure      404  {object}  map[string]string
// @Failure      409  {object}  map[string]string
// @Router       /api/analytics/posts/{post_id} [get]
func (h *AnalyticsHandler) PostDetail(c *fiber.Ctx) error {
	if h.repo == nil {
		return c.JSON(insightEnvelope{Available: false, Reason: reasonNotConfigured})
	}
	ctx := reqCtx(c)

	post, err := h.posts.GetByID(ctx, c.Params("post_id"))
	if err != nil {
		return notFound(err, "post not found")
	}
	// Analytics is defined only for posts published through a publisher; mirror
	// the CON-93 machine-readable 409 so the client can tell "wrong kind of post"
	// from "not found".
	if post.PublisherPostID == "" {
		return c.Status(fiber.StatusConflict).JSON(fiber.Map{
			"code":  "not_published_via_publisher",
			"error": "post has not been published through a publisher",
		})
	}

	cur, err := h.repo.GetByPostID(ctx, post.ID)
	if err != nil {
		return err
	}

	now := time.Now().UTC()

	header := poststat.PostHeader{
		PostID:          post.ID,
		PublisherPostID: post.PublisherPostID,
		Title:           post.Title,
		MediaFormat:     mediaFormat(*post),
		Campaign:        campaignRef(post),
	}
	published := post.PublishedAt
	if cur != nil {
		header.Platform = cur.Platform
		header.Account = poststatAccount(cur)
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
	var publishedAt time.Time
	if published != nil {
		publishedAt = *published
	}
	header.PublishedAt = publishedAt
	age := now.Sub(publishedAt)
	if age < 0 {
		age = 0
	}

	in := poststat.Inputs{
		Now:      now,
		Header:   header,
		Age:      age,
		Platform: header.Platform,
	}

	// No current row yet → the background refresh hasn't covered this post.
	if cur == nil {
		in.UpdatedAt = now
		return c.JSON(insightEnvelope{Available: true, Data: poststat.Build(in)})
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

	samples, err := h.repo.ReachByAgeSamples(ctx)
	if err != nil {
		return err
	}
	in.Samples = toBandSamples(samples)

	snaps, err := h.repo.SnapshotsByPostID(ctx, post.ID)
	if err != nil {
		return err
	}
	in.Snapshots = toPoststatSnapshots(snaps, publishedAt)

	// Workspace-wide lifespan (all-time) drives the half-life narrative + the
	// still-counting cutoff; shared with the CON-239 learnings board.
	lifeSamples, err := h.repo.LifespanSamples(ctx, time.Time{})
	if err != nil {
		return err
	}
	in.Lifespan = lifespanInfo(lifeSamples)

	return c.JSON(insightEnvelope{Available: true, Data: poststat.Build(in)})
}

// mediaFormat classifies a post's format from its media + platform post type,
// mirroring the CON-239 media_format dimension.
func mediaFormat(p models.Post) string {
	if isVideoPost(p.PlatformPostType) {
		return "video"
	}
	switch {
	case len(p.MediaURLs) > 1:
		return "carousel"
	case len(p.MediaURLs) == 1:
		return "single_image"
	case p.CTAUrl != "":
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

// poststatAccount pulls the owning account's username/id from the current row's
// per-platform breakdown, preferring the entry matching the row's platform.
// display_name defaults to the username; avatar enrichment is a follow-up.
func poststatAccount(a *models.PostAnalytics) poststat.Account {
	var username, id string
	for _, pa := range a.PlatformAnalytics {
		if strings.EqualFold(pa.Platform, a.Platform) {
			username, id = pa.AccountUsername, pa.AccountID
			break
		}
	}
	if username == "" && len(a.PlatformAnalytics) > 0 {
		username, id = a.PlatformAnalytics[0].AccountUsername, a.PlatformAnalytics[0].AccountID
	}
	return poststat.Account{ID: id, Username: username, DisplayName: username}
}

// openURL returns the "Open on {platform}" permalink, preferring the entry
// matching the row's platform, then any entry that carries one.
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
		ageH := int(s.OccurredAt.Sub(publishedAt).Hours())
		if ageH < 0 {
			ageH = 0
		}
		out = append(out, poststat.Snapshot{
			AgeHours: ageH,
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
	return &poststat.Lifespan{
		T50Hours:     life.T50Hours,
		T95Hours:     life.T95Hours,
		SettledPosts: life.SettledPosts,
	}
}
