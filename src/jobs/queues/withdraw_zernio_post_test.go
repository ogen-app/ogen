package queues_test

import (
	"context"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/publishers/zernio"
	"github.com/ogen-app/ogen/src/jobs/queues"
)

// countDeletes registers a DELETE /posts/<id> route answering status and
// returns its call counter.
func countDeletes(stub *stubZernio, id string, status int) *atomic.Int32 {
	var n atomic.Int32
	stub.handle("DELETE", "/posts/"+id, func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		if status == http.StatusNoContent {
			w.WriteHeader(status)
			return
		}
		writeJSON(w, status, map[string]string{"error": http.StatusText(status)})
	})
	return &n
}

func TestPollSupersededBySubmissionMakesNoZernioCall(t *testing.T) {
	// Scheduled, unscheduled, scheduled again: the first submission's poll
	// wakes to find the post holding a new Zernio post. It must not adopt it.
	stub := newStubZernio()
	defer stub.Close()
	var polled atomic.Int32
	stub.handle("GET", "/posts/z-2", func(w http.ResponseWriter, r *http.Request) {
		polled.Add(1)
		writeJSON(w, http.StatusOK, zernio.PostEnvelope{Post: zernio.Job{ID: "z-2", Status: zernio.JobStatusScheduled}})
	})
	deps, postRepo, logRepo := makeDeps(stub, nil)
	post := seedScheduledPost(postRepo)
	post.PublisherPostID = "z-2"
	postRepo.put(post)

	proc := &queues.PollZernioStatusProcessor{Deps: deps}
	err := proc.Process(t.Context(), queues.PollZernioStatusTask{PostID: post.ID, PublisherPostID: "z-1"})
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	if polled.Load() != 0 {
		t.Fatalf("superseded poll called Zernio %d times", polled.Load())
	}
	if !logRepo.hasSummary("poll exited: superseded by a newer submission") {
		t.Errorf("expected a superseded log, got %v", logRepo.eventTypes())
	}
}

func TestPollForCurrentSubmissionStillPolls(t *testing.T) {
	stub := newStubZernio()
	defer stub.Close()
	stub.handle("GET", "/posts/z-1", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, zernio.PostEnvelope{Post: zernio.Job{ID: "z-1", Status: zernio.JobStatusPublished}})
	})
	deps, postRepo, _ := makeDeps(stub, nil)
	post := seedScheduledPost(postRepo)
	post.PublisherPostID = "z-1"
	postRepo.put(post)

	proc := &queues.PollZernioStatusProcessor{Deps: deps}
	if err := proc.Process(t.Context(), queues.PollZernioStatusTask{PostID: post.ID, PublisherPostID: "z-1"}); err != nil {
		t.Fatalf("process: %v", err)
	}
	got, _ := postRepo.GetByID(t.Context(), post.ID)
	if got.Status != models.PostStatusPublished {
		t.Errorf("status: got %q want published", got.Status)
	}
}

func TestPollDoesNotRestoreAPostCancelledWhilePolling(t *testing.T) {
	// The cancel lands between the poll loading the post and writing Zernio's
	// answer back. The poll's stale copy must not put the post back on
	// scheduled.
	stub := newStubZernio()
	defer stub.Close()
	deps, postRepo, _ := makeDeps(stub, nil)
	post := seedScheduledPost(postRepo)
	post.PublisherPostID = "z-1"
	postRepo.put(post)
	stub.handle("GET", "/posts/z-1", func(w http.ResponseWriter, r *http.Request) {
		cancelled := *post
		cancelled.Status = models.PostStatusReadyForPublish
		cancelled.PublisherPostID = ""
		postRepo.put(&cancelled)
		writeJSON(w, http.StatusOK, zernio.PostEnvelope{Post: zernio.Job{ID: "z-1", Status: zernio.JobStatusScheduled}})
	})

	proc := &queues.PollZernioStatusProcessor{Deps: deps}
	if err := proc.Process(t.Context(), queues.PollZernioStatusTask{PostID: post.ID, PublisherPostID: "z-1"}); err != nil {
		t.Fatalf("process: %v", err)
	}
	got, _ := postRepo.GetByID(t.Context(), post.ID)
	if got.Status != models.PostStatusReadyForPublish || got.PublisherPostID != "" {
		t.Errorf("poll restored the cancelled post: status %q publisher_post_id %q", got.Status, got.PublisherPostID)
	}
}

func TestSubmitWithdrawsWhenPostUnscheduledMidFlight(t *testing.T) {
	// The user unschedules while the submit is uploading; the cancel finds no
	// Zernio id yet and lands locally. The Zernio post the submit then creates
	// must be withdrawn, not attached to the unscheduled post.
	stub := newStubZernio()
	defer stub.Close()
	deps, postRepo, _ := makeDeps(stub, map[string][]models.SocialAccount{
		"p_test": {{ID: "acc-1", Platform: "linkedin"}},
	})
	post := seedScheduledPost(postRepo)
	stub.handle("POST", "/posts", func(w http.ResponseWriter, r *http.Request) {
		unscheduled := *post
		unscheduled.Status = models.PostStatusReadyForPublish
		postRepo.put(&unscheduled)
		writeJSON(w, http.StatusCreated, zernio.PostEnvelope{Post: zernio.Job{ID: "z-1", Status: zernio.JobStatusScheduled}})
	})
	deletes := countDeletes(stub, "z-1", http.StatusNoContent)

	proc := &queues.SubmitPostProcessor{Deps: deps}
	if err := proc.Process(t.Context(), queues.SubmitPostTask{PostID: post.ID}); err != nil {
		t.Fatalf("process: %v", err)
	}
	got, _ := postRepo.GetByID(t.Context(), post.ID)
	if got.Status != models.PostStatusReadyForPublish || got.PublisherPostID != "" {
		t.Errorf("submit resurrected the post: status %q publisher_post_id %q", got.Status, got.PublisherPostID)
	}
	if deletes.Load() != 1 {
		t.Errorf("Zernio DELETE calls: got %d want 1", deletes.Load())
	}
}

func TestSubmitWithdrawsWhenPostDeletedMidFlight(t *testing.T) {
	stub := newStubZernio()
	defer stub.Close()
	deps, postRepo, _ := makeDeps(stub, map[string][]models.SocialAccount{
		"p_test": {{ID: "acc-1", Platform: "linkedin"}},
	})
	post := seedScheduledPost(postRepo)
	stub.handle("POST", "/posts", func(w http.ResponseWriter, r *http.Request) {
		postRepo.remove(post.ID)
		writeJSON(w, http.StatusCreated, zernio.PostEnvelope{Post: zernio.Job{ID: "z-1", Status: zernio.JobStatusScheduled}})
	})
	deletes := countDeletes(stub, "z-1", http.StatusNoContent)

	proc := &queues.SubmitPostProcessor{Deps: deps}
	if err := proc.Process(t.Context(), queues.SubmitPostTask{PostID: post.ID}); err != nil {
		t.Fatalf("process: %v", err)
	}
	if deletes.Load() != 1 {
		t.Errorf("Zernio DELETE calls: got %d want 1", deletes.Load())
	}
}

func TestSubmitTerminalDoesNotFailAnUnscheduledPost(t *testing.T) {
	stub := newStubZernio()
	defer stub.Close()
	deps, postRepo, _ := makeDeps(stub, map[string][]models.SocialAccount{
		"p_test": {{ID: "acc-1", Platform: "linkedin"}},
	})
	post := seedScheduledPost(postRepo)
	stub.handle("POST", "/posts", func(w http.ResponseWriter, r *http.Request) {
		unscheduled := *post
		unscheduled.Status = models.PostStatusDraft
		postRepo.put(&unscheduled)
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "platforms required"})
	})

	proc := &queues.SubmitPostProcessor{Deps: deps}
	if err := proc.Process(t.Context(), queues.SubmitPostTask{PostID: post.ID}); err != nil {
		t.Fatalf("process: %v", err)
	}
	got, _ := postRepo.GetByID(t.Context(), post.ID)
	if got.Status != models.PostStatusDraft {
		t.Errorf("status: got %q want draft (the user's choice)", got.Status)
	}
}

func TestWithdrawOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		wantErr bool
	}{
		{"deleted", http.StatusNoContent, false},
		{"already gone", http.StatusNotFound, false},
		{"already published", http.StatusConflict, false},
		{"rejected", http.StatusBadRequest, false},
		{"transient", http.StatusBadGateway, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub := newStubZernio()
			defer stub.Close()
			deletes := countDeletes(stub, "z-1", tc.status)
			deps, _, _ := makeDeps(stub, nil)

			proc := &queues.WithdrawZernioPostProcessor{Deps: deps}
			err := proc.Process(t.Context(), queues.WithdrawZernioPostTask{
				PublisherPostID: "z-1", PostID: "post-1", TenantID: "t1", Reason: queues.WithdrawReasonPostDeleted,
			})
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if deletes.Load() != 1 {
				t.Errorf("Zernio DELETE calls: got %d want 1", deletes.Load())
			}
		})
	}
}

func TestReconcileWithdrawsTimedOutSubmissions(t *testing.T) {
	stale := time.Now().Add(-2 * time.Hour).UTC()
	repo := &reconcileFakeRepo{stuck: []models.Post{
		{ID: "p1", Status: models.PostStatusScheduled, ScheduledAt: &stale},
		{ID: "p2", Status: models.PostStatusScheduled, ScheduledAt: &stale, PublisherPostID: "z-2"},
	}}
	var queued []queues.WithdrawZernioPostTask
	proc := &queues.ReconcileScheduledPostsProcessor{
		Repo: repo, LogRepo: newFakeLogRepo(), Grace: time.Hour,
		Withdraw: func(_ context.Context, task queues.WithdrawZernioPostTask) error {
			queued = append(queued, task)
			return nil
		},
	}
	if err := proc.Process(t.Context(), queues.ReconcileScheduledPostsTask{}); err != nil {
		t.Fatalf("process: %v", err)
	}
	if len(queued) != 1 || queued[0].PublisherPostID != "z-2" || queued[0].Reason != queues.WithdrawReasonReconcile {
		t.Fatalf("withdrawals = %+v, want one for z-2", queued)
	}
}

func TestOrphanSweep(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	old, young := now.Add(-3*time.Hour), now.Add(-10*time.Minute)
	queued := []map[string]any{
		{"_id": "z-orphan", "status": "scheduled", "createdAt": old},  // no Ogen post holds it
		{"_id": "z-draft", "status": "scheduled", "createdAt": old},   // held by an unscheduled post
		{"_id": "z-live", "status": "scheduled", "createdAt": old},    // held by a scheduled post
		{"_id": "z-young", "status": "scheduled", "createdAt": young}, // submit may still be in flight
		{"_id": "z-undated", "status": "scheduled"},                   // age unknown
	}
	for _, live := range []bool{false, true} {
		name := "dry run"
		if live {
			name = "live"
		}
		t.Run(name, func(t *testing.T) {
			stub := newStubZernio()
			defer stub.Close()
			stub.handle("GET", "/posts", func(w http.ResponseWriter, r *http.Request) {
				if q := r.URL.Query(); q.Get("profileId") != "p_test" || q.Get("status") != "scheduled" {
					t.Errorf("list query = %v", q)
				}
				writeJSON(w, http.StatusOK, map[string]any{
					"posts":      queued,
					"pagination": map[string]int{"page": 1, "pages": 1},
				})
			})
			var (
				mu      sync.Mutex
				deleted []string
			)
			for _, j := range queued {
				id := j["_id"].(string)
				stub.handle("DELETE", "/posts/"+id, func(w http.ResponseWriter, r *http.Request) {
					mu.Lock()
					deleted = append(deleted, id)
					mu.Unlock()
					w.WriteHeader(http.StatusNoContent)
				})
			}
			deps, postRepo, _ := makeDeps(stub, map[string][]models.SocialAccount{
				"p_test": {{ID: "acc-1", Platform: "linkedin", TenantScoped: models.TenantScoped{TenantID: "t1"}}},
			})
			postRepo.put(&models.Post{ID: "a", Status: models.PostStatusDraft, PublisherPostID: "z-draft"})
			postRepo.put(&models.Post{ID: "b", Status: models.PostStatusScheduled, PublisherPostID: "z-live"})

			proc := &queues.SweepZernioOrphansProcessor{
				Deps:   deps,
				Config: queues.OrphanSweepConfig{Live: live, MinAge: time.Hour},
				Now:    func() time.Time { return now },
			}
			if err := proc.Process(t.Context(), queues.SweepZernioOrphansTask{}); err != nil {
				t.Fatalf("process: %v", err)
			}
			mu.Lock()
			defer mu.Unlock()
			slices.Sort(deleted)
			want := []string{"z-draft", "z-orphan"}
			if !live {
				want = nil
			}
			if !slices.Equal(deleted, want) {
				t.Errorf("withdrawn = %v, want %v", deleted, want)
			}
		})
	}
}
