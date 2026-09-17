package post_assistant

import (
	"github.com/firebase/genkit/go/ai"

	"github.com/ogen-app/ogen/src/infra/eventhub"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/infra/vendors/llm"
	"github.com/ogen-app/ogen/src/kernel/usage"
	"github.com/ogen-app/ogen/src/usecase/notes"
	"github.com/ogen-app/ogen/src/usecase/notify"
	"github.com/ogen-app/ogen/src/usecase/post_actions/clone"
	"github.com/ogen-app/ogen/src/usecase/post_actions/restore"
	"github.com/ogen-app/ogen/src/usecase/post_actions/schedule"
)

// PostAssistantRequest is the input to the postAssistant flow.
type PostAssistantRequest struct {
	PostID      string `json:"postId"`
	Instruction string `json:"instruction"`
}

// PostAssistantResponse is the structured output from the assistant.
// Fields are ordered so that Explanation and UpdatedContent appear first
// in the model's JSON output — this lets them start streaming to the
// client before the smaller metadata fields are generated.
type PostAssistantResponse struct {
	Explanation    string `json:"explanation"                  jsonschema:"description=Human-readable explanation of what was changed or why the request was declined"`
	UpdatedContent string `json:"updatedContent"               jsonschema:"description=The full updated post content as Markdown; empty when action is declined"`
	Action         string `json:"action"                       jsonschema:"description=edited when content was changed; declined when the request is out of scope or you are asking the user to confirm; cloned when a clone was created; restored when the post was rolled back to an earlier version; scheduled when the post was scheduled for publishing; noted when one or more notes were created without changing the post body,enum=edited,enum=declined,enum=cloned,enum=restored,enum=scheduled,enum=noted"`
	SaveVersion    bool   `json:"saveVersion"                  jsonschema:"description=True when a new version snapshot should be created"`
	VersionNote    string `json:"versionNote,omitempty"        jsonschema:"description=Short note describing the version; only present when saveVersion is true"`
	// CloneResult is populated by the server (not the model) when the
	// clonePost tool created a clone during this turn. Action is then
	// "cloned" and UpdatedContent is empty — the source post is untouched.
	CloneResult *CloneResultPayload `json:"cloneResult,omitempty" jsonschema:"-"`
	// RestoreResult is populated by the server (not the model) when the
	// restoreVersion tool ran this turn. Action is then "restored" and
	// UpdatedContent holds the restored content.
	RestoreResult *RestoreResultPayload `json:"restoreResult,omitempty" jsonschema:"-"`
	// ScheduleResult is populated by the server (not the model) when the
	// schedulePost tool ran this turn. Action is then "scheduled" and
	// UpdatedContent is empty — scheduling doesn't change content.
	ScheduleResult *ScheduleResultPayload `json:"scheduleResult,omitempty" jsonschema:"-"`
	// NotesCreated is populated by the server (not the model) with the notes
	// the createNote tool persisted this turn (CON-188). Notes are additive:
	// they may accompany an "edited" turn or stand alone as a "noted" turn.
	NotesCreated []NotePayload `json:"notesCreated,omitempty" jsonschema:"-"`
}

// NotePayload describes a note created by the createNote tool this turn.
type NotePayload struct {
	ID    string `json:"id"`
	Type  string `json:"type"`
	Title string `json:"title,omitempty"`
	Body  string `json:"body"`
}

// CloneResultPayload describes the post created by the clonePost tool.
type CloneResultPayload struct {
	NewPostID  string `json:"newPostId"`
	PlatformID string `json:"platformId,omitempty"`
	PostType   string `json:"postType,omitempty"`
	Adapted    bool   `json:"adapted"`
}

// RestoreResultPayload describes the outcome of the restoreVersion tool.
type RestoreResultPayload struct {
	RestoredFromVersion int  `json:"restoredFromVersion"`
	NewVersionNumber    int  `json:"newVersionNumber"`
	NoOp                bool `json:"noOp"`
}

// ScheduleResultPayload describes the outcome of the schedulePost tool.
type ScheduleResultPayload struct {
	ScheduledAt string `json:"scheduledAt"`
	Status      string `json:"status"`
	AutoPublish bool   `json:"autoPublish"`
	Promoted    bool   `json:"promoted"`
}

// PostAssistantRepos bundles all repository dependencies for the flow.
type PostAssistantRepos struct {
	Posts     repository.PostRepository
	Assets    repository.AssetRepository
	Chunks    repository.AssetChunksRepository
	Campaigns repository.CampaignRepository
	Versions  repository.PostVersionRepository
	Messages  repository.PostAssistantMessageRepository
	// Platforms lets the clonePost tool resolve a platform name (e.g.
	// "Threads") to its ID and surface the available platforms to the
	// model. nil disables cross-platform clone resolution.
	Platforms repository.PlatformRepository
	// Settings backs the workspace-timezone lookup the schedulePost flow
	// uses to resolve and echo times (CON-78). nil → UTC.
	Settings repository.SettingRepository
	// Allowlist lets the scheduling context tell the model whether the
	// post's platform auto-publishes or is manual (CON-65/78). nil →
	// everything reads as manual.
	Allowlist repository.AutoPublishAllowlistRepository
	// Attachments backs the CON-74 readiness summary surfaced to the model
	// so it can pre-empt a promote-time validation failure. nil → the
	// readiness summary omits attachment-based rules.
	Attachments repository.PostAttachmentRepository
	// Notes surfaces the post's existing notes (draft theses, image prompts,
	// side notes) to the model as context (CON-188). nil omits the notes
	// section. Writes go through the shared NoteService, not this repo.
	Notes repository.PostNoteRepository
	// Brands resolves the post's brand voice/audience/guardrails into the
	// editing context (CON-245). nil falls back to the legacy tone prose.
	Brands repository.BrandRepository
}

// PostAssistantFlowConfig holds settings for the post assistant flow.
type PostAssistantFlowConfig struct {
	// Provider resolves the model reference + call config by role (CON-86 FR12).
	Provider *llm.Provider
	// Recorder captures usage events; nil disables recording (CON-86 FR5/FR10).
	Recorder *usage.Recorder
	// Checker gates the flow against the tenant's spend caps; nil = no gate.
	Checker *usage.Checker
	ModelID string
	// MaxOutputTokens caps the model's output for a single call. 0 falls
	// back to 64000 — Claude 4.x Haiku/Sonnet's max output. Anthropic
	// charges only for tokens actually emitted, so a generous cap costs
	// nothing on short responses but prevents truncation when the
	// explanation + full updated content + tool inputs combined exceed
	// a smaller cap. When Anthropic stops at the cap, the response's
	// FinishReason is FinishReasonLength and we log "TRUNCATED" loudly.
	MaxOutputTokens int64
	// MaxTurns caps tool-use round-trips. The model needs one extra turn
	// for the final answer after its last tool call, so MaxTurns=N allows
	// up to N-1 tool calls. 0 falls back to a sensible default (8) that
	// covers realistic asset-incorporation scenarios (browse + search a
	// few assets + read chunks + respond).
	MaxTurns int
	// PlannerEnabled turns on the CON-128 hybrid split: the orchestration
	// loop runs on the planning model (Haiku) and the actual copywriting is
	// delegated to the Sonnet editPost write-tool. False = the legacy
	// single-Sonnet path (loop on the generation model, no editPost tool,
	// content written inline) — the instant-rollback fallback.
	PlannerEnabled bool
	// PlannerMaxOutputTokens caps the planner turn's output when
	// PlannerEnabled. The planner only emits a short envelope + tool inputs,
	// so a small cap is plenty; the writer sub-call uses MaxOutputTokens.
	// 0 falls back to 8192.
	PlannerMaxOutputTokens int64
	Embedder               ai.Embedder // nil = semantic search unavailable
	// Hub is the event broker used to publish "operation finalised"
	// events on success/failure. nil = silent (no events emitted).
	Hub eventhub.Hub
	// PrewarmTools, when true, fires one throwaway generation carrying the full
	// tool set at init so Anthropic compiles + caches the strict-tool grammar
	// off the user path (CON-112). Only worth it alongside a stable tool order,
	// so the server sets this from cfg.AnthropicStableToolOrder.
	PrewarmTools bool
	// CloneService backs the clonePost tool (CON-59). nil disables the
	// tool — the assistant then has no clone capability.
	CloneService *clone.Service
	// RestoreService backs the restoreVersion tool (CON-68). nil disables
	// the tool — the assistant then has no restore capability.
	RestoreService *restore.Service
	// ScheduleService backs the schedulePost tool (CON-78). nil disables
	// the tool — the assistant then has no scheduling capability.
	ScheduleService *schedule.Service
	// NoteService backs the createNote tool (CON-188). nil disables the
	// tool — the assistant then cannot capture notes.
	NoteService *notes.Service
	// Notifier drops a durable "assistant finished / failed" notification to the
	// post owner (CON-285). nil is a no-op.
	Notifier *notify.Service
}

// ValidationError is returned when preconditions are not met (HTTP 400).
type ValidationError struct{ Msg string }

func (e *ValidationError) Error() string { return e.Msg }

// ErrPostRemovedDuringTurn is returned when the post is deleted — directly via
// DELETE /api/posts/:id, or by a campaign/owner cascade — while the assistant
// is mid-turn. The flow loads the post up front without a lock and only writes
// the conversation turn after a potentially long (tens of seconds) model call,
// so the row can disappear in between; the write then trips the post_id foreign
// key (SQLSTATE 23503). Surfacing this precondition error keeps the client from
// seeing a raw database error, and it is treated like "post not found" (400).
var ErrPostRemovedDuringTurn = &ValidationError{Msg: "This post was deleted while the assistant was working, so nothing was saved."}

// AIError is returned when the model call fails (HTTP 502).
type AIError struct{ Msg string }

func (e *AIError) Error() string { return e.Msg }

// ── SSE types ─────────────────────────────────────────────────────────────────

// SSEEventKind identifies the SSE event types emitted by the assistant flow.
type SSEEventKind string

const (
	SSEEventExplanationDelta SSEEventKind = "explanation_delta"
	SSEEventContentDelta     SSEEventKind = "content_delta"
	SSEEventToolCall         SSEEventKind = "tool_call"
	SSEEventToolResult       SSEEventKind = "tool_result"
	SSEEventCloneStarted     SSEEventKind = "clone_started"
	SSEEventCloneComplete    SSEEventKind = "clone_complete"
	SSEEventRestoreStarted   SSEEventKind = "restore_started"
	SSEEventRestoreComplete  SSEEventKind = "restore_complete"
	SSEEventScheduleStarted  SSEEventKind = "schedule_started"
	SSEEventScheduleComplete SSEEventKind = "schedule_complete"
	SSEEventNoteCreated      SSEEventKind = "note_created"
	SSEEventComplete         SSEEventKind = "complete"
	SSEEventError            SSEEventKind = "error"
)

// NoteCreatedEventPayload is emitted each time the createNote tool persists a
// note during a turn (CON-188), so the UI can surface it live.
type NoteCreatedEventPayload struct {
	ID    string `json:"id"`
	Type  string `json:"type"`
	Title string `json:"title,omitempty"`
	Body  string `json:"body"`
}

// CloneStartedEventPayload is emitted when the clonePost tool begins
// creating a clone, so the UI can show progress.
type CloneStartedEventPayload struct {
	TargetPlatform string `json:"targetPlatform,omitempty"`
}

// CloneCompleteEventPayload is emitted once the clone exists, carrying a
// navigable id for the new draft.
type CloneCompleteEventPayload struct {
	NewPostID  string `json:"newPostId"`
	PlatformID string `json:"platformId,omitempty"`
	PostType   string `json:"postType,omitempty"`
	Adapted    bool   `json:"adapted"`
}

// RestoreStartedEventPayload is emitted when the restoreVersion tool
// begins, so the UI can show progress.
type RestoreStartedEventPayload struct {
	TargetVersion int `json:"targetVersion"`
}

// RestoreCompleteEventPayload is emitted once the restore is persisted.
type RestoreCompleteEventPayload struct {
	RestoredFromVersion int  `json:"restoredFromVersion"`
	NewVersionNumber    int  `json:"newVersionNumber"`
	NoOp                bool `json:"noOp"`
}

// ScheduleStartedEventPayload is emitted when the schedulePost tool
// begins committing a schedule, so the UI can show progress.
type ScheduleStartedEventPayload struct {
	ScheduledAt string `json:"scheduledAt"`
	AutoPublish bool   `json:"autoPublish"`
}

// ScheduleCompleteEventPayload is emitted once the schedule is persisted.
type ScheduleCompleteEventPayload struct {
	ScheduledAt string `json:"scheduledAt"`
	Status      string `json:"status"`
	AutoPublish bool   `json:"autoPublish"`
	Promoted    bool   `json:"promoted"`
}

// DeltaEventPayload carries a fragment of a streamed string value.
type DeltaEventPayload struct {
	Delta string `json:"delta"`
}

// ToolCallEventPayload is emitted once per fully-formed tool request from
// the model. Partial streaming fragments of the tool input are suppressed
// to avoid noisy per-character events.
type ToolCallEventPayload struct {
	Name  string `json:"name"`
	Input any    `json:"input,omitempty"`
	Ref   string `json:"ref,omitempty"`
}

// ToolResultEventPayload is emitted once a tool invocation returns.
// Output bytes are intentionally omitted — the client only needs a "done"
// signal to resolve the matching tool_call chip.
type ToolResultEventPayload struct {
	Name string `json:"name"`
	Ref  string `json:"ref,omitempty"`
	OK   bool   `json:"ok"`
}

// ErrorEventPayload is emitted when the flow fails.
type ErrorEventPayload struct {
	Message string `json:"message"`
	Code    int    `json:"code"` // HTTP semantic: 400, 502, 500
}

// OnEventFunc is an optional callback invoked as SSE events are produced.
// A nil OnEventFunc is valid — the flow runs silently.
type OnEventFunc func(name SSEEventKind, data any)
