package draft_post

import (
	"text/template"

	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/infra/vendors/llm"
	"github.com/ogen-app/ogen/src/kernel/usage"
)

// DraftPostRequest is the input to the draftPost generation flow:
// rewrite SourceMaterial into Count finished, platform-ready post drafts for a
// single platform, phase, and publish window. All fields are concrete — the
// campaign_assistant draftPost tool resolves any natural language (platform,
// phase, count, date) before calling, so the flow stays deterministic.
type DraftPostRequest struct {
	CampaignID     string   `json:"campaignId"`
	PlatformID     string   `json:"platformId"`
	PostType       string   `json:"postType"`       // optional post-type slug; empty = platform/campaign default
	Count          int      `json:"count"`          // number of drafts to produce (>=1)
	SourceMaterial string   `json:"sourceMaterial"` // research/source text to rewrite into posts
	Instruction    string   `json:"instruction"`    // optional user steering ("punchier", "for execs")
	WindowStart    string   `json:"windowStart"`    // ISO YYYY-MM-DD (inclusive)
	WindowEnd      string   `json:"windowEnd"`      // ISO YYYY-MM-DD (inclusive)
	PhaseID        string   `json:"phaseId"`        // a single campaign phase id
	UsedAssetIDs   []string `json:"usedAssetIds"`   // asset ids stamped on each created post for provenance (may be empty)
}

// DraftedPost is one finished post draft produced + persisted by the flow.
type DraftedPost struct {
	Title       string `json:"title"`
	Content     string `json:"content"`
	PlatformID  string `json:"platformId"`
	PostType    string `json:"postType"`
	PublishDate string `json:"publishDate"`
	PostID      string `json:"postId"`
}

// DraftPostResponse is returned by the flow. Posts is the persisted set — every
// element already has a real Post row. An empty Posts with warnings is
// a soft failure (the model produced nothing usable), not an error.
type DraftPostResponse struct {
	Posts    []DraftedPost `json:"posts"`
	Warnings []string      `json:"warnings,omitempty"`
}

// modelDraft is the per-post schema the model emits: just a title and the
// finished copy. The platform, post type, and publish date are decided
// server-side (already resolved by the tool), so the model focuses only on
// writing native, publish-ready copy from the research.
type modelDraft struct {
	Title   string `json:"title"   jsonschema:"description=Short descriptive title for the post"`
	Content string `json:"content" jsonschema:"description=The finished, platform-ready post copy — publish-ready, not an outline or bullet list"`
}

// DraftPostRepos bundles the repository dependencies for the flow.
type DraftPostRepos struct {
	Campaigns repository.CampaignRepository
	Platforms repository.PlatformRepository
	Posts     repository.PostRepository
	// Notes stores the source research as a reference note on each created post.
	// nil skips note creation (the post is still created).
	Notes repository.PostNoteRepository
	// Brands resolves the campaign's brand voice/audience/guardrails into the
	// prompt. nil falls back to the legacy tone_guidelines prose.
	Brands repository.BrandRepository
}

// DraftPostFlowConfig holds static settings for the flow. Unlike content_plan it
// has no Hub: draftPost is only reached through the campaign assistant, whose
// runner already publishes the coarse assistant_completed finalisation event,
// so a second per-flow finalisation would be redundant.
type DraftPostFlowConfig struct {
	// Provider resolves the model reference + call config by role.
	// Draft copywriting runs on RoleGeneration (Sonnet-tier), like content_plan.
	Provider *llm.Provider
	// Recorder captures usage under flow name "draft_post"; nil disables it.
	Recorder *usage.Recorder
	// Checker gates the flow against the tenant's spend caps; nil = no gate.
	Checker *usage.Checker
	ModelID string
	// MaxOutputTokens caps a single generation call. 0 falls back to 8192 —
	// enough for a handful of full-length drafts.
	MaxOutputTokens int64

	systemTmpl  *template.Template
	contextTmpl *template.Template
}

// ValidationError is returned when preconditions are not met (HTTP 400).
type ValidationError struct{ Msg string }

func (e *ValidationError) Error() string { return e.Msg }

// AIError is returned when the model call fails or returns nothing usable
// (HTTP 502).
type AIError struct{ Msg string }

func (e *AIError) Error() string { return e.Msg }

// ── SSE types ─────────────────────────────────────────────────────────────────

// SSEEventKind identifies the SSE event types emitted by the flow. The
// campaign_assistant tool re-emits these namespaced (draft_post_*).
type SSEEventKind string

const (
	SSEEventStep     SSEEventKind = "step"
	SSEEventPost     SSEEventKind = "post"
	SSEEventWarning  SSEEventKind = "warning"
	SSEEventComplete SSEEventKind = "complete"
	SSEEventError    SSEEventKind = "error"
)

// StepEventPayload marks a flow stage as finished.
type StepEventPayload struct {
	Step   string `json:"step"`
	Status string `json:"status"` // always "done"
}

// PostEventPayload carries one finished draft with its persisted row id, so the
// client can render it immediately. Index is the draft's slot in the
// returned order.
type PostEventPayload struct {
	Post  DraftedPost `json:"post"`
	Index int         `json:"index"`
	ID    string      `json:"id"`
}

// WarningPayload is emitted when a draft is dropped (empty copy) or fails to
// persist; the run continues.
type WarningPayload struct {
	Message string `json:"message"`
}

// ErrorEventPayload is emitted when the flow fails mid-stream.
type ErrorEventPayload struct {
	Message string `json:"message"`
	Code    int    `json:"code"` // HTTP semantic: 400, 402, 502, 500
}

// OnEventFunc is an optional callback invoked as SSE events are produced. A nil
// OnEventFunc is valid — the flow runs silently.
type OnEventFunc func(name SSEEventKind, data any)
