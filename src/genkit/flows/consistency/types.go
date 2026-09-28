// Package consistency implements the read-only Campaign Assistant consistency
// review: checkBrief critiques the campaign brief's internal
// consistency, and checkPosts checks whether the campaign's non-published posts
// follow the brief. Both return structured findings and never mutate anything.
package consistency

import (
	"time"

	"github.com/ogen-app/ogen/src/infra/eventhub"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/infra/vendors/llm"
	"github.com/ogen-app/ogen/src/kernel/usage"
)

// PostsCheckRequest is the input to the posts consistency check.
type PostsCheckRequest struct {
	CampaignID string `json:"campaignId"`
	Max        int    `json:"max"` // 0 → config default
}

// Finding is one brief-consistency issue with concrete guidance (not a rewrite).
type Finding struct {
	Aspect     string `json:"aspect"`     // goal_alignment | persona | messages | tone | completeness
	Severity   string `json:"severity"`   // high | medium | low
	Issue      string `json:"issue"`      // one-line problem
	Suggestion string `json:"suggestion"` // concrete guidance
}

// BriefReview is the read-only result of a brief consistency check.
type BriefReview struct {
	CampaignID  string    `json:"campaignId"`
	Consistent  bool      `json:"consistent"` // no high-severity findings
	Findings    []Finding `json:"findings"`
	Summary     string    `json:"summary"`
	GeneratedAt time.Time `json:"generatedAt"`
}

// PostFinding flags one post that drifts from the brief.
type PostFinding struct {
	PostID     string `json:"postId"`
	Title      string `json:"title"`
	Severity   string `json:"severity"`
	Issue      string `json:"issue"`
	Suggestion string `json:"suggestion"`
}

// PostsReview is the read-only result of a posts-vs-brief consistency check.
type PostsReview struct {
	CampaignID  string        `json:"campaignId"`
	Checked     int           `json:"checked"` // posts analyzed
	Total       int           `json:"total"`   // eligible posts
	Capped      bool          `json:"capped"`  // Total > Checked
	Aligned     int           `json:"aligned"` // checked posts with no finding
	Findings    []PostFinding `json:"findings"`
	Summary     string        `json:"summary"`
	GeneratedAt time.Time     `json:"generatedAt"`
}

// ── Model output schemas (genkit.GenerateData builds the JSON schema from these) ──

type findingOutput struct {
	Aspect     string `json:"aspect"     jsonschema:"description=which aspect the issue is about,enum=goal_alignment,enum=persona,enum=messages,enum=tone,enum=completeness"`
	Severity   string `json:"severity"   jsonschema:"description=how serious the issue is,enum=high,enum=medium,enum=low"`
	Issue      string `json:"issue"      jsonschema:"description=one concise sentence describing the inconsistency or gap"`
	Suggestion string `json:"suggestion" jsonschema:"description=concrete guidance to fix it; do NOT rewrite the brief"`
}

type briefReviewOutput struct {
	Consistent bool            `json:"consistent" jsonschema:"description=true only when there are no high-severity findings"`
	Summary    string          `json:"summary"    jsonschema:"description=2-3 sentence overall assessment of the brief's consistency"`
	Findings   []findingOutput `json:"findings"   jsonschema:"description=specific issues; return an empty array when the brief is coherent and complete"`
}

type postFindingOutput struct {
	PostID     string `json:"postId"     jsonschema:"description=the exact id of the drifting post, taken from the provided list"`
	Severity   string `json:"severity"   jsonschema:"description=how serious the drift is,enum=high,enum=medium,enum=low"`
	Issue      string `json:"issue"      jsonschema:"description=how this post drifts from the brief's persona, key messages, or tone"`
	Suggestion string `json:"suggestion" jsonschema:"description=concrete guidance to bring the post in line with the brief"`
}

type postsReviewOutput struct {
	Summary  string              `json:"summary"  jsonschema:"description=2-3 sentence overall assessment of how well the posts follow the brief"`
	Findings []postFindingOutput `json:"findings" jsonschema:"description=one entry per post that drifts; omit posts that are aligned"`
}

// ── Prompt template data ─────────────────────────────────────────────────────

// briefTemplateData is the data passed to the brief-review user template.
type briefTemplateData struct {
	CampaignName   string
	CampaignType   string
	CampaignGoal   string
	Phases         []phaseTemplateData
	Language       string
	Description    string
	TargetPersona  string
	KeyMessages    string
	ToneGuidelines string
}

type phaseTemplateData struct {
	Sequence int
	Name     string
	Purpose  string
}

// postsTemplateData is the data passed to the posts-review user template.
type postsTemplateData struct {
	TargetPersona  string
	KeyMessages    string
	ToneGuidelines string
	Language       string
	Posts          []postTemplateData
}

type postTemplateData struct {
	ID       string
	Title    string
	PostType string
	Body     string
}

// ── Flow config + repos ────────────────────────────────────────────────────────

// ConsistencyFlowConfig holds static settings for the flow.
type ConsistencyFlowConfig struct {
	Provider        *llm.Provider
	Recorder        *usage.Recorder
	Checker         *usage.Checker
	ModelID         string
	MaxOutputTokens int64 // 0 → 8192
	// MaxPosts caps how many posts a posts-review analyzes in one call.
	// 0 → 20.
	MaxPosts int
	Hub      eventhub.Hub

	tmpl *templates
}

// ConsistencyRepos bundles the repository dependencies.
type ConsistencyRepos struct {
	Campaigns repository.CampaignRepository
	Posts     repository.PostRepository
}

// ValidationError → HTTP 400. AIError → HTTP 502.
type ValidationError struct{ Msg string }

func (e *ValidationError) Error() string { return e.Msg }

type AIError struct{ Msg string }

func (e *AIError) Error() string { return e.Msg }

// ── SSE ────────────────────────────────────────────────────────────────────────

type SSEEventKind string

const (
	SSEEventStep     SSEEventKind = "step"
	SSEEventComplete SSEEventKind = "complete"
	SSEEventError    SSEEventKind = "error"
)

// StepEventPayload marks a flow stage as finished.
type StepEventPayload struct {
	Step   string `json:"step"`
	Status string `json:"status"` // always "done"
}

// ErrorEventPayload is emitted when the flow fails.
type ErrorEventPayload struct {
	Message string `json:"message"`
	Code    int    `json:"code"`
}

// OnEventFunc is an optional callback invoked as SSE events are produced.
type OnEventFunc func(name SSEEventKind, data any)

func severityHigh(s string) bool { return s == "high" }
