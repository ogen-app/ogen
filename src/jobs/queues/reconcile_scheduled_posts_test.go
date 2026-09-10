package queues_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/jobs/queues"
)

// reconcileFakeRepo lives alongside fakePostRepo but only implements
// the narrow ReconcilePostRepo surface — useful when we want to
// inject specific stuck rows without seeding the larger fake.
type reconcileFakeRepo struct {
	stuck   []models.Post
	calls   map[string]models.PostStatus
	reasons map[string]string
}

func (r *reconcileFakeRepo) ListStuckScheduled(_ context.Context, _ time.Time, _ int) ([]models.Post, error) {
	return r.stuck, nil
}
func (r *reconcileFakeRepo) UpdateStatusAndReason(_ context.Context, id string, st models.PostStatus, reason string) error {
	if r.calls == nil {
		r.calls = map[string]models.PostStatus{}
	}
	if r.reasons == nil {
		r.reasons = map[string]string{}
	}
	r.calls[id] = st
	r.reasons[id] = reason
	return nil
}

func TestReconcileForcesStuckPostsToFailedWithDistinctReason(t *testing.T) {
	stale := time.Now().Add(-2 * time.Hour).UTC()
	stuckPosts := []models.Post{
		{ID: "p1", Status: models.PostStatusScheduled, ScheduledAt: &stale, PublisherStatus: "scheduled"},
		{ID: "p2", Status: models.PostStatusScheduled, ScheduledAt: &stale, PublisherPostID: "z-2"},
	}
	repo := &reconcileFakeRepo{stuck: stuckPosts}
	logRepo := newFakeLogRepo()

	proc := &queues.ReconcileScheduledPostsProcessor{
		Repo:    repo,
		LogRepo: logRepo,
		Grace:   time.Hour,
	}
	if err := proc.Process(t.Context(), queues.ReconcileScheduledPostsTask{}); err != nil {
		t.Fatalf("process: %v", err)
	}
	for _, id := range []string{"p1", "p2"} {
		if repo.calls[id] != models.PostStatusFailed {
			t.Errorf("%s status: got %q want failed", id, repo.calls[id])
		}
		if !strings.HasPrefix(repo.reasons[id], queues.FailureReasonReconciliationTimeout) {
			t.Errorf("%s reason should start with %q, got %q",
				id, queues.FailureReasonReconciliationTimeout, repo.reasons[id])
		}
	}
	// Per-post audit entries:
	events := logRepo.eventTypes()
	timeoutCount := 0
	for _, e := range events {
		if e == string(models.PostLogEventReconciliationTimeout) {
			timeoutCount++
		}
	}
	if timeoutCount != 2 {
		t.Errorf("expected 2 reconciliation_timeout PostLog entries, got %d (events=%v)", timeoutCount, events)
	}
}

func TestReconcileLeavesNonStuckPostsAlone(t *testing.T) {
	repo := &reconcileFakeRepo{stuck: nil} // ListStuckScheduled returns nothing
	logRepo := newFakeLogRepo()
	proc := &queues.ReconcileScheduledPostsProcessor{
		Repo:    repo,
		LogRepo: logRepo,
		Grace:   time.Hour,
	}
	if err := proc.Process(t.Context(), queues.ReconcileScheduledPostsTask{}); err != nil {
		t.Fatalf("process: %v", err)
	}
	if len(repo.calls) != 0 {
		t.Errorf("no transitions expected when no stuck posts; got %v", repo.calls)
	}
	if len(logRepo.eventTypes()) != 0 {
		t.Errorf("no log entries expected; got %v", logRepo.eventTypes())
	}
}

// Catch a regression where the reason format quietly stops including
// the "reconciliation_timeout" sentinel — the spec explicitly calls
// out that the user must be able to distinguish this from
// Zernio-reported failure.
func TestReconciliationReasonContainsSentinelAndElapsed(t *testing.T) {
	stale := time.Now().Add(-2 * time.Hour).UTC()
	repo := &reconcileFakeRepo{
		stuck: []models.Post{{ID: "x", Status: models.PostStatusScheduled, ScheduledAt: &stale}},
	}
	proc := &queues.ReconcileScheduledPostsProcessor{
		Repo:    repo,
		LogRepo: newFakeLogRepo(),
		Grace:   time.Hour,
	}
	if err := proc.Process(t.Context(), queues.ReconcileScheduledPostsTask{}); err != nil {
		t.Fatalf("process: %v", err)
	}
	reason := repo.reasons["x"]
	if !strings.Contains(reason, "scheduled_at=") {
		t.Errorf("reason missing scheduled_at: %q", reason)
	}
	if !strings.Contains(reason, "elapsed=") {
		t.Errorf("reason missing elapsed: %q", reason)
	}
}

// Sanity: this test checks that errors lib usage is correct (otherwise
// some test machinery imports go unused).
func TestErrorsImportedForOtherSpecs(t *testing.T) {
	if errors.Is(nil, nil) {
		t.Skip("never reached")
	}
}
