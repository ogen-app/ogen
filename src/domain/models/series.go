package models

import (
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

// SeriesRhythm is how often a series runs: Times per Per, where Per is a goal
// cadence (week|month). A nil *SeriesRhythm means occasional: the series runs
// when there is something for it and claims no plan slots. Zero times is not a
// way to say that.
type SeriesRhythm struct {
	Times int    `json:"times"`
	Per   string `json:"per" enums:"week,month"`
}

// Valid reports whether the rhythm is a complete, in-range {times, per}.
func (r SeriesRhythm) Valid() bool {
	return r.Times >= 1 && r.Times <= MaxRhythmTimes && (r.Per == "week" || r.Per == "month")
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
