// Package report computes the Activity daily report (CON-285): deterministic,
// stateless per-local-day counts of what happened in a workspace — posts
// published, posts that failed, posts created, and campaigns created. It is the
// server-side counterpart the Activity UI (CON-225) renders instead of the
// browser-computed function CON-225 §5 originally specified; computing it here
// is why the endpoints require an explicit IANA tz (the server has no other way
// to know the caller's local midnight).
//
// The heavy lifting — bucketing timestamps into local days across midnight and
// DST boundaries — lives in the pure functions below (no I/O, injected window /
// location), so it is unit-tested without a database. The Service wires them to
// tenant-scoped repositories.
package report

import (
	"errors"
	"expvar"
	"sort"
	"time"
)

// Errors the Service returns for the handler to map onto status codes.
var (
	// ErrInvalidTZ is returned when tz is missing or not a loadable IANA zone → 400.
	ErrInvalidTZ = errors.New("invalid tz")
	// ErrInvalidDate is returned when a :date / before value is not YYYY-MM-DD → 400.
	ErrInvalidDate = errors.New("invalid date")
	// ErrCampaignNotFound is returned when campaign_id is unknown in this tenant → 404.
	ErrCampaignNotFound = errors.New("campaign not found")
)

// dateLayout is the wire format for a local calendar day.
const dateLayout = "2006-01-02"

// expvar counters (CON-285 §11), surfaced at /debug/vars.
var (
	// Requests counts every served report/report-list computation.
	Requests = expvar.NewInt("ogen_activity_report_requests")
	// ComputeMillisTotal accumulates wall-clock spent loading+computing reports,
	// a coarse stand-in for a latency histogram.
	ComputeMillisTotal = expvar.NewInt("ogen_activity_report_compute_ms_total")
)

// Row inputs. Each carries the timestamp that attributes it to a local day, plus
// only the fields the report aggregates. The Service loads these tenant-scoped;
// the pure functions never fetch.

// PublishedRow is one post published at At on channel PlatformID.
type PublishedRow struct {
	At         time.Time
	PlatformID string
}

// FailedRow is one post's transition into a terminal non-publish state at At.
type FailedRow struct {
	At            time.Time
	PostID        string
	PlatformID    string
	Status        string // failed | not_published
	FailureReason string
}

// CreatedRow is one post created at At by CreatedBy; Scheduled is true when the
// post carries a scheduled_at (it was given a time, not left an unplanned draft).
type CreatedRow struct {
	At        time.Time
	CreatedBy string
	Scheduled bool
}

// CampaignRow is one campaign created at At.
type CampaignRow struct {
	At         time.Time
	CampaignID string
}

// Inputs is the raw material for a report: the four event streams for a tenant
// (optionally one campaign) over some time span. computeReport filters to a
// single day; bucketReports groups the whole span by local day.
type Inputs struct {
	Published []PublishedRow
	Failed    []FailedRow
	Created   []CreatedRow
	Campaigns []CampaignRow
}

// Window is a half-open instant range [Lo, Hi).
type Window struct {
	Lo, Hi time.Time
}

func (w Window) contains(t time.Time) bool {
	return !t.Before(w.Lo) && t.Before(w.Hi)
}

// dayWindow returns the [00:00 local, next-00:00 local) span of the calendar day
// `date` in loc. AddDate(0,0,1) — not a fixed 24h — so a 23- or 25-hour DST day
// is spanned correctly (CON-285 FR2/§10).
func dayWindow(date string, loc *time.Location) (Window, error) {
	d, err := time.ParseInLocation(dateLayout, date, loc)
	if err != nil {
		return Window{}, ErrInvalidDate
	}
	return Window{Lo: d, Hi: d.AddDate(0, 0, 1)}, nil
}

// ChannelCount is a per-platform tally.
type ChannelCount struct {
	PlatformID string `json:"platform_id"`
	Count      int    `json:"count"`
}

// AuthorCount is a per-author tally.
type AuthorCount struct {
	UserID string `json:"user_id"`
	Count  int    `json:"count"`
}

// FailedPost links a failed post to its live feed entry.
type FailedPost struct {
	PostID        string `json:"post_id"`
	PlatformID    string `json:"platform_id"`
	Status        string `json:"status"`
	FailureReason string `json:"failure_reason"`
}

// Published is the day's publish tally.
type Published struct {
	Total     int            `json:"total"`
	ByChannel []ChannelCount `json:"by_channel"`
}

// Failed is the day's failure tally plus the per-post detail for feed linking.
type Failed struct {
	Total     int            `json:"total"`
	ByChannel []ChannelCount `json:"by_channel"`
	Posts     []FailedPost   `json:"posts"`
}

// Created is the day's authorship tally.
type Created struct {
	PostsTotal     int           `json:"posts_total"`
	ScheduledTotal int           `json:"scheduled_total"`
	ByAuthor       []AuthorCount `json:"by_author"`
}

// CampaignsCreated is the day's new-campaign tally.
type CampaignsCreated struct {
	Total       int      `json:"total"`
	CampaignIDs []string `json:"campaign_ids"`
}

// Report is the full single-day report (CON-285 §7 GET /report/:date).
type Report struct {
	Date             string           `json:"date"`
	TZ               string           `json:"tz"`
	Published        Published        `json:"published"`
	Failed           Failed           `json:"failed"`
	Created          Created          `json:"created"`
	CampaignsCreated CampaignsCreated `json:"campaigns_created"`
}

// ListItem is one non-empty day's headline totals (CON-285 §7 GET /reports).
type ListItem struct {
	Date                  string `json:"date"`
	PublishedTotal        int    `json:"published_total"`
	FailedTotal           int    `json:"failed_total"`
	CreatedTotal          int    `json:"created_total"`
	CampaignsCreatedTotal int    `json:"campaigns_created_total"`
}

// ReportList is the day-list response.
type ReportList struct {
	Reports     []ListItem `json:"reports"`
	GeneratedAt time.Time  `json:"generated_at"`
}

// computeReport aggregates the inputs falling in window w into a single-day
// report. Pure: same inputs ⇒ same output (CON-285 FR1). Empty buckets serialise
// as [] not null, and orderings are deterministic (platform/author/id ascending)
// so the shape is stable for the client and for tests.
func computeReport(in Inputs, date, tz string, w Window) Report {
	r := Report{Date: date, TZ: tz}

	pubByCh := map[string]int{}
	for _, p := range in.Published {
		if w.contains(p.At) {
			r.Published.Total++
			pubByCh[p.PlatformID]++
		}
	}
	r.Published.ByChannel = channelCounts(pubByCh)

	failByCh := map[string]int{}
	posts := make([]FailedPost, 0)
	for _, f := range in.Failed {
		if w.contains(f.At) {
			r.Failed.Total++
			failByCh[f.PlatformID]++
			posts = append(posts, FailedPost{
				PostID:        f.PostID,
				PlatformID:    f.PlatformID,
				Status:        f.Status,
				FailureReason: f.FailureReason,
			})
		}
	}
	sort.Slice(posts, func(i, j int) bool { return posts[i].PostID < posts[j].PostID })
	r.Failed.ByChannel = channelCounts(failByCh)
	r.Failed.Posts = posts

	byAuthor := map[string]int{}
	for _, c := range in.Created {
		if w.contains(c.At) {
			r.Created.PostsTotal++
			if c.Scheduled {
				r.Created.ScheduledTotal++
			}
			byAuthor[c.CreatedBy]++
		}
	}
	r.Created.ByAuthor = authorCounts(byAuthor)

	ids := make([]string, 0)
	for _, cm := range in.Campaigns {
		if w.contains(cm.At) {
			r.CampaignsCreated.Total++
			ids = append(ids, cm.CampaignID)
		}
	}
	sort.Strings(ids)
	r.CampaignsCreated.Total = len(ids)
	r.CampaignsCreated.CampaignIDs = ids

	return r
}

// bucketReports groups every input row by its local calendar day in loc and
// returns the non-empty days newest-first, at most `limit`. `floor` guards
// against a truncated load: when the Service capped a stream it can only vouch
// for rows at/after `floor`, so any day starting before it may be undercounted
// and is dropped (the client keeps paging with `before`). A zero floor keeps
// everything. Days-with-nothing never appear — they are simply never keyed
// (CON-225 §5, CON-285 FR4).
func bucketReports(in Inputs, loc *time.Location, limit int, floor time.Time) []ListItem {
	type agg struct{ pub, fail, created, camp int }
	days := map[string]*agg{}
	at := func(t time.Time) *agg {
		key := t.In(loc).Format(dateLayout)
		a := days[key]
		if a == nil {
			a = &agg{}
			days[key] = a
		}
		return a
	}
	for _, p := range in.Published {
		at(p.At).pub++
	}
	for _, f := range in.Failed {
		at(f.At).fail++
	}
	for _, c := range in.Created {
		at(c.At).created++
	}
	for _, cm := range in.Campaigns {
		at(cm.At).camp++
	}

	keys := make([]string, 0, len(days))
	for k := range days {
		keys = append(keys, k)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(keys))) // newest day first (lexical == chronological for YYYY-MM-DD)

	out := make([]ListItem, 0, limit)
	for _, k := range keys {
		if len(out) >= limit {
			break
		}
		if !floor.IsZero() {
			// A day is fully covered only if its first instant is at/after floor;
			// a day straddling floor may be missing its older half — skip it.
			dayStart, _ := time.ParseInLocation(dateLayout, k, loc)
			if dayStart.Before(floor) {
				continue
			}
		}
		a := days[k]
		out = append(out, ListItem{
			Date:                  k,
			PublishedTotal:        a.pub,
			FailedTotal:           a.fail,
			CreatedTotal:          a.created,
			CampaignsCreatedTotal: a.camp,
		})
	}
	return out
}

func channelCounts(m map[string]int) []ChannelCount {
	out := make([]ChannelCount, 0, len(m))
	for id, n := range m {
		out = append(out, ChannelCount{PlatformID: id, Count: n})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PlatformID < out[j].PlatformID })
	return out
}

func authorCounts(m map[string]int) []AuthorCount {
	out := make([]AuthorCount, 0, len(m))
	for id, n := range m {
		out = append(out, AuthorCount{UserID: id, Count: n})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UserID < out[j].UserID })
	return out
}
