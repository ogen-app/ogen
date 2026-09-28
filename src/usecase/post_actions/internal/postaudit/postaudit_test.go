package postaudit

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/eventhub"
)

func TestCreatorFor(t *testing.T) {
	tests := map[string]string{
		TriggerAssistant: CreatorAssistant,
		TriggerAPI:       CreatorUser,
		"":               CreatorUser,
		"other":          CreatorUser,
	}
	for trigger, want := range tests {
		if got := CreatorFor(trigger); got != want {
			t.Errorf("CreatorFor(%q) = %q, want %q", trigger, got, want)
		}
	}
}

func TestNotFound(t *testing.T) {
	sentinel := errors.New("missing")
	if got := NotFound(sql.ErrNoRows, sentinel); got != sentinel {
		t.Errorf("ErrNoRows: got %v, want sentinel", got)
	}
	wrapped := errors.Join(errors.New("ctx"), sql.ErrNoRows)
	if got := NotFound(wrapped, sentinel); got != sentinel {
		t.Errorf("wrapped ErrNoRows: got %v, want sentinel", got)
	}
	other := errors.New("boom")
	if got := NotFound(other, sentinel); got != other {
		t.Errorf("other: got %v, want passthrough", got)
	}
}

func TestNewLog(t *testing.T) {
	entry, err := NewLog("p1", models.PostLogEventPostCloned, "u1", "summary", map[string]any{"adapted": true})
	if err != nil {
		t.Fatalf("NewLog: %v", err)
	}
	if entry.ID == "" || entry.PostID != "p1" || entry.Actor != "u1" || entry.Summary != "summary" ||
		entry.EventType != models.PostLogEventPostCloned {
		t.Fatalf("unexpected entry: %+v", entry)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(entry.Payload), &got); err != nil {
		t.Fatalf("payload not JSON: %v", err)
	}
	if got["adapted"] != true {
		t.Errorf("payload = %v", got)
	}
}

type recordingHub struct{ events []eventhub.Event }

func (h *recordingHub) Publish(_ context.Context, ev eventhub.Event) error {
	h.events = append(h.events, ev)
	return nil
}

func (h *recordingHub) Subscribe(context.Context, eventhub.SubscribeOpts) (<-chan eventhub.Event, func(), error) {
	return nil, func() {}, nil
}

func TestPublish(t *testing.T) {
	Publish(nil, "p1", "post.cloned", "u1", nil)

	h := &recordingHub{}
	Publish(h, "p1", "post.cloned", "u1", map[string]any{"k": 1})
	if len(h.events) != 1 {
		t.Fatalf("want 1 event, got %d", len(h.events))
	}
	ev := h.events[0]
	if ev.ID == "" || ev.Topic != "entity:post:p1" || ev.Type != "post.cloned" || ev.UserID != "u1" {
		t.Errorf("unexpected event: %+v", ev)
	}
}
