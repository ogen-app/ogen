package post_quality

import (
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
)

// PostQualityRequest is the input to the assessPostQuality flow.
type PostQualityRequest struct {
	PostID string `json:"postId"`
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

// ValidationError is returned when input preconditions are not met
// (missing post, empty body, no platform/type). Handlers map it to 400.
type ValidationError struct{ Msg string }

func (e *ValidationError) Error() string { return e.Msg }

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
// genkit.GenerateData. It is deliberately distinct from
// models.EvaluationResult: the model returns only the critique —
// rationale, weakness, score, and suggestions — while the backend adds
// the weights, weighted contributions, and overall percentage afterward
// (the model never computes the overall, per the CON-85 weighting
// contract). It also does not echo platform/type: the backend already
// knows those from the Post and stamps them, so the model can't get them
// wrong.
//
// Field order is load-bearing. Within every dimension the Rationale field
// is declared BEFORE Score because the model emits fields in schema
// order, so it must reason before committing to a number ("reason before
// scoring"). The fieldOrder test guards this — do not reorder.
type assessmentOutput struct {
	Correctness dimensionOutput `json:"correctness" jsonschema:"description=Is it true and well-formed: factual accuracy of checkable claims, internal consistency, consistency with the supplied campaign brief and brand facts, and mechanical correctness (grammar/spelling/punctuation). Scope is consistency with supplied context, not open-world fact-checking."`
	Clarity     dimensionOutput `json:"clarity" jsonschema:"description=Is it understood on one pass: one clear takeaway, coherent logical flow, sentence complexity and jargon appropriate to the audience. NOT about how engaging it is."`
	Engagement  dimensionOutput `json:"engagement" jsonschema:"description=Does it make people care and act, for THIS platform: hook strength (does line one earn line two), emotional/curiosity pull, audience relevance, call-to-action quality, shareability, opening-line weight under feed truncation."`
	Delivery    dimensionOutput `json:"delivery" jsonschema:"description=Does it fit the channel as a finished artifact, for THIS platform: length vs platform norms, formatting (line breaks, spacing, emoji density, hashtag count/placement, mentions/links), tone consistency with brand, and absence of artifacts (truncated links, CTA with no link, hashtag soup)."`
}

// dimensionOutput is one dimension's critique. Rationale precedes Score by
// design (see assessmentOutput). Weakness is mandatory — the model must
// name at least one concrete weakness even on strong posts, to counter
// sycophantic score inflation.
type dimensionOutput struct {
	Rationale   string             `json:"rationale" jsonschema:"description=Your reasoning for THIS dimension against the anchored bands, written BEFORE the score. Address the author directly in fluent, complete sentences (2-4 of them) that connect into a short argument - an articulate editor's note, not clipped rubric shorthand. Concrete and grounded in the actual post; never empty. Required for every dimension - correctness, clarity, engagement, and delivery."`
	Weakness    string             `json:"weakness" jsonschema:"description=At least one concrete, specific weakness for this dimension, phrased as a clear sentence addressed to the author. Required even when the post is strong - never leave this empty."`
	Score       int                `json:"score" jsonschema:"description=Integer from 0 to 10 inclusive, chosen against the anchored bands in the rubric. Do not cluster scores in 7-9."`
	Suggestions []suggestionOutput `json:"suggestions" jsonschema:"description=Concrete improvement suggestions for this dimension, ordered most-severe first. Guidance only, never a rewrite. Each must quote a span of the post. Emit at most the suggestion cap stated in the prompt."`
}

// suggestionOutput is one span-anchored, guidance-only suggestion. The
// dimension is implied by the parent block, so the model does not repeat
// it; the backend fills models.EvaluationSuggestion.Dimension when it maps
// the output. Span is required and must quote actual post text — the guard
// against generic advice.
type suggestionOutput struct {
	Severity string `json:"severity" jsonschema:"description=Exactly one of: high, medium, low."`
	Issue    string `json:"issue" jsonschema:"description=A clear, self-contained sentence naming what is wrong with the quoted span."`
	Fix      string `json:"fix" jsonschema:"description=An articulate directive the author can act on, addressed to them in the second person. Guidance, not rewritten copy."`
	Span     string `json:"span" jsonschema:"description=The exact text quoted from the post that this suggestion reacts to. Required - never empty."`
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

	SuggestionCap int
}
