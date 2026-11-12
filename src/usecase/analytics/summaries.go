package analytics

import (
	"cmp"
	"context"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/publishers/zernio"
	"github.com/ogen-app/ogen/src/infra/repository"
)

// MinSlotPosts is the fewest posts a best-times slot needs to be ranked, so
// one lucky post doesn't make a slot the best or the weakest.
const MinSlotPosts = 3

// UnitAvgInteractions is the best-times figure: the publisher's average
// interactions per post in the slot, a count, not a rate.
const UnitAvgInteractions = "avg_interactions"

// Slot is one ranked best-times slot. DayOfWeek is 0-6 from Sunday and
// Hour 0-23, both UTC, as the publisher counts them.
type Slot struct {
	DayOfWeek       int     `json:"dayOfWeek"`
	Hour            int     `json:"hour"`
	AvgInteractions float64 `json:"avgInteractions"`
	Posts           int     `json:"posts"`
}

// BestTimesSummary ranks the slots with at least MinPosts posts: the best,
// the best on another day, and the weakest. TooFew is no slot with enough
// posts to rank.
type BestTimesSummary struct {
	Unit     string `json:"unit"`
	MinPosts int    `json:"minPosts"`
	Best     *Slot  `json:"best,omitempty"`
	RunnerUp *Slot  `json:"runnerUp,omitempty"`
	Weakest  *Slot  `json:"weakest,omitempty"`
	TooFew   bool   `json:"tooFew,omitzero"`
}

// SummarizeBestTimes ranks the slots by average interactions, more posts
// first on a tie. The runner-up is the best slot on another day than the
// best's; the weakest is named only when it isn't one of those two.
func SummarizeBestTimes(slots []zernio.BestTimeSlot) BestTimesSummary {
	out := BestTimesSummary{Unit: UnitAvgInteractions, MinPosts: MinSlotPosts}
	var ranked []Slot
	for _, s := range slots {
		if s.PostCount >= MinSlotPosts {
			ranked = append(ranked, Slot{DayOfWeek: s.DayOfWeek, Hour: s.Hour, AvgInteractions: s.AvgEngagement, Posts: s.PostCount})
		}
	}
	if len(ranked) == 0 {
		out.TooFew = true
		return out
	}
	slices.SortStableFunc(ranked, func(a, b Slot) int {
		return cmp.Or(cmp.Compare(b.AvgInteractions, a.AvgInteractions), cmp.Compare(b.Posts, a.Posts),
			cmp.Compare(a.DayOfWeek, b.DayOfWeek), cmp.Compare(a.Hour, b.Hour))
	})
	out.Best = &ranked[0]
	runnerUp := slices.IndexFunc(ranked, func(s Slot) bool { return s.DayOfWeek != ranked[0].DayOfWeek })
	if runnerUp > 0 {
		out.RunnerUp = &ranked[runnerUp]
	}
	if last := len(ranked) - 1; last > 0 && last != runnerUp {
		out.Weakest = &ranked[last]
	}
	return out
}

// HalfLife is when a post reaches half its final engagement: Hours read off
// the curve between the buckets either side of 50%, and the Bucket it falls
// in. Hours is zero when the bucket's label has no age to read.
type HalfLife struct {
	Hours  float64 `json:"hours,omitzero"`
	Bucket string  `json:"bucket"`
}

// DecayHalfLife is the buckets' half-life, nil when they never reach half.
// A bucket's share is cumulative: what a post has reached by the bucket's
// end, its label's upper age, e.g. "6-12h" or "1-3d".
func DecayHalfLife(buckets []zernio.ContentDecayBucket) *HalfLife {
	buckets = slices.SortedFunc(slices.Values(buckets), func(a, b zernio.ContentDecayBucket) int {
		return cmp.Compare(a.BucketOrder, b.BucketOrder)
	})
	prevHours, prevPct, known := 0.0, 0.0, true
	for _, b := range buckets {
		end, ok := bucketEndHours(b.BucketLabel)
		if b.AvgPctOfFinal >= 50 {
			h := &HalfLife{Bucket: b.BucketLabel}
			if ok && known && b.AvgPctOfFinal > prevPct {
				h.Hours = math.Round((prevHours+(50-prevPct)/(b.AvgPctOfFinal-prevPct)*(end-prevHours))*10) / 10
			}
			return h
		}
		prevHours, prevPct, known = end, b.AvgPctOfFinal, ok
	}
	return nil
}

// bucketEndHours is a decay bucket label's upper age in hours: "0-6h" is 6,
// "1-3d" is 72, "12h" is 12. An open bucket ("7d+") has none.
func bucketEndHours(label string) (float64, bool) {
	label = strings.ToLower(strings.TrimSpace(label))
	if strings.HasSuffix(label, "+") {
		return 0, false
	}
	unit := 1.0
	switch {
	case strings.HasSuffix(label, "h"):
		label = strings.TrimSuffix(label, "h")
	case strings.HasSuffix(label, "d"):
		label, unit = strings.TrimSuffix(label, "d"), 24
	case strings.HasSuffix(label, "w"):
		label, unit = strings.TrimSuffix(label, "w"), 24*7
	default:
		return 0, false
	}
	if _, after, ok := strings.Cut(label, "-"); ok {
		label = after
	}
	n, err := strconv.ParseFloat(strings.TrimSpace(label), 64)
	if err != nil {
		return 0, false
	}
	return n * unit, true
}

// CadenceWeeks is how many weeks, this one included, the posting cadence
// counts.
const CadenceWeeks = 12

// WeekCount is the posts published in the ISO week starting WeekStart
// (a Monday, UTC); Current is this week, still under way.
type WeekCount struct {
	WeekStart string `json:"weekStart"`
	Posts     int    `json:"posts"`
	Current   bool   `json:"current,omitzero"`
}

// PlatformCadence is one platform's posts a week over the full weeks.
// Platform is the platform's slug.
type PlatformCadence struct {
	Platform string  `json:"platform"`
	PerWeek  float64 `json:"perWeek"`
}

// PostingCadence is how often the workspace actually posts, from the posts
// published through Ogen: each of the last CadenceWeeks weeks, the average
// over the full ones, last calendar month's count, and each platform's
// average.
type PostingCadence struct {
	Weeks     []WeekCount       `json:"weeks"`
	PerWeek   float64           `json:"perWeek"`
	LastMonth int               `json:"lastMonth"`
	Platforms []PlatformCadence `json:"platforms"`
}

// PostingCadence counts the posts published through Ogen, on the platform
// named (its display name) or every one; nil with ReasonNotConfigured when
// the analytics database isn't.
func (s *Service) PostingCadence(ctx context.Context, platformName string, now time.Time) (*PostingCadence, string, error) {
	if s.Repo == nil {
		return nil, ReasonNotConfigured, nil
	}
	from := isoWeekStart(now).AddDate(0, 0, -7*(CadenceWeeks-1))
	if m := lastMonthStart(now); m.Before(from) {
		from = m
	}
	rows, err := s.Repo.PublishedBetween(ctx, from, now)
	if err != nil {
		return nil, "", err
	}
	if platformName != "" {
		rows = slices.DeleteFunc(rows, func(r models.PostAnalytics) bool { return !strings.EqualFold(r.Platform, platformName) })
	}
	slugs, err := s.slugsByName(ctx)
	if err != nil {
		return nil, "", err
	}
	return CadenceOf(rows, slugs, now), "", nil
}

// CadenceOf counts rows' publish days into weeks and last month, as of now.
// slugOf maps a platform's lower-cased display name to its slug; a name
// without one stays as it is.
func CadenceOf(rows []models.PostAnalytics, slugOf map[string]string, now time.Time) *PostingCadence {
	now = now.UTC()
	first := isoWeekStart(now).AddDate(0, 0, -7*(CadenceWeeks-1))
	out := &PostingCadence{Weeks: make([]WeekCount, CadenceWeeks), Platforms: []PlatformCadence{}}
	for i := range out.Weeks {
		out.Weeks[i].WeekStart = first.AddDate(0, 0, 7*i).Format(time.DateOnly)
	}
	out.Weeks[CadenceWeeks-1].Current = true
	monthFrom, monthTo := lastMonthStart(now), lastMonthStart(now).AddDate(0, 1, 0)
	perPlatform := map[string]int{}
	full := 0
	for _, r := range rows {
		if r.PublishedAt == nil {
			continue
		}
		at := r.PublishedAt.UTC()
		if !at.Before(monthFrom) && at.Before(monthTo) {
			out.LastMonth++
		}
		if at.Before(first) || at.After(now) {
			continue
		}
		i := int(isoWeekStart(at).Sub(first).Hours() / (24 * 7))
		out.Weeks[i].Posts++
		if i < CadenceWeeks-1 {
			full++
			name := strings.ToLower(r.Platform)
			perPlatform[cmp.Or(slugOf[name], name)]++
		}
	}
	const fullWeeks = CadenceWeeks - 1
	out.PerWeek = math.Round(float64(full)/fullWeeks*10) / 10
	for _, p := range slices.Sorted(maps.Keys(perPlatform)) {
		out.Platforms = append(out.Platforms, PlatformCadence{Platform: p, PerWeek: math.Round(float64(perPlatform[p])/fullWeeks*10) / 10})
	}
	return out
}

// isoWeekStart is the Monday 00:00 UTC that starts t's ISO week.
func isoWeekStart(t time.Time) time.Time {
	t = t.UTC()
	day := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	return day.AddDate(0, 0, -(int(day.Weekday())+6)%7)
}

// lastMonthStart is the first day of the calendar month before t's, UTC.
func lastMonthStart(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month()-1, 1, 0, 0, 0, 0, time.UTC)
}

// ConnectedPlatforms is the slugs of the platforms the workspace has an
// active account on in its publishing profile, sorted; none when the
// publisher isn't set up.
func (s *Service) ConnectedPlatforms(ctx context.Context) ([]string, error) {
	if s.Accounts == nil || s.ProfileID == nil {
		return nil, nil
	}
	profile, err := s.ProfileID(ctx)
	if err != nil || profile == "" {
		return nil, err
	}
	accounts, err := s.Accounts.ListActive(ctx, profile)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, a := range accounts {
		out = append(out, strings.ToLower(a.Platform))
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}

// slugsByName maps each platform's lower-cased display name to its slug.
func (s *Service) slugsByName(ctx context.Context) (map[string]string, error) {
	bySlug, err := s.platformsBySlug(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(bySlug))
	for slug, p := range bySlug {
		out[strings.ToLower(p.Name)] = slug
	}
	return out, nil
}

// FollowerDay is the day the accounts together gained the most followers,
// and the post that reached most published that day or the day before.
type FollowerDay struct {
	Date      string `json:"date"`
	Gain      int64  `json:"gain"`
	PostID    string `json:"post_id,omitempty"`
	PostTitle string `json:"post_title,omitempty"`
}

// biggestDay is the date with the largest follower gain over the day
// before, summed over the accounts; nil when none gained. points are daily
// and ordered by account and date; a gap of more than a day isn't counted,
// so a gain is never one day's when it took several.
func biggestDay(points []repository.FollowerSeriesPoint) *FollowerDay {
	gains := map[time.Time]int64{}
	for i := 1; i < len(points); i++ {
		prev, p := points[i-1], points[i]
		if prev.SocialAccountID != p.SocialAccountID || !prev.PointDate.AddDate(0, 0, 1).Equal(p.PointDate) {
			continue
		}
		gains[p.PointDate] += p.Followers - prev.Followers
	}
	var best *FollowerDay
	var bestDate time.Time
	for d, g := range gains {
		if g <= 0 || best != nil && (g < best.Gain || g == best.Gain && d.After(bestDate)) {
			continue
		}
		best, bestDate = &FollowerDay{Date: d.Format(time.DateOnly), Gain: g}, d
	}
	return best
}

// postOfDay names the post that reached most among those published on day
// or the day before, when the analytics database is there.
func (s *Service) postOfDay(ctx context.Context, day *FollowerDay) error {
	if s.Repo == nil || day == nil {
		return nil
	}
	d, err := time.Parse(time.DateOnly, day.Date)
	if err != nil {
		return err
	}
	rows, err := s.Repo.PublishedBetween(ctx, d.AddDate(0, 0, -1), d.AddDate(0, 0, 1))
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		return nil
	}
	top := slices.MaxFunc(rows, func(a, b models.PostAnalytics) int { return cmp.Compare(a.Reach, b.Reach) })
	day.PostID, day.PostTitle = top.PostID, top.Title
	return nil
}
