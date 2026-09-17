package queues_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/eventhub"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/jobs/queues"
	"github.com/ogen-app/ogen/src/usecase/notify"
)

// dueLister returns a fixed set of overdue manual-publish posts.
type dueLister struct{ posts []models.Post }

func (d dueLister) ListManualPublishDue(context.Context, time.Time, int) ([]models.Post, error) {
	return d.posts, nil
}

// ownersByTenant returns configured owners per tenant id.
type ownersByTenant struct{ owners map[string][]models.User }

func (o ownersByTenant) ListOwnersByTenant(_ context.Context, tenantID string) ([]models.User, error) {
	return o.owners[tenantID], nil
}

// capturingNotifRepo records every inserted notification for assertions and
// always reports a fresh insert.
type capturingNotifRepo struct {
	mu   sync.Mutex
	rows []*models.Notification
}

func (r *capturingNotifRepo) Insert(_ context.Context, n *models.Notification) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rows = append(r.rows, n)
	return true, nil
}
func (r *capturingNotifRepo) List(context.Context, string, repository.NotificationListOpts) ([]models.Notification, error) {
	return nil, nil
}
func (r *capturingNotifRepo) ReplaySince(context.Context, string, int64, int) ([]models.Notification, error) {
	return nil, nil
}
func (r *capturingNotifRepo) UnreadCount(context.Context, string) (int, error) { return 0, nil }
func (r *capturingNotifRepo) Get(context.Context, string, string) (*models.Notification, error) {
	return nil, nil
}
func (r *capturingNotifRepo) SetRead(context.Context, string, string, bool) (bool, error) {
	return false, nil
}
func (r *capturingNotifRepo) MarkAllRead(context.Context, string, int64) (int, error) { return 0, nil }
func (r *capturingNotifRepo) Dismiss(context.Context, string, string) (bool, error)   { return false, nil }
func (r *capturingNotifRepo) DeleteExpired(context.Context, time.Time, time.Duration) (int64, error) {
	return 0, nil
}

// TestManualPublishDueSweep_NotifiesOwnersPerPost checks each overdue post
// notifies its tenant's owners once, with the right type + per-post dedupe key,
// and that owners of another tenant are not cross-notified.
func TestManualPublishDueSweep_NotifiesOwnersPerPost(t *testing.T) {
	posts := []models.Post{
		{ID: "p1", TenantScoped: models.TenantScoped{TenantID: "t1"}, PlatformID: "linkedin"},
		{ID: "p2", TenantScoped: models.TenantScoped{TenantID: "t1"}, PlatformID: "x"},
		{ID: "p3", TenantScoped: models.TenantScoped{TenantID: "t2"}, PlatformID: "instagram"},
	}
	owners := map[string][]models.User{
		"t1": {{ID: "owner_a"}, {ID: "owner_b"}},
		"t2": {{ID: "owner_c"}},
	}
	repo := &capturingNotifRepo{}
	proc := &queues.DetectManualPublishDueProcessor{
		Posts:    dueLister{posts: posts},
		Users:    ownersByTenant{owners: owners},
		Notifier: notify.New(repo, eventhub.New(eventhub.Config{})),
	}

	if err := proc.Process(context.Background(), queues.DetectManualPublishDueTask{}); err != nil {
		t.Fatalf("process: %v", err)
	}

	// t1: 2 posts × 2 owners = 4 rows; t2: 1 post × 1 owner = 1 row.
	if len(repo.rows) != 5 {
		t.Fatalf("want 5 notifications, got %d", len(repo.rows))
	}
	for _, n := range repo.rows {
		if n.Type != "post.manual_publish_due" {
			t.Fatalf("wrong type: %q", n.Type)
		}
		if n.DedupeKey != "manual_publish:"+n.EntityID {
			t.Fatalf("wrong dedupe key %q for %q", n.DedupeKey, n.EntityID)
		}
	}
	// p3 (t2) must only ever reach owner_c.
	for _, n := range repo.rows {
		if n.EntityID == "p3" && n.UserID != "owner_c" {
			t.Fatalf("t2 post p3 leaked to %q", n.UserID)
		}
	}
}

// TestManualPublishDueSweep_Empty is a no-op with no due posts.
func TestManualPublishDueSweep_Empty(t *testing.T) {
	repo := &capturingNotifRepo{}
	proc := &queues.DetectManualPublishDueProcessor{
		Posts:    dueLister{},
		Users:    ownersByTenant{owners: map[string][]models.User{}},
		Notifier: notify.New(repo, eventhub.New(eventhub.Config{})),
	}
	if err := proc.Process(context.Background(), queues.DetectManualPublishDueTask{}); err != nil {
		t.Fatalf("process: %v", err)
	}
	if len(repo.rows) != 0 {
		t.Fatalf("want 0 notifications, got %d", len(repo.rows))
	}
}
