package ideas

import (
	"context"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/eventhub"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
)

// Bus contract of idea changes: EventDeleted on Topic after ideas are
// deleted, carrying {ids}. A refetch hint for every Ideas list and the asset
// counts that read ideas.
const (
	Topic        = "entity:ideas:list"
	EventDeleted = "ideas.deleted"
)

// PublishDeleted announces committed deletes to every member of the
// workspace in ctx. It fails closed without a tenant in ctx, because an
// event with neither filter set would reach every tenant. A nil hub
// publishes nothing. Best-effort.
func PublishDeleted(ctx context.Context, hub eventhub.Hub, ids []string) {
	if hub == nil || len(ids) == 0 {
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
	_ = hub.Publish(ctx, eventhub.Event{
		ID: evID, Topic: Topic, Type: EventDeleted, TenantID: tenantID,
		Payload: map[string][]string{"ids": ids},
	})
}
