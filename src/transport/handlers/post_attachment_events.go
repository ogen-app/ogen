package handlers

import (
	"context"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/eventhub"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
)

// EventPostAttachmentsChanged is the bus type published on entity:post:<id>
// after any committed attachment write. It is an invalidation hint: clients
// refetch the attachment list rather than read data off the event.
const EventPostAttachmentsChanged = "post.attachments.changed"

// What happened to the post's attachments.
const (
	attachmentActionCreated   = "created"
	attachmentActionUpdated   = "updated"
	attachmentActionReordered = "reordered"
	attachmentActionDeleted   = "deleted"
)

// Which entry point wrote the attachment.
const (
	attachmentSourceEditor      = "editor"
	attachmentSourceBank        = "bank"
	attachmentSourceFigmaPlugin = "figma_plugin"
	attachmentSourceAltText     = "alt_text"
)

// WithEventHub makes attachment writes publish post.attachments.changed. A nil
// hub (the default) disables eventing.
func (h *PostAttachmentsHandler) WithEventHub(hub eventhub.Hub) *PostAttachmentsHandler {
	h.hub = hub
	return h
}

// publishAttachmentsChanged announces a committed attachment write to every
// workspace member watching the post, not just the actor: the event carries
// the tenant and no user. It fails closed without a tenant in ctx, because an
// event with neither filter set would reach every tenant's subscribers.
// Best-effort; the write has already committed. Presigned URLs never travel on
// the bus, so attachmentID may be empty (reorder touches the whole list).
func (h *PostAttachmentsHandler) publishAttachmentsChanged(ctx context.Context, postID, attachmentID, action, source string) {
	if h.hub == nil {
		return
	}
	tenantID, ok := tenantctx.From(ctx)
	if !ok {
		return
	}
	evID, err := models.NewID()
	if err != nil {
		return
	}
	_ = h.hub.Publish(ctx, eventhub.Event{
		ID:       evID,
		Topic:    "entity:post:" + postID,
		Type:     EventPostAttachmentsChanged,
		TenantID: tenantID,
		Payload: map[string]any{
			"post_id":       postID,
			"attachment_id": attachmentID,
			"action":        action,
			"source":        source,
		},
	})
}
