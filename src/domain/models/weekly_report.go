package models

// WeeklyReportSchemaVersion is bumped whenever WeeklyReportView changes shape,
// so a queued send built by an older release can be told apart.
const WeeklyReportSchemaVersion = 1

// WeeklyReportView is the formatted weekly report the email template renders.
// It rides the send_email job args, so every value is display-ready (numbers,
// arrows, pixel heights, absolute links) and the template only lays it out. A
// nil section is left out of the email.
type WeeklyReportView struct {
	SchemaVersion int    `json:"schema_version"`
	Workspace     string `json:"workspace"`
	RangeLabel    string `json:"range_label"`
	WeekOf        string `json:"week_of"`
	Timezone      string `json:"timezone"`
	Headline      string `json:"headline"`
	Lede          string `json:"lede"`
	AppURL        string `json:"app_url"`

	Attention   []WeeklyAttention  `json:"attention,omitempty"`
	Publishing  *WeeklyPublishing  `json:"publishing,omitempty"`
	Performance *WeeklyPerformance `json:"performance,omitempty"`
	Campaigns   *WeeklyCampaigns   `json:"campaigns,omitempty"`
	Ideas       *WeeklyIdeas       `json:"ideas,omitempty"`
	Team        *WeeklyTeam        `json:"team,omitempty"`
}

// WeeklyAttention is one "Needs your attention" row.
type WeeklyAttention struct {
	Text      string `json:"text"`
	Detail    string `json:"detail,omitempty"`
	LinkLabel string `json:"link_label"`
	URL       string `json:"url"`
}

// WeeklyFigure is a headline number with its change against last week. Good
// marks an improvement (rendered teal); a decline is a plain fact.
type WeeklyFigure struct {
	Value string `json:"value"`
	Delta string `json:"delta,omitempty"`
	Good  bool   `json:"good,omitempty"`
}

// WeeklyBar is one day's bar in a table-cell chart. Height is in pixels; a
// zero day draws no bar.
type WeeklyBar struct {
	Day    string `json:"day"`
	Value  string `json:"value"`
	Height int    `json:"height"`
}

// WeeklyPublishing is the Publishing section.
type WeeklyPublishing struct {
	Published    WeeklyFigure    `json:"published"`
	Failed       string          `json:"failed,omitempty"`
	FailedNote   string          `json:"failed_note,omitempty"`
	ChartHeight  int             `json:"chart_height"`
	Bars         []WeeklyBar     `json:"bars"`
	Channels     []WeeklyChannel `json:"channels,omitempty"`
	MoreChannels string          `json:"more_channels,omitempty"`
	Failures     []WeeklyFailure `json:"failures,omitempty"`
	MoreFailures string          `json:"more_failures,omitempty"`
	FailuresURL  string          `json:"failures_url,omitempty"`
	NextWeek     string          `json:"next_week,omitempty"`
	CalendarURL  string          `json:"calendar_url"`
}

// WeeklyChannel is one row of the By channel table.
type WeeklyChannel struct {
	Platform  string `json:"platform"`
	Account   string `json:"account,omitempty"`
	Published string `json:"published"`
	Failed    string `json:"failed"`
	HasFailed bool   `json:"has_failed,omitempty"`
	Reach     string `json:"reach"`
}

// WeeklyFailure is one "What didn't go out" row.
type WeeklyFailure struct {
	Title  string `json:"title"`
	Meta   string `json:"meta"`
	Reason string `json:"reason"`
	URL    string `json:"url"`
}

// WeeklyPerformance is the Performance section.
type WeeklyPerformance struct {
	Reach          WeeklyFigure `json:"reach"`
	Interactions   WeeklyFigure `json:"interactions"`
	Engagement     WeeklyFigure `json:"engagement"`
	Followers      WeeklyFigure `json:"followers"`
	FollowerHeight int          `json:"follower_height,omitempty"`
	FollowerBars   []WeeklyBar  `json:"follower_bars,omitempty"`
	Top            []WeeklyPost `json:"top,omitempty"`
	Below          []WeeklyPost `json:"below,omitempty"`
	URL            string       `json:"url"`
}

// WeeklyPost is one top or below-typical post row.
type WeeklyPost struct {
	Rank     string `json:"rank,omitempty"`
	Title    string `json:"title"`
	Meta     string `json:"meta"`
	Reach    string `json:"reach"`
	Multiple string `json:"multiple"`
	URL      string `json:"url"`
}

// WeeklyCampaigns is the Campaigns section.
type WeeklyCampaigns struct {
	Rows    []WeeklyCampaign `json:"rows,omitempty"`
	Summary string           `json:"summary,omitempty"`
	URL     string           `json:"url"`
}

// WeeklyCampaign is one campaign's goal progress. Fill and Rest are the
// progress bar's two cell widths in pixels.
type WeeklyCampaign struct {
	Name   string `json:"name"`
	Goal   string `json:"goal"`
	Status string `json:"status"`
	Met    bool   `json:"met,omitempty"`
	Fill   int    `json:"fill"`
	Rest   int    `json:"rest"`
	URL    string `json:"url"`
}

// WeeklyIdeas is the Ideas and content bank section.
type WeeklyIdeas struct {
	Waiting string `json:"waiting,omitempty"`
	Summary string `json:"summary,omitempty"`
	URL     string `json:"url"`
}

// WeeklyTeam is the Team section.
type WeeklyTeam struct {
	Summary string `json:"summary"`
}
