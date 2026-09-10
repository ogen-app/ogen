// Package reschedule computes new publish dates for a campaign's non-published
// posts (CON-115). Plan is a pure function — no I/O — so it is unit-testable
// without a database or model. It reassigns the ScheduledAt of draft and
// ready-for-publish posts, spreading each phase's eligible posts evenly across
// that phase's slice of the campaign timeline (posts with no phase spread
// across the whole range). Already-scheduled and published posts are never
// moved (they carry committed publish jobs — see CON-78).
package reschedule

import (
	"cmp"
	"slices"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
)

// Assignment is a post's newly-computed publish date (00:00 UTC).
type Assignment struct {
	PostID      string
	ScheduledAt time.Time
}

// Eligible reports whether a post's status makes it redistributable: only
// posts with no committed schedule (draft, ready-for-publish).
func Eligible(status models.PostStatus) bool {
	return status == models.PostStatusDraft || status == models.PostStatusReadyForPublish
}

// Plan returns a new publish date for every eligible post. It returns nil when
// the campaign has no start/end dates (the caller should ask the user to set
// them first). Ordering is deterministic so runs are stable.
func Plan(campaign *models.Campaign, posts []models.Post) []Assignment {
	if campaign == nil || campaign.StartDate == nil || campaign.EndDate == nil {
		return nil
	}
	start, end := dateOnly(*campaign.StartDate), dateOnly(*campaign.EndDate)

	phases := sortedPhases(campaign)
	windows := computeWindows(start, end, max(len(phases), 1))
	windowByPhase := make(map[string][2]time.Time, len(phases))
	known := make(map[string]bool, len(phases))
	for i, ph := range phases {
		windowByPhase[ph.ID] = windows[i]
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
		dates := spread(win[0], win[1], len(grp))
		for i, p := range grp {
			out = append(out, Assignment{PostID: p.ID, ScheduledAt: dates[i]})
		}
	}
	return out
}

// dateOnly truncates a time to its calendar date at 00:00 UTC.
func dateOnly(t time.Time) time.Time {
	u := t.UTC()
	return time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
}

func sortedPhases(campaign *models.Campaign) []models.CampaignTypePhase {
	if campaign.CampaignType == nil {
		return nil
	}
	phases := append([]models.CampaignTypePhase(nil), campaign.CampaignType.Phases...)
	slices.SortStableFunc(phases, func(a, b models.CampaignTypePhase) int { return cmp.Compare(a.Sequence, b.Sequence) })
	return phases
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

// computeWindows partitions [start, end] into n contiguous inclusive date
// windows in chronological order (remainder to earliest windows). Mirrors
// content_plan's per-phase date partition.
func computeWindows(start, end time.Time, n int) [][2]time.Time {
	if n <= 0 {
		return nil
	}
	out := make([][2]time.Time, n)
	totalDays := int(end.Sub(start).Hours()/24) + 1
	if totalDays < n { // more phases than days → every window is the full range
		for i := range out {
			out[i] = [2]time.Time{start, end}
		}
		return out
	}
	base, rem := totalDays/n, totalDays%n
	cursor := start
	for i := range n {
		d := base
		if i < rem {
			d++
		}
		winEnd := cursor.AddDate(0, 0, d-1)
		out[i] = [2]time.Time{cursor, winEnd}
		cursor = winEnd.AddDate(0, 0, 1)
	}
	return out
}

// spread returns n evenly-spaced dates across the inclusive window [ws, we],
// earliest first. n==1 → the window start.
func spread(ws, we time.Time, n int) []time.Time {
	out := make([]time.Time, n)
	if n <= 0 {
		return out
	}
	if n == 1 {
		out[0] = ws
		return out
	}
	days := int(we.Sub(ws).Hours() / 24) // window span in days (0 for a 1-day window)
	for i := range n {
		// Evenly distribute i across [0, days].
		offset := (i * days) / (n - 1)
		out[i] = ws.AddDate(0, 0, offset)
	}
	return out
}
