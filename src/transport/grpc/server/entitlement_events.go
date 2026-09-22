package server

import (
	"context"

	"log/slog"

	"github.com/ogen-app/ogen/src/infra/eventhub"
	"github.com/ogen-app/ogen/src/kernel/logging"
)

// publishEntitlementChange fans a tenant-scoped invalidation event onto the
// in-process hub after an operator changes a tenant's tier/version (CON-295 §4).
// A client subscribed to /api/events treats it as a pure refetch hint for
// GET /api/me/entitlements, so an open tab drops controls a downgrade removed
// (or gains ones an upgrade granted) without waiting for a query to remount.
//
// TenantID is set explicitly because the operator gRPC context carries no
// tenant; the hub's per-tenant isolation then routes the event only to that
// tenant's subscribers. Best-effort: a nil hub or a publish error never affects
// the assignment that already committed.
func publishEntitlementChange(ctx context.Context, hub eventhub.Hub, tenantID string) {
	if hub == nil || tenantID == "" {
		return
	}
	if err := hub.Publish(ctx, eventhub.Event{
		Topic:    "entity:entitlements:" + tenantID,
		Type:     "updated",
		TenantID: tenantID,
	}); err != nil {
		slog.WarnContext(ctx, "entitlement invalidation publish failed",
			logging.AttrComponent, "grpcserver", "tenant_id", tenantID, logging.AttrError, err)
	}
}
