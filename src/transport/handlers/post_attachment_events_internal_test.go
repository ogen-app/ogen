package handlers

import (
	"context"
	"errors"
	"testing"

	"github.com/ogen-app/ogen/src/infra/eventhub"
	"github.com/ogen-app/ogen/src/infra/repository"
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

// altTextRepo records SetGeneratedAltText; any other repository call panics.
type altTextRepo struct {
	repository.PostAttachmentRepository
	stored string
	err    error
}

func (r *altTextRepo) SetGeneratedAltText(_ context.Context, _ string, alt string) error {
	r.stored = alt
	return r.err
}

func TestStoreGeneratedAltText_AnnouncesTheWrite(t *testing.T) {
	hub, repo := &recordingHub{}, &altTextRepo{}
	h := (&PostAttachmentsHandler{repo: repo}).WithEventHub(hub)

	h.storeGeneratedAltText(tenantctx.With(context.Background(), "tn-1"), "p1", "a1", "  A red pixel  ")
	if repo.stored != "A red pixel" {
		t.Fatalf("stored %q, want trimmed alt text", repo.stored)
	}
	if len(hub.events) != 1 {
		t.Fatalf("published %d events, want 1", len(hub.events))
	}
	payload := hub.events[0].Payload.(map[string]any)
	if payload["action"] != attachmentActionUpdated || payload["source"] != attachmentSourceAltText || payload["attachment_id"] != "a1" {
		t.Errorf("unexpected payload %v", payload)
	}
}

func TestStoreGeneratedAltText_SilentWhenNothingStored(t *testing.T) {
	ctx := tenantctx.With(context.Background(), "tn-1")

	// Blank generation: no write, no event.
	hub, repo := &recordingHub{}, &altTextRepo{}
	(&PostAttachmentsHandler{repo: repo}).WithEventHub(hub).storeGeneratedAltText(ctx, "p1", "a1", "   ")
	if repo.stored != "" || len(hub.events) != 0 {
		t.Errorf("blank alt text: stored %q, %d events; want neither", repo.stored, len(hub.events))
	}

	// Failed write: no event.
	hub, repo = &recordingHub{}, &altTextRepo{err: errors.New("db down")}
	(&PostAttachmentsHandler{repo: repo}).WithEventHub(hub).storeGeneratedAltText(ctx, "p1", "a1", "A red pixel")
	if len(hub.events) != 0 {
		t.Errorf("failed write published %d events, want 0", len(hub.events))
	}
}

func TestPublishAttachmentsChanged_NilHub(t *testing.T) {
	// The default handler has no hub; publishing must be a no-op, not a panic.
	(&PostAttachmentsHandler{}).publishAttachmentsChanged(tenantctx.With(context.Background(), "tn-1"), "p1", "a1", attachmentActionDeleted, attachmentSourceEditor)
}
