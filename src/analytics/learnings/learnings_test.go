package learnings

import (
	"testing"
	"time"
)

var now = time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)

func TestHeatmapStrongestSlot(t *testing.T) {
	hi := time.Date(2026, 8, 6, 18, 0, 0, 0, time.UTC) // some Thursday 18:00
	lo := time.Date(2026, 8, 4, 9, 0, 0, 0, time.UTC)  // a Tuesday 09:00
	var posts []PostFact
	for range 3 {
		posts = append(posts, PostFact{PublishedAt: hi, Platform: "linkedin", Reach: 1000})
		posts = append(posts, PostFact{PublishedAt: lo, Platform: "linkedin", Reach: 100})
	}
	h := buildHeatmap(posts, MetricReach)
	if h.InsufficientHistory {
		t.Fatalf("6 posts should be enough for a heatmap")
	}
	if h.MeasuredPosts != 6 {
		t.Fatalf("measured = %d, want 6", h.MeasuredPosts)
	}
	if h.Strongest == nil || h.Strongest.Hour != 18 || h.Strongest.DayOfWeek != int(hi.Weekday()) {
		t.Fatalf("strongest = %+v, want Thu 18:00", h.Strongest)
	}
	if h.Strongest.PostCount != 3 {
		t.Fatalf("strongest post_count = %d, want 3", h.Strongest.PostCount)
	}
}

func TestHeatmapInsufficient(t *testing.T) {
	posts := []PostFact{{PublishedAt: now, Reach: 1}, {PublishedAt: now, Reach: 2}}
	if h := buildHeatmap(posts, MetricReach); !h.InsufficientHistory {
		t.Fatalf("2 posts should be insufficient")
	}
}

func TestLifespanPercentiles(t *testing.T) {
	// 8 identical settled posts: 50% of final by 20h, 75% by 40h, 95% by 90h.
	var pts []LifespanPoint
	for i := range 8 {
		id := itoa(i)
		pts = append(pts,
			LifespanPoint{PostID: id, AgeHours: 0, Reach: 0},
			LifespanPoint{PostID: id, AgeHours: 20, Reach: 500},
			LifespanPoint{PostID: id, AgeHours: 40, Reach: 750},
			LifespanPoint{PostID: id, AgeHours: 90, Reach: 950},
			LifespanPoint{PostID: id, AgeHours: 120, Reach: 1000},
		)
	}
	l := buildLifespan(pts)
	if l.InsufficientHistory {
		t.Fatalf("8 settled posts should be enough")
	}
	if l.SettledPosts != 8 {
		t.Fatalf("settled = %d, want 8", l.SettledPosts)
	}
	if l.T50Hours != 20 || l.T75Hours != 40 || l.T95Hours != 90 {
		t.Fatalf("t50/t75/t95 = %d/%d/%d, want 20/40/90", l.T50Hours, l.T75Hours, l.T95Hours)
	}
	if len(l.Curve) == 0 || l.Curve[0].AgeHours != 0 {
		t.Fatalf("curve should start at age 0, got %+v", l.Curve)
	}
}

func TestLifespanInsufficient(t *testing.T) {
	// only 2 settled posts
	var pts []LifespanPoint
	for i := range 2 {
		id := itoa(i)
		pts = append(pts, LifespanPoint{PostID: id, AgeHours: 0, Reach: 0}, LifespanPoint{PostID: id, AgeHours: 120, Reach: 1000})
	}
	if l := buildLifespan(pts); !l.InsufficientHistory {
		t.Fatalf("2 settled posts should be insufficient")
	}
}

func TestPatternsWorks(t *testing.T) {
	pub := now.AddDate(0, 0, -10) // recent (within trend window), all same weekday bucket
	var posts []PostFact
	for range 8 {
		posts = append(posts, PostFact{PublishedAt: pub, Platform: "linkedin", Reach: 3000, MediaCount: 2, ContentLength: 200, HashtagCount: 1}) // carousel
		posts = append(posts, PostFact{PublishedAt: pub, Platform: "linkedin", Reach: 1000, MediaCount: 1, ContentLength: 200, HashtagCount: 1}) // single image
	}
	p := buildPatterns(posts, MetricReach, now, 90)
	if p.InsufficientHistory {
		t.Fatalf("16 posts should be enough")
	}
	if len(p.Works) == 0 {
		t.Fatalf("expected a 'works' card, got none")
	}
	top := p.Works[0]
	if top.Dimension != "media_format" || top.Segment != "carousel" {
		t.Fatalf("top works = %s/%s, want media_format/carousel", top.Dimension, top.Segment)
	}
	if top.Lift < 1.4 || top.Lift > 1.6 {
		t.Fatalf("carousel lift = %v, want ~1.5", top.Lift)
	}
	if top.Support != 8 {
		t.Fatalf("carousel support = %d, want 8", top.Support)
	}
}

// Finding 3 regression: with many tied works cards, ranking + capCards must be
// deterministic (ID tie-breaker) across repeated runs.
func TestPatternsDeterministicOrder(t *testing.T) {
	pub := now.AddDate(0, 0, -10)
	// Build >3 dimensions that each clear the works threshold with the SAME
	// lift/support, so only the ID tie-breaker orders them. Each dimension gets a
	// strong segment (8 posts at 3000) and a weak one (8 at 1000); overall median
	// lands at 2000 → strong lift 1.5 in every dimension.
	base := func(f *PostFact, strong bool) {
		if strong {
			f.Reach = 3000
		} else {
			f.Reach = 1000
		}
	}
	var posts []PostFact
	for range 8 {
		// media_format: carousel(strong) vs single_image(weak)
		c := PostFact{PublishedAt: pub, Platform: "linkedin", MediaCount: 2, ContentLength: 50, HashtagCount: 0, HasLink: true}
		s := PostFact{PublishedAt: pub, Platform: "instagram", MediaCount: 1, ContentLength: 500, HashtagCount: 5, HasLink: false}
		base(&c, true)
		base(&s, false)
		posts = append(posts, c, s)
	}
	var firstIDs []string
	for run := range 5 {
		p := buildPatterns(posts, MetricReach, now, 90)
		if len(p.Works) == 0 || len(p.Works) > maxCards {
			t.Fatalf("works len = %d, want 1..%d", len(p.Works), maxCards)
		}
		ids := make([]string, len(p.Works))
		for i, c := range p.Works {
			ids[i] = c.ID
		}
		if run == 0 {
			firstIDs = ids
			continue
		}
		for i := range ids {
			if ids[i] != firstIDs[i] {
				t.Fatalf("run %d order %v differs from %v", run, ids, firstIDs)
			}
		}
	}
}

func TestPatternsInsufficient(t *testing.T) {
	posts := []PostFact{{PublishedAt: now, Reach: 100}}
	if p := buildPatterns(posts, MetricReach, now, 90); !p.InsufficientHistory {
		t.Fatalf("1 post should be insufficient for patterns")
	}
}

func TestBuildScope(t *testing.T) {
	since := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	resp := Build(Inputs{Now: now, Since: &since, TrendWindowDays: 30, Metric: MetricSaves, Posts: nil, Lifespan: nil})
	if resp.Scope.Metric != "saves" || resp.Scope.TrendWindowDays != 30 {
		t.Fatalf("scope = %+v", resp.Scope)
	}
	if resp.Scope.Since == nil || *resp.Scope.Since != "2026-01-01" {
		t.Fatalf("scope.since = %v, want 2026-01-01", resp.Scope.Since)
	}
	// empty inputs → every section degrades, never nil
	if resp.Heatmap == nil || !resp.Heatmap.InsufficientHistory ||
		resp.Lifespan == nil || !resp.Lifespan.InsufficientHistory ||
		resp.Patterns == nil || !resp.Patterns.InsufficientHistory {
		t.Fatalf("empty inputs should degrade all sections")
	}
}

// small itoa to avoid an strconv import in tests.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [8]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
