package post_quality

import (
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
)

// PostQualityRequest is the input to the assessPostQuality flow.
type PostQualityRequest struct {
	PostID string `json:"postId"`
	// Trigger is what started the run; empty means manual.
	Trigger models.QualityTrigger `json:"trigger,omitempty"`
}

// PostQualityResponse is returned by the flow and the handler. Evaluation
// is the persisted result, with the backend-computed overall percentage
// and per-dimension contributions already attached. Cached is true when the
// inputs were unchanged since the last run and the stored evaluation was
// returned without calling the model.
type PostQualityResponse struct {
	PostID      string                 `json:"postId"`
	GeneratedAt time.Time              `json:"generatedAt"`
	Evaluation  *models.PostEvaluation `json:"evaluation"`
	Cached      bool                   `json:"cached"`
}

// Status is a post's stored assessment as the read endpoint reports it.
// Stale is true when the post's content or its context changed since the
// assessment ran; StaleReason says which. RunsLimit is the
// quality_reviews_per_post entitlement, nil when unlimited.
type Status struct {
	*models.PostEvaluation
	Stale       bool                      `json:"stale"`
	StaleReason models.QualityStaleReason `json:"stale_reason,omitempty"`
	RunsLimit   *int                      `json:"runs_limit"`
}

// ValidationError is returned when input preconditions are not met
// (missing post, empty body, no platform/type). Handlers map it to 400.
type ValidationError struct{ Msg string }

func (e *ValidationError) Error() string { return e.Msg }

// SkippedError is returned by an automatic run that did not call the model
// because the post's run allowance or the tenant's spend cap blocked it. The
// stored assessment is left as it is (and reads as stale); nothing is
// reported to the author.
type SkippedError struct{ Reason string }

func (e *SkippedError) Error() string { return "assessment skipped: " + e.Reason }

// AIError is returned when the model call or output validation fails after
// the retry. Handlers map it to 502.
type AIError struct{ Msg string }

func (e *AIError) Error() string { return e.Msg }

// SSEEventKind identifies the SSE event types emitted by the flow.
type SSEEventKind string

const (
	SSEEventStep     SSEEventKind = "step"
	SSEEventComplete SSEEventKind = "complete"
	SSEEventError    SSEEventKind = "error"
)

// StepEventPayload is the data payload for a "step" SSE event.
type StepEventPayload struct {
	Step   string `json:"step"`
	Status string `json:"status"` // always "done"
}

// ErrorEventPayload is the data payload for an "error" SSE event.
type ErrorEventPayload struct {
	Message string `json:"message"`
	Code    int    `json:"code"` // HTTP semantic: 400, 502, 500
}

// OnEventFunc is an optional callback invoked after each flow step
// completes. A nil OnEventFunc is valid — the flow then runs silently.
type OnEventFunc func(name SSEEventKind, data any)

// assessmentOutput is the typed structured output the model fills via
// genkit.GenerateData, one dimension per call. It is deliberately distinct
// from models.EvaluationResult: the model returns only the critique (verdict,
// score, suggestions) while the backend adds the weights, weighted
// contributions, and overall percentage afterward; the model never computes
// the overall. It also does not echo platform/type: the backend already knows
// those from the Post and stamps them, so the model can't get them wrong.
//
// Field order is load-bearing. Within every dimension Verdict is declared
// BEFORE Score because the model emits fields in schema order, so it must
// reason before committing to a number. TestVerdictPrecedesScore guards
// this; do not reorder.
type assessmentOutput struct {
	Correctness dimensionOutput `json:"correctness"`
	Clarity     dimensionOutput `json:"clarity"`
	Engagement  dimensionOutput `json:"engagement"`
	Delivery    dimensionOutput `json:"delivery"`
}

// dimensionOutput is one dimension's critique. Suggestions must hold at least
// one entry even on a strong post, the counter to sycophantic score
// inflation.
type dimensionOutput struct {
	Verdict     string             `json:"verdict" jsonschema:"description=One or two plain sentences to the author on what works and what does not for THIS dimension, written BEFORE the score. At most 240 characters. Do not restate the score."`
	Score       int                `json:"score" jsonschema:"description=Integer from 0 to 10 inclusive, chosen against the anchored bands in the rubric. Do not cluster scores in 7-9."`
	Suggestions []suggestionOutput `json:"suggestions" jsonschema:"description=One or two fixes for this dimension, most severe first. At least one, even on a strong post."`
}

// suggestionOutput is one span-anchored, guidance-only suggestion. The
// dimension is implied by the parent block, so the model does not repeat it;
// the backend fills models.EvaluationSuggestion.Dimension when it maps the
// output.
type suggestionOutput struct {
	Severity string `json:"severity" jsonschema:"description=Exactly one of: high, medium, low."`
	Span     string `json:"span" jsonschema:"description=The exact text quoted from the post that this suggestion reacts to. Required - never empty."`
	Advice   string `json:"advice" jsonschema:"description=ONE sentence to the author: what is wrong with the span and what to do about it. Guidance, not rewritten copy. At most 200 characters. Do not repeat the span."`
}

// assetContext is one brand/reference asset summary shown to the model in
// the user prompt (title + short preview). Populated from the Post's used
// assets in the flow's buildContext step.
type assetContext struct {
	Title   string
	Preview string
}

// assessmentTemplateData is the data passed to the user-prompt template.
// Everything here is per-call/variable; the platform-agnostic rubric lives
// in the static system block so it can form a stable (cacheable) prefix.
//
// CaptionScoped is true when the Post carried media the model cannot see;
// the prompt then states that the score is caption/text-scoped and the
// visual is not judged.
type assessmentTemplateData struct {
	Platform            string
	PostType            string
	CaptionScoped       bool
	PlatformConventions string

	CampaignName        string
	CampaignDescription string
	TargetPersona       string
	KeyMessages         string
	ToneGuidelines      string
	Language            string

	PhaseName        string
	PhaseDescription string

	Assets []assetContext

	PostTitle string
	PostBody  string
}
