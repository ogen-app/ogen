package models

import (
	"time"

	"github.com/uptrace/bun"
)

// PostLogEventType is the controlled vocabulary of event names.
// Strings are stable across versions so the frontend and ops queries
// can switch on them without parsing free text.
type PostLogEventType string

const (
	// State machine
	PostLogEventStateTransition        PostLogEventType = "state_transition"
	PostLogEventStateTransitionBlocked PostLogEventType = "state_transition_blocked"

	// Validation
	PostLogEventValidationPassed PostLogEventType = "validation_passed"
	PostLogEventValidationFailed PostLogEventType = "validation_failed"

	// Allowlist + scheduling decision
	PostLogEventAllowlistDecision PostLogEventType = "allowlist_decision"

	// Background task lifecycle (River)
	PostLogEventTaskEnqueued  PostLogEventType = "task_enqueued"
	PostLogEventTaskStarted   PostLogEventType = "task_started"
	PostLogEventTaskSucceeded PostLogEventType = "task_succeeded"
	PostLogEventTaskFailed    PostLogEventType = "task_failed"
	PostLogEventTaskRetried   PostLogEventType = "task_retried"
	PostLogEventTaskPanicked  PostLogEventType = "task_panicked"

	// Zernio interactions
	PostLogEventZernioSubmit PostLogEventType = "zernio_submit"
	PostLogEventZernioPoll   PostLogEventType = "zernio_poll"
	PostLogEventZernioCancel PostLogEventType = "zernio_cancel"
	PostLogEventZernioRetry  PostLogEventType = "zernio_retry"

	// Reconciliation
	PostLogEventReconciliationTimeout PostLogEventType = "reconciliation_timeout"

	// User actions
	PostLogEventUserSchedule PostLogEventType = "user_schedule"
	PostLogEventUserCancel   PostLogEventType = "user_cancel"
	PostLogEventUserRetry    PostLogEventType = "user_retry"

	// Clone: recorded on the NEW post; payload carries the
	// source id, target platform, adaptation flag and trigger.
	PostLogEventPostCloned PostLogEventType = "post_cloned"

	// Restore: recorded when a post is rolled back to an earlier
	// version. Payload carries the restored-from version, the new version
	// number, whether a pre-restore safety snapshot was taken, and the
	// trigger (assistant|api).
	PostLogEventPostRestored PostLogEventType = "post_restored"

	// Quality assessment: recorded when the Post quality agent
	// finalises an evaluation; payload carries the overall percentage, the
	// per-dimension scores, and the model used.
	PostLogEventQualityAssessed PostLogEventType = "quality_assessed"
)

// ActorSystem is the actor sentinel used when the event was produced
// by automated machinery rather than a User.
const ActorSystem = "system"

// PostLog is one entry in a Post's audit history.
//
// Payload is a JSON-encoded blob. Writers MUST run it through
// logs.Capper to enforce the 64 KB ceiling and through
// logs.Sanitize to strip secrets — the column is plain TEXT so the
// guarantees live in code, not in the schema.
type PostLog struct {
	bun.BaseModel `bun:"table:post_logs,alias:pl" swaggerignore:"true"`
	TenantScoped  // tenant_id column + central scoping hooks

	ID             string           `bun:"id,pk"                                        json:"id"`
	PostID         string           `bun:"post_id,notnull"                              json:"post_id"`
	EventTimestamp time.Time        `bun:"event_timestamp,notnull,default:current_timestamp" json:"event_timestamp"`
	EventType      PostLogEventType `bun:"event_type,notnull"                           json:"event_type"`
	Actor          string           `bun:"actor,notnull"                                json:"actor"`
	FromStatus     *PostStatus      `bun:"from_status"                                  json:"from_status,omitempty"`
	ToStatus       *PostStatus      `bun:"to_status"                                    json:"to_status,omitempty"`
	Payload        string           `bun:"payload,notnull,default:'{}'"                 json:"payload"`
	Summary        string           `bun:"summary,notnull,default:''"                   json:"summary"`
}
