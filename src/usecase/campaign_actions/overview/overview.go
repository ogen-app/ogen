// Package overview computes a read-only "quick overview" of a campaign
// (CON-113): its brief, phases with per-phase post counts, and content
// distribution by status, platform, and content type. The same service backs
// both the Campaign Assistant's getCampaignOverview tool and the
// GET /api/campaigns/:id/overview REST endpoint.
package overview

import (
	"errors"
	"time"
)

// ErrNotFound is returned by Service.Overview when the campaign does not exist
// (or belongs to another tenant). Handlers map it to 404.
var ErrNotFound = errors.New("campaign not found")

// Overview is the read-only snapshot returned to the assistant tool and the
// REST endpoint.
type Overview struct {
	CampaignID   string       `json:"campaignId"`
	Name         string       `json:"name"`
	Status       string       `json:"status"`
	Type         string       `json:"type"` // campaign type name
	Language     string       `json:"language"`
	Brief        Brief        `json:"brief"`
	Phases       []PhaseInfo  `json:"phases"` // ordered by sequence
	TotalPosts   int          `json:"totalPosts"`
	Distribution Distribution `json:"distribution"`
	// Goal is the CON-182 post-rate goal progress, or null when the campaign has
	// no goal configured (no positive estimated_post_count).
	Goal        *GoalProgress `json:"goal"`
	GeneratedAt time.Time     `json:"generatedAt"`
}

// GoalProgress recaps a campaign's post-rate goal (CON-182): the per-period
// target, how many committed posts land in each period, and whether the goal is
// met per period and overall. "Committed" posts are those scheduled or
// published, bucketed by their scheduled_at.
type GoalProgress struct {
	Cadence        string       `json:"cadence"`        // "week" | "month"
	PostsPerPeriod int          `json:"postsPerPeriod"` // = estimated_post_count
	Periods        int          `json:"periods"`
	TotalTarget    int          `json:"totalTarget"` // postsPerPeriod × periods
	TotalAchieved  int          `json:"totalAchieved"`
	Reached        bool         `json:"reached"`
	Percent        int          `json:"percent"` // 0..100, capped
	Streak         int          `json:"streak"`  // trailing consecutive reached periods
	Buckets        []GoalBucket `json:"buckets"`
}

// GoalBucket is one goal period. Start is inclusive, End exclusive.
type GoalBucket struct {
	Index    int       `json:"index"` // 1-based
	Label    string    `json:"label"` // "Week 1" / "Aug 2026"
	Start    time.Time `json:"start"`
	End      time.Time `json:"end"`
	Target   int       `json:"target"`
	Achieved int       `json:"achieved"`
	Reached  bool      `json:"reached"`
}

// Brief recaps the campaign's brief fields.
type Brief struct {
	Description    string `json:"description"`
	TargetPersona  string `json:"targetPersona"`
	KeyMessages    string `json:"keyMessages"`
	ToneGuidelines string `json:"toneGuidelines"`
}

// PhaseInfo is one campaign phase plus how many posts are assigned to it — the
// content distribution across phases.
type PhaseInfo struct {
	ID        string `json:"id"`
	Sequence  int    `json:"sequence"`
	Name      string `json:"name"`
	Purpose   string `json:"purpose"`
	PostCount int    `json:"postCount"`
	// StartDate/EndDate are the phase's effective window (YYYY-MM-DD, inclusive)
	// from the campaign's phase plan (CON-166); null when the campaign is undated.
	StartDate *string `json:"startDate"`
	EndDate   *string `json:"endDate"`
}

// Distribution holds the flat post breakdowns. Counts reconcile: TotalPosts
// equals the sum of each breakdown, and the sum of phase PostCounts plus
// UnassignedPhasePostCount.
type Distribution struct {
	// UnassignedPhasePostCount counts posts with no phase (or a stale phase id).
	UnassignedPhasePostCount int      `json:"unassignedPhasePostCount"`
	ByStatus                 []Bucket `json:"byStatus"`
	ByPlatform               []Bucket `json:"byPlatform"`
	ByContentType            []Bucket `json:"byContentType"`
}

// Bucket is one entry in a distribution breakdown.
type Bucket struct {
	Key   string `json:"key"`   // status string / platform id / post-type slug
	Label string `json:"label"` // human label (platform name; else the key or "None")
	Count int    `json:"count"`
}
