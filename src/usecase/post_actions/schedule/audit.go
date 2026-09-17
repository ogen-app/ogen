package schedule

import (
	"context"
	"encoding/json"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/eventhub"
	"github.com/ogen-app/ogen/src/infra/publishers/zernio"
	"github.com/ogen-app/ogen/src/usecase/post_actions/logs"
)

// promoteLogs builds the audit entries for an auto-promote: the CON-74
// validation pass plus the Draft → ReadyForPublish state transition.
func (s *Service) promoteLogs(postID, actor string) []*models.PostLog {
	from := models.PostStatusDraft
	to := models.PostStatusReadyForPublish
	validationID, _ := models.NewID()
	transitionID, _ := models.NewID()
	return []*models.PostLog{
		{
			ID:         validationID,
			PostID:     postID,
			EventType:  models.PostLogEventValidationPassed,
			Actor:      actor,
			FromStatus: &from,
			ToStatus:   &to,
			Summary:    "draft → ready_for_publish passed pre-publish validation (auto-promote before schedule)",
			Payload:    "{}",
		},
		{
			ID:         transitionID,
			PostID:     postID,
			EventType:  models.PostLogEventStateTransition,
			Actor:      actor,
			FromStatus: &from,
			ToStatus:   &to,
			Summary:    "auto-promoted draft → ready_for_publish before scheduling",
			Payload:    "{}",
		},
	}
}

// decisionLog builds the allowlist-decision audit entry describing how
// the post was routed (auto vs manual) for its platform.
func (s *Service) decisionLog(postID string, from, to models.PostStatus, platformID string, supported *zernio.SupportedPlatform, autoPublish bool, actor string) *models.PostLog {
	id, _ := models.NewID()
	payload := logs.MarshalCapped(map[string]any{
		"platform":        platformID,
		"zernio_platform": zernioName(supported),
		"auto_publish":    autoPublish,
		"chosen_status":   string(to),
	})
	fromCopy, toCopy := from, to
	return &models.PostLog{
		ID:         id,
		PostID:     postID,
		EventType:  models.PostLogEventAllowlistDecision,
		Actor:      actor,
		FromStatus: &fromCopy,
		ToStatus:   &toCopy,
		Summary:    "auto-publish allowlist decision",
		Payload:    logs.SanitizeAndCap(payload),
	}
}

// transitionLog builds a state-transition audit entry.
func (s *Service) transitionLog(postID string, from, to models.PostStatus, summary, actor string) *models.PostLog {
	id, _ := models.NewID()
	fromCopy, toCopy := from, to
	return &models.PostLog{
		ID:         id,
		PostID:     postID,
		EventType:  models.PostLogEventStateTransition,
		Actor:      actor,
		FromStatus: &fromCopy,
		ToStatus:   &toCopy,
		Summary:    summary,
		Payload:    "{}",
	}
}

// scheduleActionLog builds the CON-23 action-log entry recorded for every
// schedule, carrying the resolved instant, routed status, auto/manual
// mode, whether the post was promoted, and the trigger.
func (s *Service) scheduleActionLog(postID string, scheduledAt time.Time, target models.PostStatus, autoPublish, promoted bool, opts Options) *models.PostLog {
	id, _ := models.NewID()
	payload, _ := json.Marshal(map[string]any{
		"scheduled_at": scheduledAt.Format(time.RFC3339),
		"status":       string(target),
		"auto_publish": autoPublish,
		"promoted":     promoted,
		"trigger":      opts.Trigger,
	})
	return &models.PostLog{
		ID:        id,
		PostID:    postID,
		EventType: models.PostLogEventUserSchedule,
		Actor:     opts.Actor,
		Summary:   "post scheduled for publishing",
		Payload:   logs.SanitizeAndCap(string(payload)),
	}
}

// publishScheduled announces a successful schedule on the shared event
// hub so other tabs can refresh. Best-effort; a nil hub is a no-op.
func (s *Service) publishScheduled(postID, actor string, scheduledAt time.Time, target models.PostStatus, autoPublish, promoted bool) {
	if s.hub == nil {
		return
	}
	evID, err := models.NewID()
	if err != nil {
		return
	}
	_ = s.hub.Publish(context.Background(), eventhub.Event{
		ID:     evID,
		Topic:  "entity:post:" + postID,
		// Dotted bus wire type (CON-285). The post_logs event for scheduling is a
		// separate constant (PostLogEventUserSchedule = "user_schedule") and the
		// tenant_activity_events taxonomy stays "post_scheduled" — neither renamed.
		Type:   "post.scheduled",
		UserID: actor,
		Payload: map[string]any{
			"scheduledAt": scheduledAt.Format(time.RFC3339),
			"status":      string(target),
			"autoPublish": autoPublish,
			"promoted":    promoted,
		},
	})
}
