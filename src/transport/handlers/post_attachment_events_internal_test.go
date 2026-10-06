package handlers

import (
	"context"
	"testing"

	"github.com/ogen-app/ogen/src/infra/eventhub"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
)

// recordingHub captures published events.
type recordingHub struct{ events []eventhub.Event }

func (r *recordingHub) Publish(_ context.Context, ev eventhub.Event) error {
	r.events = append(r.events, ev)
	return nil
}

func (r *recordingHub) Subscribe(context.Context, eventhub.SubscribeOpts) (<-chan eventhub.Event, func(), error) {
	return nil, func() {}, nil
}

func TestPublishAttachmentsChanged_FailsClosedWithoutTenant(t *testing.T) {
	hub := &recordingHub{}
	h := (&PostAttachmentsHandler{}).WithEventHub(hub)

	// No tenant and no user would reach every tenant's subscribers.
	h.publishAttachmentsChanged(context.Background(), "p1", "a1", attachmentActionCreated, attachmentSourceEditor)
	if len(hub.events) != 0 {
		t.Fatalf("published %d events without a tenant, want 0", len(hub.events))
	}
}

func TestPublishAttachmentsChanged_ScopesToTenantNotUser(t *testing.T) {
	hub := &recordingHub{}
	h := (&PostAttachmentsHandler{}).WithEventHub(hub)

	h.publishAttachmentsChanged(tenantctx.With(context.Background(), "tn-1"), "p1", "a1", attachmentActionCreated, attachmentSourceFigmaPlugin)
	if len(hub.events) != 1 {
		t.Fatalf("published %d events, want 1", len(hub.events))
	}
	ev := hub.events[0]
	if ev.TenantID != "tn-1" || ev.UserID != "" {
		t.Errorf("tenant/user = %q/%q, want tn-1/empty", ev.TenantID, ev.UserID)
	}
	if ev.Topic != "entity:post:p1" || ev.Type != EventPostAttachmentsChanged || ev.ID == "" {
		t.Errorf("unexpected envelope %+v", ev)
	}
}

func TestPublishAttachmentsChanged_NilHub(t *testing.T) {
	// The default handler has no hub; publishing must be a no-op, not a panic.
	(&PostAttachmentsHandler{}).publishAttachmentsChanged(tenantctx.With(context.Background(), "tn-1"), "p1", "a1", attachmentActionDeleted, attachmentSourceEditor)
}
