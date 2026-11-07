// Package reschedule computes new publish dates for a campaign's non-published
// posts. Plan is a pure function — no I/O — so it is unit-testable
// without a database or model. It reassigns the ScheduledAt of draft and
// ready-for-publish posts, spreading each phase's eligible posts evenly across
// that phase's slice of the campaign timeline (posts with no phase spread
// across the whole range), on the campaign's publishing days. Already-scheduled and published posts are never
// moved (they carry committed publish jobs — see CON-78).
package reschedule

import (
	"cmp"
	"slices"
	"time"

	"github.com/ogen-app/ogen/src/domain/campaignphase"
	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/usecase/scheduling"
)

// Assignment is a post's newly-computed publish date: the campaign-local
// calendar day, as 00:00 UTC.
type Assignment struct {
	PostID      string
	ScheduledAt time.Time
}

// EligibleStatuses are the statuses a redistribution may move: posts with no
// committed schedule.
var EligibleStatuses = []models.PostStatus{models.PostStatusPlanned, models.PostStatusDraft, models.PostStatusReadyForPublish}

// Eligible reports whether a post's status makes it redistributable.
func Eligible(status models.PostStatus) bool {
	return slices.Contains(EligibleStatuses, status)
}

// Plan returns a new publish date for every eligible post. It returns nil when
// the campaign has no start/end dates (the caller should ask the user to set
// them first). Ordering is deterministic so runs are stable.
func Plan(campaign *models.Campaign, posts []models.Post) []Assignment {
	if campaign == nil || campaign.StartDate == nil || campaign.EndDate == nil {
		return nil
	}
	start, end := campaignphase.Date(*campaign.StartDate), campaignphase.Date(*campaign.EndDate)
	enabled := scheduling.EnabledWeekdays(campaign.PublishingDays)

	// Each phase's slice is the campaign's effective phase plan — its
	// manual plan when set, else the even split of the dates.
	phases := campaignphase.SortedPhases(campaign)
	windows, _ := campaignphase.Resolve(campaign)
	windowByPhase := make(map[string][2]time.Time, len(windows))
	for _, w := range windows {
		windowByPhase[w.Phase.ID] = [2]time.Time{w.Start, w.End}
	}
	known := make(map[string]bool, len(phases))
	for _, ph := range phases {
		known[ph.ID] = true
	}

	// Group eligible posts by phase; "" is the unassigned/stale-phase bucket.
	groups := make(map[string][]models.Post)
	for _, p := range posts {
		if !Eligible(p.Status) {
			continue
		}
		key := ""
		if p.CampaignTypePhaseID != nil && known[*p.CampaignTypePhaseID] {
			key = *p.CampaignTypePhaseID
		}
		groups[key] = append(groups[key], p)
	}

	// Deterministic group order: phases in sequence, then the unassigned bucket.
	order := make([]string, 0, len(phases)+1)
	for _, ph := range phases {
		order = append(order, ph.ID)
	}
	order = append(order, "")

	var out []Assignment
	for _, key := range order {
		grp := groups[key]
		if len(grp) == 0 {
			continue
		}
		sortPosts(grp)
		win := [2]time.Time{start, end}
		if key != "" {
			win = windowByPhase[key]
		}
		dates := spread(win[0], win[1], len(grp), enabled)
		for i, p := range grp {
			out = append(out, Assignment{PostID: p.ID, ScheduledAt: dates[i]})
		}
	}
	return out
}

// sortPosts orders a group deterministically: earliest current ScheduledAt
// first (nulls last), then created-at, then id.
func sortPosts(posts []models.Post) {
	slices.SortStableFunc(posts, func(a, b models.Post) int {
		ai, bi := a.ScheduledAt, b.ScheduledAt
		switch {
		case ai != nil && bi != nil && !ai.Equal(*bi):
			return ai.Compare(*bi)
		case ai != nil && bi == nil:
			return -1 // scheduled ones before undated
		case ai == nil && bi != nil:
			return 1
		}
		if !a.CreatedAt.Equal(b.CreatedAt) {
			return a.CreatedAt.Compare(b.CreatedAt)
		}
		return cmp.Compare(a.ID, b.ID)
	})
}

// spread returns n evenly-spaced dates across the inclusive window [ws, we],
// earliest first, on the enabled weekdays only (every day when the window has
// none of them). n==1 → the window's first enabled day.
func spread(ws, we time.Time, n int, enabled map[time.Weekday]bool) []time.Time {
	out := make([]time.Time, n)
	if n <= 0 {
		return out
	}
	var days, all []time.Time
	for d := ws; !d.After(we); d = d.AddDate(0, 0, 1) {
		all = append(all, d)
		if enabled[d.Weekday()] {
			days = append(days, d)
		}
	}
	if len(days) == 0 {
		days = all
	}
	if len(days) == 0 {
		days = []time.Time{ws}
	}
	if n == 1 {
		out[0] = days[0]
		return out
	}
	for i := range n {
		// Evenly distribute i across the window's days.
		out[i] = days[(i*(len(days)-1))/(n-1)]
	}
	return out
}
