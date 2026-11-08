package post_assistant

import (
	"github.com/firebase/genkit/go/ai"

	"github.com/ogen-app/ogen/src/infra/eventhub"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/infra/vendors/llm"
	"github.com/ogen-app/ogen/src/kernel/usage"
	"github.com/ogen-app/ogen/src/usecase/notes"
	"github.com/ogen-app/ogen/src/usecase/post_actions/clone"
	"github.com/ogen-app/ogen/src/usecase/post_actions/restore"
	"github.com/ogen-app/ogen/src/usecase/post_actions/schedule"
	"github.com/ogen-app/ogen/src/usecase/qualityauto"
)

// PostAssistantResponse is what the post pack's tools did in a turn, under
// "post" in the assistant's results: the action that labels the turn, the
// fallback reply, the content the edit or restore left, and each committed
// tool result.
type PostAssistantResponse struct {
	Explanation string `json:"explanation"`
	// Action is edited, cloned, restored, scheduled or noted.
	Action string `json:"action" enums:"edited,cloned,restored,scheduled,noted"`
	// UpdatedContent is the post's content after an edit or a restore.
	UpdatedContent string `json:"updatedContent,omitempty"`
	// SaveVersion is set when the edit was saved as a version, with its note.
	SaveVersion bool   `json:"saveVersion,omitempty"`
	VersionNote string `json:"versionNote,omitempty"`
	// CloneResult is the clone clonePost created; the post is untouched.
	CloneResult *CloneResultPayload `json:"cloneResult,omitempty"`
	// RestoreResult is the version restoreVersion rolled back to.
	RestoreResult *RestoreResultPayload `json:"restoreResult,omitempty"`
	// ScheduleResult is the schedule schedulePost committed.
	ScheduleResult *ScheduleResultPayload `json:"scheduleResult,omitempty"`
	// NotesCreated are the notes createNote saved. Notes are additive: they
	// may accompany an edit or stand alone as a "noted" turn.
	NotesCreated []NotePayload `json:"notesCreated,omitempty"`
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

// PostAssistantRepos bundles the stores the post pack reads and writes.
type PostAssistantRepos struct {
	Posts     repository.PostRepository
	Assets    repository.AssetRepository
	Chunks    repository.AssetChunksRepository
	Campaigns repository.CampaignRepository
	Versions  repository.PostVersionRepository
	// Logs records the status change when writing a planned post's body
	// makes it a draft. nil skips the audit entry.
	Logs repository.PostLogRepository
	// Platforms lets the clonePost tool resolve a platform name (e.g.
	// "Threads") to its ID and surface the available platforms to the
	// model. nil disables cross-platform clone resolution.
	Platforms repository.PlatformRepository
	// Settings backs the workspace-timezone lookup the schedulePost tool
	// uses to resolve and echo times. nil → UTC.
	Settings repository.SettingRepository
	// Allowlist tells the scheduling context whether the post's platform
	// auto-publishes or is manual. nil → everything reads as manual.
	Allowlist repository.AutoPublishAllowlistRepository
	// Attachments backs the readiness summary, so the model can pre-empt a
	// promote-time validation failure. nil omits attachment-based rules.
	Attachments repository.PostAttachmentRepository
	// Notes surfaces the post's notes (draft theses, image prompts, side
	// notes) as context. nil omits them. Writes go through NoteService.
	Notes repository.PostNoteRepository
	// Brands resolves the post's brand voice/audience/guardrails into the
	// editing context. nil falls back to the campaign's own tone.
	Brands repository.BrandRepository
}

// PostAssistantFlowConfig holds the post pack's services and limits.
type PostAssistantFlowConfig struct {
	// Provider resolves the writer's model and call config.
	Provider *llm.Provider
	// Recorder captures the writer's usage; nil disables recording.
	Recorder *usage.Recorder
	// MaxOutputTokens caps the writer's output. 0 falls back to 64000, so
	// a long post is never cut off; only emitted tokens are billed.
	MaxOutputTokens int64
	Embedder        ai.Embedder // nil = semantic search unavailable
	// Hub announces committed work on the post, so its other open tabs
	// refresh. nil = silent.
	Hub eventhub.Hub
	// CloneService, RestoreService, ScheduleService and NoteService back
	// clonePost, restoreVersion, schedulePost and createNote; nil makes
	// that tool refuse.
	CloneService    *clone.Service
	RestoreService  *restore.Service
	ScheduleService *schedule.Service
	NoteService     *notes.Service
	// QualityAuto queues an automatic quality assessment when an edit is
	// saved as a version. nil queues none.
	QualityAuto *qualityauto.Service
}

// postRemoved is the refusal when the post is deleted while a tool works
// on it: the write then finds no row, or trips the post_id foreign key.
const postRemoved = "This post was deleted while I was working on it, so nothing was saved."

// SSEEventKind names an event the post pack streams through the turn.
type SSEEventKind string

const (
	// SSEEventContentDelta is a fragment of the post the writer is writing.
	SSEEventContentDelta     SSEEventKind = "content_delta"
	SSEEventCloneStarted     SSEEventKind = "clone_started"
	SSEEventCloneComplete    SSEEventKind = "clone_complete"
	SSEEventRestoreStarted   SSEEventKind = "restore_started"
	SSEEventRestoreComplete  SSEEventKind = "restore_complete"
	SSEEventScheduleStarted  SSEEventKind = "schedule_started"
	SSEEventScheduleComplete SSEEventKind = "schedule_complete"
	SSEEventNoteCreated      SSEEventKind = "note_created"
)

// NoteCreatedEventPayload is emitted each time the createNote tool persists a
// note, so the UI can surface it live.
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

// OnEventFunc receives the pack's events; nil streams nothing.
type OnEventFunc func(name SSEEventKind, data any)
