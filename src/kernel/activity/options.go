package activity

import "github.com/ogen-app/ogen/src/domain/models"

// Category values name the primary activity tag — the main slice dimension
// (CON-125 §8). Free-form is allowed, but call-sites should use these.
const (
	CategoryAuthentication = "authentication"
	CategoryPost           = "post"
	CategoryCampaign       = "campaign"
	CategoryAIFlow         = "ai_flow"
	CategoryPublish        = "publish"
	CategoryEmail          = "email"
	CategoryWorkspace      = "workspace"
	CategoryBrand          = "brand"
	CategoryIdea           = "idea"
)

// Source values name how an activity was triggered. Free-form is allowed, but
// prefer these so the dimension stays queryable.
const (
	SourceAPI       = "api"
	SourceAssistant = "assistant"
	SourceJob       = "job"
	SourceSystem    = "system"
)

// Option customises an ActivityEvent before it is enqueued. Options are applied
// AFTER the Recorder resolves tenant + user from context, so WithUser can
// override (or supply) the actor when the context does not carry one — e.g. the
// signup path, where the user was just created.
type Option func(*models.ActivityEvent)

// WithUser sets the acting user id explicitly.
func WithUser(userID string) Option {
	return func(e *models.ActivityEvent) { e.UserID = userID }
}

// WithEntity sets the entity the action targeted.
func WithEntity(entityType, entityID string) Option {
	return func(e *models.ActivityEvent) {
		e.EntityType = entityType
		e.EntityID = entityID
	}
}

// WithStatus sets the outcome / resulting state (e.g. "success", "failed", or a
// "from->to" transition).
func WithStatus(status string) Option {
	return func(e *models.ActivityEvent) { e.Status = status }
}

// WithSource sets how the action was triggered (see the Source* constants).
func WithSource(source string) Option {
	return func(e *models.ActivityEvent) { e.Source = source }
}

// WithTags appends optional secondary labels.
func WithTags(tags ...string) Option {
	return func(e *models.ActivityEvent) { e.Tags = append(e.Tags, tags...) }
}

// WithPayload attaches the activity-specific field bag. A nil/empty map is
// ignored so the column stays NULL.
func WithPayload(payload map[string]any) Option {
	return func(e *models.ActivityEvent) {
		if len(payload) == 0 {
			return
		}
		e.Payload = models.JSONMap(payload)
	}
}
