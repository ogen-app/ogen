package server

import (
	"context"
	"testing"

	"github.com/ogen-app/ogen/src/infra/eventhub"
)

// spyHub records published events and is a no-op subscriber — enough to assert
// what an operator tier change fans out (CON-295 §4) without the in-process Hub.
type spyHub struct{ events []eventhub.Event }

func (s *spyHub) Publish(_ context.Context, ev eventhub.Event) error {
	s.events = append(s.events, ev)
	return nil
}

func (s *spyHub) Subscribe(context.Context, eventhub.SubscribeOpts) (<-chan eventhub.Event, func(), error) {
	return nil, func() {}, nil
}

func TestPublishEntitlementChange(t *testing.T) {
	ctx := context.Background()

	// Nil hub is a no-op (a hub-less build must not panic on an assignment).
	publishEntitlementChange(ctx, nil, "tn-1")

	hub := &spyHub{}
	// An empty tenant id publishes nothing.
	publishEntitlementChange(ctx, hub, "")
	if len(hub.events) != 0 {
		t.Fatalf("empty tenant should publish nothing, got %d", len(hub.events))
	}

	// A real tenant publishes exactly one tenant-scoped invalidation event.
	publishEntitlementChange(ctx, hub, "tn-1")
	if len(hub.events) != 1 {
		t.Fatalf("want 1 event, got %d", len(hub.events))
	}
	ev := hub.events[0]
	if ev.Topic != "entity:entitlements:tn-1" {
		t.Fatalf("topic = %q, want entity:entitlements:tn-1", ev.Topic)
	}
	if ev.Type != "updated" {
		t.Fatalf("type = %q, want updated", ev.Type)
	}
	// TenantID must be set explicitly: the operator gRPC context has no tenant, so
	// the hub's isolation would otherwise not route this to the tenant's tabs.
	if ev.TenantID != "tn-1" {
		t.Fatalf("TenantID = %q, want tn-1 (tenant isolation)", ev.TenantID)
	}
}
