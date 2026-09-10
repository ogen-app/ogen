package notify

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/eventhub"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
)

// fakeRepo is an in-memory NotificationRepository for DB-free service tests.
type fakeRepo struct {
	mu         sync.Mutex
	rows       []*models.Notification
	seq        int64
	inserted   int
	failInsert bool
	collide    bool // Insert reports "swallowed duplicate" (inserted=false, nil err)
}

func (f *fakeRepo) Insert(_ context.Context, n *models.Notification) (bool, error) {
	if f.failInsert {
		return false, errors.New("boom")
	}
	if f.collide {
		return false, nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	n.Seq = f.seq
	n.CreatedAt = time.Now().UTC()
	f.rows = append(f.rows, n)
	f.inserted++
	return true, nil
}

func (f *fakeRepo) List(context.Context, string, repository.NotificationListOpts) ([]models.Notification, error) {
	return nil, nil
}
func (f *fakeRepo) ReplaySince(context.Context, string, int64, int) ([]models.Notification, error) {
	return nil, nil
}
func (f *fakeRepo) UnreadCount(context.Context, string) (int, error) { return 0, nil }
func (f *fakeRepo) Get(context.Context, string, string) (*models.Notification, error) {
	return nil, nil
}
func (f *fakeRepo) SetRead(context.Context, string, string, bool) (bool, error) { return false, nil }
func (f *fakeRepo) MarkAllRead(context.Context, string, int64) (int, error)     { return 0, nil }
func (f *fakeRepo) Dismiss(context.Context, string, string) (bool, error)       { return false, nil }
func (f *fakeRepo) DeleteExpired(context.Context, time.Time, time.Duration) (int64, error) {
	return 0, nil
}

// tenantCtx returns a context carrying a tenant, as every producer's Emit ctx
// must (the hub derives the event tenant from it).
func tenantCtx(tid string) context.Context {
	return tenantctx.With(context.Background(), tid)
}

func TestEmit_InsertsAndPublishesToStream(t *testing.T) {
	hub := eventhub.New(eventhub.Config{})
	repo := &fakeRepo{}
	svc := New(repo, hub)

	ch, unsub, err := hub.Subscribe(t.Context(), eventhub.SubscribeOpts{
		UserID:   "u1",
		TenantID: "t1",
		Topics:   []string{StreamTopic},
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer unsub()

	err = svc.Emit(tenantCtx("t1"), "u1", Spec{
		Level: models.NotificationLevelError,
		Type:  "post.publish_failed",
		Title: "Post failed to publish",
	})
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	if repo.inserted != 1 {
		t.Fatalf("want 1 insert, got %d", repo.inserted)
	}

	select {
	case ev := <-ch:
		n, ok := ev.Payload.(*models.Notification)
		if !ok {
			t.Fatalf("payload type = %T, want *models.Notification", ev.Payload)
		}
		if n.UserID != "u1" || n.Type != "post.publish_failed" {
			t.Fatalf("unexpected payload: %+v", n)
		}
		if n.Seq == 0 {
			t.Fatalf("seq not populated from insert")
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for live notification")
	}
}

func TestEmit_DedupeCollisionDoesNotPublish(t *testing.T) {
	hub := eventhub.New(eventhub.Config{})
	repo := &fakeRepo{collide: true}
	svc := New(repo, hub)

	ch, unsub, err := hub.Subscribe(t.Context(), eventhub.SubscribeOpts{
		UserID: "u1", TenantID: "t1", Topics: []string{StreamTopic},
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer unsub()

	if err := svc.Emit(tenantCtx("t1"), "u1", Spec{Type: "x", Title: "y", DedupeKey: "k"}); err != nil {
		t.Fatalf("emit: %v", err)
	}
	select {
	case ev := <-ch:
		t.Fatalf("collapsed duplicate should not publish, got %+v", ev)
	case <-time.After(100 * time.Millisecond):
		// expected: nothing delivered
	}
}

func TestEmit_DefaultsInvalidLevelToInfo(t *testing.T) {
	repo := &fakeRepo{}
	svc := New(repo, nil) // nil hub: publish is skipped, insert still happens

	if err := svc.Emit(tenantCtx("t1"), "u1", Spec{Level: "bogus", Type: "x", Title: "y"}); err != nil {
		t.Fatalf("emit: %v", err)
	}
	if len(repo.rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(repo.rows))
	}
	if repo.rows[0].Level != models.NotificationLevelInfo {
		t.Fatalf("level = %q, want info", repo.rows[0].Level)
	}
}

func TestEmit_NilServiceAndEmptyUserAreNoOps(t *testing.T) {
	var svc *Service // nil receiver
	if err := svc.Emit(t.Context(), "u1", Spec{Type: "x"}); err != nil {
		t.Fatalf("nil service Emit: %v", err)
	}
	repo := &fakeRepo{}
	if err := New(repo, nil).Emit(tenantCtx("t1"), "", Spec{Type: "x"}); err != nil {
		t.Fatalf("empty user Emit: %v", err)
	}
	if repo.inserted != 0 {
		t.Fatalf("empty user should not insert, got %d", repo.inserted)
	}
}

func TestEmitToUsers_FanOutAndErrorAggregation(t *testing.T) {
	repo := &fakeRepo{}
	svc := New(repo, nil)
	if err := svc.EmitToUsers(tenantCtx("t1"), []string{"u1", "u2", "u3"}, Spec{Type: "x", Title: "y"}); err != nil {
		t.Fatalf("emitToUsers: %v", err)
	}
	if repo.inserted != 3 {
		t.Fatalf("want 3 rows (one per recipient), got %d", repo.inserted)
	}

	failing := New(&fakeRepo{failInsert: true}, nil)
	if err := failing.EmitToUsers(tenantCtx("t1"), []string{"u1"}, Spec{Type: "x"}); err == nil {
		t.Fatal("want error when insert fails, got nil")
	}
}
