package calendarcheck

import (
	"cmp"
	"fmt"
	"maps"
	"math"
	"slices"
	"time"

	"github.com/ogen-app/ogen/src/domain/campaignphase"
	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/usecase/campaign_actions/reschedule"
	"github.com/ogen-app/ogen/src/usecase/campaign_actions/state"
	"github.com/ogen-app/ogen/src/usecase/campaigngoal"
	"github.com/ogen-app/ogen/src/usecase/scheduling"
)

// calendar is the campaign's dated posts by day, for the checks that look
// for a free day to move a post to.
type calendar struct {
	c       *models.Campaign
	day     func(time.Time) time.Time
	today   time.Time
	dated   bool
	start   time.Time
	end     time.Time
	enabled map[time.Weekday]bool
	// posts are the dated posts, published ones too, in date order.
	posts []dated
	// busy counts posts per "platform|YYYY-MM-DD", the moves offered so far
	// included.
	busy map[string]int
}

// dated is a post with its day in the campaign's timezone.
type dated struct {
	*models.Post
	day time.Time
}

func newCalendar(c *models.Campaign, posts []models.Post, day func(time.Time) time.Time, today time.Time) *calendar {
	k := &calendar{c: c, day: day, today: today, enabled: scheduling.EnabledWeekdays(c.PublishingDays), busy: map[string]int{}}
	if c.StartDate != nil && c.EndDate != nil {
		k.dated, k.start, k.end = true, campaignphase.Date(*c.StartDate), campaignphase.Date(*c.EndDate)
	}
	for i := range posts {
		p := &posts[i]
		if p.ScheduledAt == nil {
			continue
		}
		d := day(*p.ScheduledAt)
		k.posts = append(k.posts, dated{Post: p, day: d})
		k.busy[p.PlatformID+"|"+d.Format(time.DateOnly)]++
	}
	slices.SortStableFunc(k.posts, func(a, b dated) int { return a.ScheduledAt.Compare(*b.ScheduledAt) })
	return k
}

// freeDay is the day nearest from, today or later and inside the campaign,
// that the campaign publishes on, has no post on platform and passes ok.
// It counts the day as taken, so the next move offered goes elsewhere.
func (k *calendar) freeDay(platform string, from time.Time, ok func(time.Time) bool) (time.Time, bool) {
	lo, hi := k.today, from.AddDate(0, 0, 30)
	if k.dated {
		lo, hi = later(k.start, k.today), k.end
	}
	for i := 1; ; i++ {
		later, earlier := from.AddDate(0, 0, i), from.AddDate(0, 0, -i)
		if later.After(hi) && earlier.Before(lo) {
			return time.Time{}, false
		}
		for _, d := range []time.Time{later, earlier} {
			key := platform + "|" + d.Format(time.DateOnly)
			if d.Before(lo) || d.After(hi) || len(k.enabled) > 0 && !k.enabled[d.Weekday()] || k.busy[key] > 0 || !ok(d) {
				continue
			}
			k.busy[key]++
			return d, true
		}
	}
}

// move is the fix moving p to the free day nearest its own; false when
// there is none or p can't be moved.
func (k *calendar) move(p dated, ok func(time.Time) bool) (Fix, bool) {
	if !reschedule.Eligible(p.Status) {
		return Fix{}, false
	}
	to, found := k.freeDay(p.PlatformID, p.day, ok)
	if !found {
		return Fix{}, false
	}
	return Fix{Kind: FixMovePost, Title: fmt.Sprintf("Move %q to %s", p.Title, to.Format("Mon 2 Jan")), PostID: p.ID,
		From: p.day.Format(time.DateOnly), To: to.Format(time.DateOnly)}, true
}

// postTypes is a finding per platform and post type the campaign no longer
// includes, over its unpublished posts of that type. The fixes allow the
// type again, convert the posts to a type it includes, take them off the
// calendar, or delete them.
func postTypes(c *models.Campaign, posts []models.Post) []Finding {
	groups := map[[2]string][]models.Post{}
	for _, p := range posts {
		if p.Status == models.PostStatusPublished || p.PlatformID == "" || p.PlatformPostType == "" {
			continue
		}
		allowed := state.AllowedPostTypes(c, p.PlatformID)
		if allowed == nil || slices.Contains(allowed, p.PlatformPostType) {
			continue
		}
		g := [2]string{p.PlatformID, p.PlatformPostType}
		groups[g] = append(groups[g], p)
	}
	var out []Finding
	for _, g := range slices.SortedFunc(maps.Keys(groups), func(a, b [2]string) int {
		return cmp.Or(cmp.Compare(a[0], b[0]), cmp.Compare(a[1], b[1]))
	}) {
		platform, postType := g[0], g[1]
		var ids, onCalendar []string
		thread := postType == models.PostTypeThread
		for _, p := range groups[g] {
			ids = append(ids, p.ID)
			if p.ScheduledAt != nil {
				onCalendar = append(onCalendar, p.ID)
			}
		}
		ids, name := sorted(ids), platformName(c, platform)
		fixes := []Fix{{Kind: FixEnablePostType, Title: fmt.Sprintf("Allow %s on %s again", postType, name), PlatformID: platform, PostType: postType}}
		// A thread's segments and a link post's link don't carry over to
		// another type, so those are converted by hand.
		converts := 0
		for _, t := range state.AllowedPostTypes(c, platform) {
			if thread || t == models.PostTypeThread || t == models.PostTypeLinkPost || converts == ConvertTypes {
				continue
			}
			fixes = append(fixes, Fix{Kind: FixConvertPostType, Title: "Make them " + t, PostIDs: ids, PostType: t})
			converts++
		}
		if len(onCalendar) > 0 {
			fixes = append(fixes, Fix{Kind: FixUnschedule, Title: "Take them off the calendar", PostIDs: sorted(onCalendar)})
		}
		fixes = append(fixes, Fix{Kind: FixDelete, Title: "Delete them", PostIDs: ids})
		out = append(out, Finding{Code: CodePostTypeNotAllowed, Severity: SeverityWarning, PostIDs: ids,
			Message: fmt.Sprintf("%d %s post(s) on %s use a post type the campaign no longer includes there", len(ids), postType, name),
			Fixes:   fixes})
	}
	return out
}

// closeRule is one "same thing, too close together" check.
type closeRule struct {
	code string
	days int
	what string
	// shares reports whether two posts share the thing.
	shares func(a, b *models.Post) bool
}

// closeTogether is a finding per pair of posts on one platform from the
// same idea within SameIdeaDays, or the same source within SameSourceDays,
// a pair counted once. The fix moves the later post when it can be moved,
// else the earlier, to the nearest day far enough from every post it
// shares the thing with.
func (k *calendar) closeTogether(ideas map[string][]string) []Finding {
	rules := []closeRule{
		{code: CodeSameIdea, days: SameIdeaDays, what: "come from the same idea", shares: func(a, b *models.Post) bool {
			return slices.ContainsFunc(ideas[a.ID], func(id string) bool { return slices.Contains(ideas[b.ID], id) })
		}},
		{code: CodeSameSource, days: SameSourceDays, what: "use the same source", shares: func(a, b *models.Post) bool {
			return slices.ContainsFunc(a.UsedAssetIDs, func(id string) bool { return slices.Contains(b.UsedAssetIDs, id) })
		}},
	}
	seen := map[[2]string]bool{}
	var out []Finding
	for _, r := range rules {
		for i, a := range k.posts {
			for _, b := range k.posts[i+1:] {
				pair := [2]string{a.ID, b.ID}
				if seen[pair] || a.PlatformID != b.PlatformID || days(a.day, b.day) >= r.days ||
					a.Status == models.PostStatusPublished && b.Status == models.PostStatusPublished || !r.shares(a.Post, b.Post) {
					continue
				}
				seen[pair] = true
				f := Finding{Code: r.code, Severity: SeverityWarning, PostIDs: sorted(pair[:]), Fixes: []Fix{},
					Message: fmt.Sprintf("%q and %q %s and publish %s on %s", a.Title, b.Title, r.what,
						apart(days(a.day, b.day)), cmp.Or(platformName(k.c, a.PlatformID), "no platform"))}
				for _, p := range []dated{b, a} {
					fix, ok := k.move(p, func(d time.Time) bool { return k.farFrom(p, d, r) })
					if ok {
						f.Fixes = append(f.Fixes, fix)
						break
					}
				}
				out = append(out, f)
			}
		}
	}
	return out
}

// farFrom reports whether day d is at least r.days from every other post on
// p's platform that shares r's thing with p.
func (k *calendar) farFrom(p dated, d time.Time, r closeRule) bool {
	for _, o := range k.posts {
		if o.ID != p.ID && o.PlatformID == p.PlatformID && days(o.day, d) < r.days && r.shares(p.Post, o.Post) {
			return false
		}
	}
	return true
}

// crowdedSlots is a finding per run of unpublished posts to one account on
// one platform, each less than the slot (SlotMinutes, or the campaign's
// spread when wider) after the one before. The fix moves the run's last
// movable post to the nearest free day.
func (k *calendar) crowdedSlots() []Finding {
	slot := time.Duration(max(k.c.SpreadMinutes, SlotMinutes)) * time.Minute
	byAccount := map[string][]dated{}
	var accounts []string
	for _, p := range k.posts {
		if p.Status == models.PostStatusPublished {
			continue
		}
		key := p.PlatformID + "|" + p.SocialAccountID
		if _, ok := byAccount[key]; !ok {
			accounts = append(accounts, key)
		}
		byAccount[key] = append(byAccount[key], p)
	}
	var runs [][]dated
	for _, key := range accounts {
		posts := byAccount[key]
		for i := 0; i < len(posts); {
			j := i + 1
			for j < len(posts) && posts[j].ScheduledAt.Sub(*posts[j-1].ScheduledAt) < slot {
				j++
			}
			if j-i > 1 {
				runs = append(runs, posts[i:j])
			}
			i = j
		}
	}
	var out []Finding
	for _, run := range runs {
		ids := make([]string, len(run))
		for i, p := range run {
			ids[i] = p.ID
		}
		f := Finding{Code: CodeCrowdedSlot, Severity: SeverityWarning, PostIDs: sorted(ids), Fixes: []Fix{},
			Message: fmt.Sprintf("%d posts go to the same %s account within %s of each other around %s", len(run),
				cmp.Or(platformName(k.c, run[0].PlatformID), "no platform"), slot, run[0].ScheduledAt.In(Location(k.c)).Format("Mon 2 Jan 15:04"))}
		for _, p := range slices.Backward(run) {
			if fix, ok := k.move(p, func(time.Time) bool { return true }); ok {
				f.Fixes = append(f.Fixes, fix)
				break
			}
		}
		if len(f.Fixes) == 0 {
			f.Message += "; they're all scheduled, so reschedule one by hand"
		}
		out = append(out, f)
	}
	return out
}

// goal is a finding per goal period not over yet with fewer posts dated in
// it than the goal asks, and per period with half again as many. A partial
// period's target is the goal's share of the days it has. A short period's
// fix plans the missing posts' ideas for what is left of it.
func (k *calendar) goal() []Finding {
	c := k.c
	if c.EstimatedPostCount == nil || *c.EstimatedPostCount <= 0 || !k.dated {
		return nil
	}
	cadence := cmp.Or(c.GoalCadence, campaigngoal.DefaultCadence)
	var out []Finding
	// The campaign's dates are calendar dates, so the periods are too.
	for _, w := range campaigngoal.Windows(cadence, &k.start, &k.end, time.UTC) {
		from, until := w.Start, w.End // until is exclusive
		if !until.After(k.today) {
			continue
		}
		target := share(*c.EstimatedPostCount, cadence, from, until)
		n := 0
		for _, p := range k.posts {
			if !p.day.Before(from) && p.day.Before(until) {
				n++
			}
		}
		scope := from.Format(time.DateOnly)
		switch {
		case n < target:
			f := Finding{Code: CodeGoalShort, Severity: SeverityInfo, scope: scope, Fixes: []Fix{},
				Message: fmt.Sprintf("%s has %d of the %d posts the goal asks for", w.Label, n, target)}
			if first := later(from, k.today); first.Before(until) {
				f.Fixes = append(f.Fixes, Fix{Kind: FixPlanWindow, Title: fmt.Sprintf("Plan %d more for %s", target-n, w.Label),
					From: first.Format(time.DateOnly), To: until.AddDate(0, 0, -1).Format(time.DateOnly), Count: target - n})
			}
			out = append(out, f)
		case n > target+target/2:
			out = append(out, Finding{Code: CodeGoalOver, Severity: SeverityInfo, scope: scope, Fixes: []Fix{},
				Message: fmt.Sprintf("%s has %d posts, well over the goal's %d", w.Label, n, target)})
		}
	}
	return out
}

// share is a period's part of the goal: perPeriod for a whole period, and
// for a partial one its share of the days, rounded up.
func share(perPeriod int, cadence string, from, until time.Time) int {
	full := 7
	if cadence == campaigngoal.CadenceMonth {
		full = time.Date(from.Year(), from.Month()+1, 0, 0, 0, 0, 0, time.UTC).Day()
	}
	n := days(from, until)
	if n >= full {
		return perPeriod
	}
	return int(math.Ceil(float64(perPeriod) * float64(n) / float64(full)))
}

// later is the later of two instants.
func later(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// days is how many days apart two days are.
func days(a, b time.Time) int {
	n := int(math.Round(b.Sub(a).Hours() / 24))
	return max(n, -n)
}

func apart(n int) string {
	switch n {
	case 0:
		return "the same day"
	case 1:
		return "a day apart"
	}
	return fmt.Sprintf("%d days apart", n)
}

// platformName is the platform's name on the campaign, else its id.
func platformName(c *models.Campaign, id string) string {
	if i := slices.IndexFunc(c.Platforms, func(p models.Platform) bool { return p.ID == id }); i >= 0 {
		return c.Platforms[i].Name
	}
	return id
}
