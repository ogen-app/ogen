package server

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/ogen-app/ogen/src/domain/entitlements"
	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
	"github.com/ogen-app/ogen/src/usecase/notify"
)

// limitNotifyTTL fades a near-limit notification out of the inbox after a few
// days — it is transient "you're running out" state, not a durable record.
const limitNotifyTTL = 7 * 24 * time.Hour

// limitNotifier adapts near-limit crossings from the entitlement Limiter (CON-295
// §12) to a durable notification (CON-242). It lives in the wiring layer so the
// domain never imports the notification center or the repository: on a crossing
// it resolves the tenant's workspace owners and fans one spec out to their
// inboxes. Best-effort throughout — a lookup or emit failure is logged, never
// propagated, so it can never affect the create path.
type limitNotifier struct {
	notify *notify.Service
	users  repository.UserRepository
}

// NotifyLimit implements entitlements.Notifier. Delivery (owner lookup + one
// insert per owner) runs in a goroutine so it never delays the create path, as
// the Notifier contract requires. The request context cannot be reused after the
// handler returns (fasthttp recycles it), so we start a fresh tenant-scoped
// context from the passed tenantID rather than detaching the caller's ctx.
func (n *limitNotifier) NotifyLimit(_ context.Context, tenantID string, ev entitlements.LimitEvent) {
	if n == nil || n.notify == nil || n.users == nil || tenantID == "" {
		return
	}
	go n.deliver(tenantctx.With(context.Background(), tenantID), tenantID, ev)
}

// deliver performs the owner lookup + fan-out off the request path. A panic here
// must not take down the process, so it is recovered and logged.
func (n *limitNotifier) deliver(ctx context.Context, tenantID string, ev entitlements.LimitEvent) {
	defer func() {
		if r := recover(); r != nil {
			slog.ErrorContext(ctx, "entitlement near-limit notify: panic recovered",
				logging.AttrComponent, "entitlements", "feature", ev.Feature, "panic", r)
		}
	}()
	owners, err := n.users.ListOwnersByTenant(ctx, tenantID)
	if err != nil {
		slog.WarnContext(ctx, "entitlement near-limit notify: list owners failed",
			logging.AttrComponent, "entitlements", "feature", ev.Feature, logging.AttrError, err)
		return
	}
	if len(owners) == 0 {
		return
	}
	ids := make([]string, 0, len(owners))
	for _, o := range owners {
		ids = append(ids, o.ID)
	}
	if err := n.notify.EmitToUsers(ctx, ids, limitSpec(ev)); err != nil {
		slog.WarnContext(ctx, "entitlement near-limit notify: emit failed",
			logging.AttrComponent, "entitlements", "feature", ev.Feature, logging.AttrError, err)
	}
}

// limitSpec renders a LimitEvent into a notification. The DedupeKey collapses
// repeats of the same feature+state while the prior one is still unread, so
// retries and re-crossings do not spam the inbox.
func limitSpec(ev entitlements.LimitEvent) notify.Spec {
	name := ev.Name
	if name == "" {
		name = ev.Feature
	}
	expiresAt := time.Now().Add(limitNotifyTTL)
	spec := notify.Spec{
		Level: models.NotificationLevelWarning,
		Data: map[string]any{
			"feature":   ev.Feature,
			"current":   ev.Current,
			"limit":     ev.Limit,
			"remaining": max(ev.Limit-ev.Current, 0),
			"percent":   ev.Percent,
			"state":     string(ev.State),
		},
		DedupeKey: "entitlement_limit:" + ev.Feature + ":" + string(ev.State),
		ExpiresAt: &expiresAt,
	}
	switch ev.State {
	case entitlements.LimitReached:
		spec.Type = "entitlement.limit_reached"
		spec.Title = "You've reached your " + name + " limit"
		spec.Body = fmt.Sprintf("You've used all %d %s on your plan. Upgrade to add more.", ev.Limit, name)
	default: // approaching
		spec.Type = "entitlement.limit_approaching"
		spec.Title = "Approaching your " + name + " limit"
		spec.Body = fmt.Sprintf("You've used %d of %d %s on your plan.", ev.Current, ev.Limit, name)
	}
	return spec
}
