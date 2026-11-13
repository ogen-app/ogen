package models

import (
	"strings"
	"time"

	"github.com/uptrace/bun"
)

// SeriesSupply says where a series gets the subject of each post.
type SeriesSupply string

const (
	// SeriesSupplySelf supplies its own subject (a date, the week's news).
	SeriesSupplySelf SeriesSupply = "self"
	// SeriesSupplyIdea needs a subject from the Ideas queue before it can run.
	SeriesSupplyIdea SeriesSupply = "idea"
)

func (s SeriesSupply) Valid() bool {
	return s == SeriesSupplySelf || s == SeriesSupplyIdea
}

// MaxRhythmTimes is the most times a series can run in one period: a daily
// series in a 31-day month.
const MaxRhythmTimes = 31

// MaxRhythmEvery is the longest interval a rhythm can skip between active
// periods: a year in months, or twelve weeks.
const MaxRhythmEvery = 12

// rhythmWeekdays are the days a weekly rhythm can name, by their JSON form.
var rhythmWeekdays = map[string]time.Weekday{
	"monday": time.Monday, "tuesday": time.Tuesday, "wednesday": time.Wednesday, "thursday": time.Thursday,
	"friday": time.Friday, "saturday": time.Saturday, "sunday": time.Sunday,
}

// SeriesRhythm is how often a series runs: Times per Per, where Per is a goal
// cadence (week|month), in one of every Every periods (absent is 1), on
// Weekday when one is named. A nil *SeriesRhythm means occasional: the series
// runs when there is something for it and claims no plan slots. Zero times is
// not a way to say that.
//
// "Every Friday" is {1, week, weekday: friday}; "every 2 weeks on Tuesday" is
// {1, week, every: 2, weekday: tuesday}. A stored {times, per} keeps its
// meaning: every period, on no set day.
type SeriesRhythm struct {
	Times int    `json:"times"`
	Per   string `json:"per" enums:"week,month"`
	// Every is the interval in Per between periods it runs in; absent is 1.
	Every int `json:"every,omitzero" minimum:"1" maximum:"12"`
	// Weekday is the day a once-a-week rhythm runs on; absent is any day.
	Weekday string `json:"weekday,omitempty" enums:"monday,tuesday,wednesday,thursday,friday,saturday,sunday"`
}

// Valid reports whether the rhythm is a complete, in-range rhythm. A weekday
// needs a once-a-week rhythm: it says which day the one post goes on.
func (r SeriesRhythm) Valid() bool {
	if r.Times < 1 || r.Times > MaxRhythmTimes || (r.Per != "week" && r.Per != "month") {
		return false
	}
	if r.Every < 0 || r.Every > MaxRhythmEvery {
		return false
	}
	if r.Weekday == "" {
		return true
	}
	_, ok := rhythmWeekdays[r.Weekday]
	return ok && r.Per == "week" && r.Times == 1
}

// Normalized is r in its stored form: per and weekday trimmed and lower-cased,
// and an Every of 1 left out, so the same rhythm always compares equal.
func (r SeriesRhythm) Normalized() SeriesRhythm {
	r.Per = strings.ToLower(strings.TrimSpace(r.Per))
	r.Weekday = strings.ToLower(strings.TrimSpace(r.Weekday))
	if r.Every == 1 {
		r.Every = 0
	}
	return r
}

// Interval is how many periods apart its runs are: Every, or 1 when absent.
func (r SeriesRhythm) Interval() int {
	return max(r.Every, 1)
}

// Day is the weekday it runs on, and false when it names none.
func (r SeriesRhythm) Day() (time.Weekday, bool) {
	d, ok := rhythmWeekdays[r.Weekday]
	return d, ok
}

// SeriesUsage is what a series has produced, counted from posts.series_id.
// Derived on read, never stored.
type SeriesUsage struct {
	Drafts    int `json:"drafts"`
	Published int `json:"published"`
}

// Series is a standing instruction for a recurring kind of post: a name and
// how one is built. CampaignID nil puts it in the workspace library; set, it
// was defined inside that campaign and ends with it.
type Series struct {
	bun.BaseModel `bun:"table:brand_series,alias:bs" swaggerignore:"true"`
	TenantScoped

	ID            string         `bun:"id,pk"                                        json:"id"`
	Name          string         `bun:"name,notnull"                                 json:"name"`
	Promise       string         `bun:"promise,notnull"                              json:"promise"`
	Recipe        string         `bun:"recipe,notnull"                               json:"recipe"`
	Supply        SeriesSupply   `bun:"supply,notnull"                               json:"supply"         enums:"self,idea"`
	ContentFormat *ContentFormat `bun:"content_format"                               json:"content_format" swaggertype:"string" extensions:"x-nullable"`
	DefaultRhythm *SeriesRhythm  `bun:"default_rhythm,type:jsonb"                    json:"default_rhythm" extensions:"x-nullable"`
	CampaignID    *string        `bun:"campaign_id"                                  json:"campaign_id"    extensions:"x-nullable"`
	Usage         SeriesUsage    `bun:"-"                                            json:"usage"`
	CreatedAt     time.Time      `bun:"created_at,notnull,default:current_timestamp" json:"created_at"`
	UpdatedAt     time.Time      `bun:"updated_at,notnull,default:current_timestamp" json:"updated_at"`
	DeletedAt     *time.Time     `bun:"deleted_at"                                   json:"-"`
}

// CampaignSeriesRun is one campaign's run of one series. Rhythm nil is
// occasional.
type CampaignSeriesRun struct {
	bun.BaseModel `bun:"table:campaign_series,alias:cs" swaggerignore:"true"`
	TenantScoped

	CampaignID string        `bun:"campaign_id,pk"                               json:"-"`
	SeriesID   string        `bun:"series_id,pk"                                 json:"series_id"`
	Rhythm     *SeriesRhythm `bun:"rhythm,type:jsonb"                            json:"rhythm" extensions:"x-nullable"`
	CreatedAt  time.Time     `bun:"created_at,notnull,default:current_timestamp" json:"-"`
}

// CampaignSeries is every series one campaign runs, oldest attach first. Each
// campaign series write answers with the whole thing.
type CampaignSeries struct {
	CampaignID string              `json:"campaign_id"`
	Runs       []CampaignSeriesRun `json:"runs"`
}
