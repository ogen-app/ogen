package analytics

import (
	"context"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ogen-app/ogen/src/analytics/learnings"
	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
)

// DefaultTrendWindowDays is the fading comparison window when none is given.
const DefaultTrendWindowDays = 90

// LessonsOptions shape the lessons read.
type LessonsOptions struct {
	// Since is the baseline's lower bound; nil is all time.
	Since *time.Time
	// TrendWindowDays drives the fading comparison; zero is
	// DefaultTrendWindowDays (Build's own default).
	TrendWindowDays int
	// Metric is learnings.MetricReach (default) or learnings.MetricSaves.
	Metric string
}

// Lessons is what the workspace's history has taught: the day×hour heatmap,
// the post-lifespan curve and the structural patterns that work or fade.
// Only the platform part of the scope applies: a lesson mined from one
// campaign is too thin a claim. A nil response comes with the reason there
// is none.
func (s *Service) Lessons(ctx context.Context, sc *Scope, opt LessonsOptions) (*learnings.Response, string, error) {
	if s.Repo == nil {
		return nil, ReasonNotConfigured, nil
	}
	current, err := s.Repo.CurrentMetricsByPostID(ctx)
	if err != nil {
		return nil, "", err
	}
	if len(current) == 0 {
		return nil, ReasonNoData, nil
	}
	var since time.Time
	if opt.Since != nil {
		since = *opt.Since
	}
	var posts []models.Post
	if s.Posts != nil {
		if posts, err = s.Posts.ListPublishedSince(ctx, since); err != nil {
			return nil, "", err
		}
	}
	life, err := s.lifespanSamples(ctx, since)
	if err != nil {
		return nil, "", err
	}
	posts, life = narrowLessons(sc, posts, life)
	resp := learnings.Build(learnings.Inputs{
		Now:             time.Now().UTC(),
		Since:           opt.Since,
		TrendWindowDays: opt.TrendWindowDays,
		Metric:          opt.Metric,
		Posts:           postFacts(posts, current, opt.Since),
		Lifespan:        toLifespanPoints(life),
		UpdatedAt:       maxLastCheckedMap(current),
	})
	return &resp, "", nil
}

func (s *Service) lifespanSamples(ctx context.Context, since time.Time) ([]repository.LifespanSample, error) {
	if s.LifespanSamples != nil {
		return s.LifespanSamples(ctx, since)
	}
	return s.Repo.LifespanSamples(ctx, since)
}

// narrowLessons drops the posts and lifespan samples outside the scope. It
// runs before Build so each section's minimum-support floor applies to the
// filtered sample: a thin single-platform history withdraws the section as
// insufficient_history rather than serving a weak lesson.
func narrowLessons(sc *Scope, posts []models.Post, samples []repository.LifespanSample) ([]models.Post, []repository.LifespanSample) {
	if !sc.NarrowsPlatforms() {
		return posts, samples
	}
	posts = slices.DeleteFunc(posts, func(p models.Post) bool { return !sc.AllowsPlatformID(p.PlatformID) })
	inScope := make(map[string]bool, len(posts))
	for i := range posts {
		inScope[posts[i].ID] = true
	}
	samples = slices.DeleteFunc(samples, func(s repository.LifespanSample) bool { return !inScope[s.PostID] })
	return posts, samples
}

// postFacts joins each published post to its latest metrics, skipping posts
// with none and those published before since.
func postFacts(posts []models.Post, current map[string]*models.PostAnalytics, since *time.Time) []learnings.PostFact {
	facts := make([]learnings.PostFact, 0, len(posts))
	for i := range posts {
		p := posts[i]
		a := current[p.ID]
		if a == nil || a.PublishedAt == nil {
			continue
		}
		if since != nil && a.PublishedAt.Before(*since) {
			continue
		}
		facts = append(facts, learnings.PostFact{
			PublishedAt:   *a.PublishedAt,
			Platform:      a.Platform,
			Reach:         a.Reach,
			Saves:         a.Saves,
			MediaCount:    len(p.MediaURLs),
			IsVideo:       IsVideoPost(p.PlatformPostType),
			ContentLength: utf8.RuneCountInString(p.Content),
			HashtagCount:  strings.Count(p.Content, "#"),
			HasLink:       hasLink(p),
		})
	}
	return facts
}

var trendWindowRe = regexp.MustCompile(`^(\d+)(d|w|mo)$`)

// ParseTrendWindow resolves a fading comparison window like 90d, 12w or 3mo
// to whole days; empty is DefaultTrendWindowDays.
func ParseTrendWindow(s string) (int, bool) {
	if s == "" {
		return DefaultTrendWindowDays, true
	}
	m := trendWindowRe.FindStringSubmatch(s)
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	if err != nil || n <= 0 {
		return 0, false
	}
	perUnit := map[string]int{"d": 1, "w": 7, "mo": 30}[m[2]]
	if perUnit == 0 || n > math.MaxInt/perUnit {
		return 0, false
	}
	return n * perUnit, true
}

// IsVideoPost reports whether a platform post type is a video format.
func IsVideoPost(platformPostType string) bool {
	t := strings.ToLower(platformPostType)
	return strings.Contains(t, "video") || strings.Contains(t, "reel") || strings.Contains(t, "short")
}

func hasLink(p models.Post) bool {
	return p.IsLinkPost() || strings.Contains(strings.ToLower(p.Content), "http")
}

func toLifespanPoints(in []repository.LifespanSample) []learnings.LifespanPoint {
	out := make([]learnings.LifespanPoint, len(in))
	for i, s := range in {
		out[i] = learnings.LifespanPoint{PostID: s.PostID, AgeHours: s.AgeHours, Reach: s.Reach}
	}
	return out
}

func maxLastCheckedMap(current map[string]*models.PostAnalytics) time.Time {
	var latest time.Time
	for _, a := range current {
		if a.LastCheckedAt.After(latest) {
			latest = a.LastCheckedAt
		}
	}
	return latest
}
