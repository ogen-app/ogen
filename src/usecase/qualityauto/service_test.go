package qualityauto

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
)

type scheduled struct {
	tenantID, postID string
	trigger          models.QualityTrigger
	due              time.Time
}

type fakeQueue struct {
	mu        sync.Mutex
	scheduled []scheduled
	claim     []repository.QueuedAssessment
	finished  []string
	retried   map[string]time.Time
}

func (f *fakeQueue) Schedule(_ context.Context, tenantID, postID string, trigger models.QualityTrigger, due time.Time) error {
	f.scheduled = append(f.scheduled, scheduled{tenantID, postID, trigger, due})
	return nil
}

func (f *fakeQueue) Claim(context.Context, time.Time, int, time.Duration) ([]repository.QueuedAssessment, error) {
	return f.claim, nil
}

func (f *fakeQueue) Finish(_ context.Context, q repository.QueuedAssessment) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.finished = append(f.finished, q.PostID)
	return nil
}

func (f *fakeQueue) Retry(_ context.Context, q repository.QueuedAssessment, next time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.retried == nil {
		f.retried = map[string]time.Time{}
	}
	f.retried[q.PostID] = next
	return nil
}

type fakeTenants map[string]string

func (f fakeTenants) GetStatus(_ context.Context, id string) (string, error) {
	st, ok := f[id]
	if !ok {
		return "", sql.ErrNoRows
	}
	return st, nil
}

var now = time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)

func newService(q *fakeQueue, tenants TenantStatusReader, enabled bool) *Service {
	s := New(q, tenants, Config{Enabled: enabled, ReadyDelay: time.Minute, VersionDelay: 10 * time.Minute})
	s.now = func() time.Time { return now }
	return s
}

func TestScheduleDelays(t *testing.T) {
	q := &fakeQueue{}
	s := newService(q, nil, true)
	post := &models.Post{ID: "p1", TenantScoped: models.TenantScoped{TenantID: "t1"}, Status: models.PostStatusDraft}

	s.Schedule(t.Context(), post, models.QualityTriggerReady)
	s.Schedule(t.Context(), post, models.QualityTriggerVersion)
	s.Schedule(t.Context(), post, models.QualityTriggerScheduled)

	want := []time.Duration{time.Minute, 10 * time.Minute, 0}
	if len(q.scheduled) != len(want) {
		t.Fatalf("scheduled %d, want %d", len(q.scheduled), len(want))
	}
	for i, d := range want {
		got := q.scheduled[i]
		if got.due != now.Add(d) || got.tenantID != "t1" || got.postID != "p1" {
			t.Errorf("schedule %d = %+v, want due %v", i, got, now.Add(d))
		}
	}
}

func TestScheduleSkips(t *testing.T) {
	t.Run("disabled", func(t *testing.T) {
		q := &fakeQueue{}
		newService(q, nil, false).Schedule(t.Context(), &models.Post{ID: "p"}, models.QualityTriggerReady)
		if len(q.scheduled) != 0 {
			t.Fatal("a disabled service queued an assessment")
		}
	})
	t.Run("version on a scheduled post", func(t *testing.T) {
		q := &fakeQueue{}
		newService(q, nil, true).Schedule(t.Context(), &models.Post{ID: "p", Status: models.PostStatusScheduled}, models.QualityTriggerVersion)
		if len(q.scheduled) != 0 {
			t.Fatal("a version of a frozen post was queued")
		}
	})
	t.Run("nil service", func(t *testing.T) {
		var s *Service
		s.Schedule(t.Context(), &models.Post{ID: "p"}, models.QualityTriggerReady)
		if err := s.Sweep(t.Context()); err != nil {
			t.Fatal(err)
		}
	})
}

func TestSweepSettlesEachOutcome(t *testing.T) {
	q := &fakeQueue{claim: []repository.QueuedAssessment{
		{PostID: "done", TenantID: "t1", Trigger: models.QualityTriggerReady, Attempts: 1},
		{PostID: "skipped", TenantID: "t1", Trigger: models.QualityTriggerVersion, Attempts: 1},
		{PostID: "failed-first", TenantID: "t1", Trigger: models.QualityTriggerReady, Attempts: 1},
		{PostID: "failed-last", TenantID: "t1", Trigger: models.QualityTriggerReady, Attempts: maxAttempts},
		{PostID: "suspended", TenantID: "t2", Trigger: models.QualityTriggerReady, Attempts: 1},
	}}
	s := newService(q, fakeTenants{"t1": models.TenantStatusActive, "t2": "suspended"}, true)

	var mu sync.Mutex
	ran := map[string]string{}
	s.SetRunner(func(ctx context.Context, postID string, _ models.QualityTrigger) (Outcome, error) {
		tid, _ := tenantctx.From(ctx)
		mu.Lock()
		ran[postID] = tid
		mu.Unlock()
		switch postID {
		case "skipped":
			return Skipped, errors.New("cap")
		case "failed-first", "failed-last":
			return Failed, errors.New("model down")
		}
		return Done, nil
	})

	if err := s.Sweep(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, ok := ran["suspended"]; ok {
		t.Error("ran an assessment for a suspended tenant")
	}
	if ran["done"] != "t1" {
		t.Errorf("run context tenant = %q, want t1", ran["done"])
	}
	if next, ok := q.retried["failed-first"]; !ok || next != now.Add(retryAfter) {
		t.Errorf("failed-first should retry at %v, got %v (%v)", now.Add(retryAfter), next, ok)
	}
	finished := map[string]bool{}
	for _, id := range q.finished {
		finished[id] = true
	}
	for _, id := range []string{"done", "skipped", "failed-last", "suspended"} {
		if !finished[id] {
			t.Errorf("%s should be finished; finished = %v", id, q.finished)
		}
	}
	if finished["failed-first"] {
		t.Error("failed-first should be retried, not finished")
	}
}

func TestSweepWithoutRunnerClaimsNothing(t *testing.T) {
	q := &fakeQueue{claim: []repository.QueuedAssessment{{PostID: "p"}}}
	if err := newService(q, nil, true).Sweep(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(q.finished) != 0 || len(q.retried) != 0 {
		t.Fatal("a sweep without a runner touched the queue")
	}
}
