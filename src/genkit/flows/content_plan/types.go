package content_plan

import "time"

// ContentPlanRequest is the input to the generateContentPlan flow.
type ContentPlanRequest struct {
	CampaignID string `json:"campaignId"`
}

// GeneratePostsRequest is the input to the targeted generation entry point
// (CON-114): generate exactly Count draft posts for a platform subset, in a
// single phase, with publish dates within [WindowStart, WindowEnd]. All fields
// are concrete — the assistant tool resolves any natural language before
// calling, so the engine stays deterministic.
type GeneratePostsRequest struct {
	CampaignID  string   `json:"campaignId"`
	PlatformIDs []string `json:"platformIds"` // subset of the campaign's target platforms
	PhaseID     string   `json:"phaseId"`     // a single campaign phase id
	Count       int      `json:"count"`       // number of posts to generate
	WindowStart string   `json:"windowStart"` // ISO YYYY-MM-DD (inclusive)
	WindowEnd   string   `json:"windowEnd"`   // ISO YYYY-MM-DD (inclusive)
	PostType    string   `json:"postType"`    // optional post-type slug; empty = platform default(s)
}

// DraftPost is both the AI output schema (via GenerateData[[]DraftPost]) and
// the shape persisted as a Post record with status=draft.
type DraftPost struct {
	Title       string   `json:"title"                   jsonschema:"description=Short descriptive title for the post"`
	Body        string   `json:"body"                    jsonschema:"description=A concise bullet-point thesis: 5-7 short key-point lines to expand later, NOT finished copy (max 500 chars). Stored as a draft_thesis note, not the post body."`
	ContentType string   `json:"contentType"             jsonschema:"description=Content format slug e.g. text-post article carousel thread video reel"`
	PlatformID  string   `json:"platformId"              jsonschema:"description=Exact platform ID string from the campaign e.g. linkedin x-twitter instagram"`
	PublishDate string   `json:"publishDate"             jsonschema:"description=ISO 8601 date YYYY-MM-DD within the campaign date range"`
	ToneNotes   string   `json:"toneNotes"               jsonschema:"description=How the campaign tone guidelines apply to this specific post"`
	PhaseID     string   `json:"phaseId"                 jsonschema:"description=Exact phase ID from the campaign phases list; every post must be assigned to one phase"`
	AssetRefs   []string `json:"assetRefs,omitempty"     jsonschema:"description=IDs of assets whose facts or ideas were directly used in this post; omit if none"`
	// BrandVoiceID is the resolved brand voice for this post (CON-245), set
	// server-side after generation. json:"-" keeps it out of the model schema.
	BrandVoiceID *string `json:"-"`
}

// ContentPlanResponse is returned by the handler and the flow.
type ContentPlanResponse struct {
	CampaignID  string      `json:"campaignId"`
	GeneratedAt time.Time   `json:"generatedAt"`
	Posts       []DraftPost `json:"posts"`
	Warnings    []string    `json:"warnings,omitempty"`
	// UsedAssets lists the campaign assets retrieved into the generation context
	// and offered to the model for this plan (CON-118). Each post records only
	// the subset it actually drew on (Post.UsedAssetIDs), so this plan-level list
	// is a superset of any single post's binding. Empty when UseAssets is off or
	// nothing was retrieved.
	UsedAssets []AssetRef `json:"usedAssets,omitempty"`
}

// AssetRef is the id+title provenance of an asset that informed generation
// (CON-118).
type AssetRef struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

// resolvedPiece is an internal type used to build the prompt context.
type resolvedPiece struct {
	ID      string
	Title   string
	Excerpt string
}

// resolvedPlatform is an internal type used to build the prompt context.
type resolvedPlatform struct {
	ID           string
	Name         string
	PostTypes    string   // comma-separated slugs shown in the prompt
	AllowedSlugs []string // non-empty = hard-enforce these slugs in validateOutput; nil = allow all
	Cadence      string   // e.g. "1–2 posts per week"
	Constraints  string   // character limits, format notes
}

// resolvedPhase is an internal type used to build the prompt context.
type resolvedPhase struct {
	ID       string
	Name     string
	Purpose  string
	Sequence int
	// Window pins the phase's date window from the campaign's manual phase plan
	// (CON-166); nil = derive it from the campaign dates.
	Window *dateWindow
}

// contentPlanTemplateData is the data passed to the user-prompt template.
//
// Batch is non-nil when the user template is rendered for a specific batch
// (the production path under parallel batching). The template uses Batch to
// emit the per-batch slot table — required count, phase mix, platform mix,
// date window — so the model produces exactly the slice we asked for. When
// Batch is nil, the template falls back to the global EstimatedPostCount
// rendering for a single-shot run.
type contentPlanTemplateData struct {
	Name                    string
	Description             string
	CampaignTypeLabel       string
	CampaignTypeDescription string
	Phases                  []resolvedPhase
	TargetPersona           string
	KeyMessages             string
	ToneGuidelines          string
	// BrandBlock is the resolved brand voice/audience/guardrails prompt block
	// (CON-245); supersedes TargetPersona/ToneGuidelines in the template.
	BrandBlock         string
	Language           string
	StartDate          string
	EndDate            string
	DayCount           int
	EstimatedPostCount int
	Platforms          []resolvedPlatform
	Assets             []resolvedPiece
	Batch              *batchSpec
	// PublishingDays is the comma-separated label list of enabled weekdays
	// (e.g. "Mon, Wed, Fri"), or "" when every day is enabled (CON-181). The
	// server snaps any stray date to an enabled day regardless; this just
	// steers the model up front.
	PublishingDays string
}

// ValidationError is returned by the flow when preconditions are not met.
// Handlers map this to HTTP 400.
type ValidationError struct{ Msg string }

func (e *ValidationError) Error() string { return e.Msg }

// AIError is returned when the Anthropic API call fails.
// Handlers map this to HTTP 502.
type AIError struct{ Msg string }

func (e *AIError) Error() string { return e.Msg }

// ── SSE types ─────────────────────────────────────────────────────────────────

// SSEEventKind identifies the three SSE event types emitted by GenerateDraft.
type SSEEventKind string

const (
	SSEEventStep     SSEEventKind = "step"
	SSEEventPost     SSEEventKind = "post"
	SSEEventWarning  SSEEventKind = "warning"
	SSEEventComplete SSEEventKind = "complete"
	SSEEventError    SSEEventKind = "error"
)

// StepEventPayload is the data payload for a "step" SSE event.
type StepEventPayload struct {
	Step   string `json:"step"`
	Status string `json:"status"` // always "done"
}

// PostEventPayload is the data payload for a "post" SSE event.
// Emitted incrementally during model generation for each draft post
// as it is parsed from the streaming model response.
//
// Index is the post's deterministic global slot index assigned by the batch
// planner — stable across runs and independent of arrival order. Under
// parallel batching, posts arrive interleaved by completion time, so the
// stream-arrival order is no longer a reliable identifier; the UI should
// place posts by Index, not by the order they appear on the wire.
//
// ID is the persisted Post row's primary key. Per CON-66 every post is
// inserted before its event fires, so the client can reference the row
// (edit, delete, drag) immediately rather than waiting for the
// "complete" event.
type PostEventPayload struct {
	Post  DraftPost `json:"post"`
	Index int       `json:"index"`
	ID    string    `json:"id"`
}

// WarningPayload is the data payload for a "warning" SSE event.
// Emitted when a parsed post is dropped (validation failure) or
// rejected (persist failure). Index points at the global slot the
// post would have occupied; clients can use it to remove a previously
// emitted preview if the post was streamed before being rejected.
type WarningPayload struct {
	Message string `json:"message"`
	Index   int    `json:"index,omitzero"`
}

// ErrorEventPayload is the data payload for an "error" SSE event.
type ErrorEventPayload struct {
	Message string `json:"message"`
	Code    int    `json:"code"` // HTTP semantic: 400, 502, 500
}

// OnEventFunc is an optional callback invoked after each flow step completes.
// name is one of the SSEEventKind constants; data is JSON-serialisable.
// A nil OnEventFunc is valid — the flow runs silently.
type OnEventFunc func(name SSEEventKind, data any)
