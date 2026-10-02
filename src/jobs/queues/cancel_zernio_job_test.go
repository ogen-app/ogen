package queues_test

import (
	"net/http"
	"testing"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/jobs/queues"
)

func TestCancelHappyPathTransitionsToReadyForPublish(t *testing.T) {
	stub := newStubZernio()
	defer stub.Close()
	stub.handle("DELETE", "/posts/z-1", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	deps, postRepo, _ := makeDeps(stub, nil)
	post := seedScheduledPost(postRepo)
	post.PublisherPostID = "z-1"
	postRepo.put(post)

	proc := &queues.CancelZernioJobProcessor{Deps: deps}
	err := proc.Process(t.Context(), queues.CancelZernioJobTask{
		PostID: post.ID, Target: queues.CancelTargetReadyForPublish, Actor: "user-1",
	})
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	got, _ := postRepo.GetByID(t.Context(), post.ID)
	if got.Status != models.PostStatusReadyForPublish {
		t.Errorf("status: got %q want ready_for_publish", got.Status)
	}
}

func TestCancelHappyPathTransitionsToDraft(t *testing.T) {
	stub := newStubZernio()
	defer stub.Close()
	stub.handle("DELETE", "/posts/z-1", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	deps, postRepo, _ := makeDeps(stub, nil)
	post := seedScheduledPost(postRepo)
	post.PublisherPostID = "z-1"
	postRepo.put(post)

	proc := &queues.CancelZernioJobProcessor{Deps: deps}
	_ = proc.Process(t.Context(), queues.CancelZernioJobTask{
		PostID: post.ID, Target: queues.CancelTargetDraft, Actor: "user-1",
	})
	got, _ := postRepo.GetByID(t.Context(), post.ID)
	if got.Status != models.PostStatusDraft {
		t.Errorf("status: got %q want draft", got.Status)
	}
}

func TestCancelConvertsToManualPublishing(t *testing.T) {
	// The convert-to-manual target cancels the Zernio job and
	// lands the post directly on scheduled_for_manual_publishing, keeping
	// scheduled_at so the intended date still shows.
	stub := newStubZernio()
	defer stub.Close()
	stub.handle("DELETE", "/posts/z-1", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	deps, postRepo, logRepo := makeDeps(stub, nil)
	post := seedScheduledPost(postRepo)
	post.PublisherPostID = "z-1"
	postRepo.put(post)
	wantSchedAt := post.ScheduledAt

	proc := &queues.CancelZernioJobProcessor{Deps: deps}
	if err := proc.Process(t.Context(), queues.CancelZernioJobTask{
		PostID: post.ID, Target: queues.CancelTargetManualPublish, Actor: "user-1",
	}); err != nil {
		t.Fatalf("process: %v", err)
	}
	got, _ := postRepo.GetByID(t.Context(), post.ID)
	if got.Status != models.PostStatusScheduledForManualPublish {
		t.Errorf("status: got %q want scheduled_for_manual_publishing", got.Status)
	}
	if got.ScheduledAt == nil || wantSchedAt == nil || !got.ScheduledAt.Equal(*wantSchedAt) {
		t.Errorf("scheduled_at must be preserved: got %v want %v", got.ScheduledAt, wantSchedAt)
	}
	// The audit trail describes the intent (convert), not just a cancel,
	// and is a single direct transition — no ready_for_publish hop.
	if !logRepo.hasTransitionTo(models.PostStatusScheduledForManualPublish) {
		t.Errorf("expected a state_transition to scheduled_for_manual_publishing")
	}
	if logRepo.hasTransitionTo(models.PostStatusReadyForPublish) {
		t.Errorf("conversion must not detour through ready_for_publish")
	}
}

func TestCancelRaceWithPublishKeepsPostScheduled(t *testing.T) {
	// Zernio reports already-published; we MUST NOT transition the
	// post locally — the next poll tick will land Published.
	stub := newStubZernio()
	defer stub.Close()
	stub.handle("DELETE", "/posts/z-1", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "already published"})
	})
	deps, postRepo, _ := makeDeps(stub, nil)
	post := seedScheduledPost(postRepo)
	post.PublisherPostID = "z-1"
	postRepo.put(post)

	proc := &queues.CancelZernioJobProcessor{Deps: deps}
	if err := proc.Process(t.Context(), queues.CancelZernioJobTask{
		PostID: post.ID, Target: queues.CancelTargetReadyForPublish, Actor: "user-1",
	}); err != nil {
		t.Fatalf("process should swallow race: %v", err)
	}
	got, _ := postRepo.GetByID(t.Context(), post.ID)
	if got.Status != models.PostStatusScheduled {
		t.Errorf("status: got %q want scheduled (race resolves via poll)", got.Status)
	}
	if got.PublisherPostID != "z-1" {
		t.Errorf("publisher_post_id: got %q want z-1 kept for the poll", got.PublisherPostID)
	}
}

func TestCancelClearsPublisherIdentity(t *testing.T) {
	// The Zernio DELETE removes the post, so the id it leaves behind is dead:
	// a later schedule must create a fresh post, not retry this one.
	for _, target := range []queues.CancelTarget{
		queues.CancelTargetReadyForPublish, queues.CancelTargetDraft, queues.CancelTargetManualPublish,
	} {
		t.Run(string(target), func(t *testing.T) {
			stub := newStubZernio()
			defer stub.Close()
			stub.handle("DELETE", "/posts/z-1", func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			})
			deps, postRepo, _ := makeDeps(stub, nil)
			post := seedScheduledPost(postRepo)
			post.PublisherPostID = "z-1"
			post.PublisherStatus = "scheduled"
			postRepo.put(post)

			proc := &queues.CancelZernioJobProcessor{Deps: deps}
			if err := proc.Process(t.Context(), queues.CancelZernioJobTask{
				PostID: post.ID, Target: target, Actor: "user-1",
			}); err != nil {
				t.Fatalf("process: %v", err)
			}
			got, _ := postRepo.GetByID(t.Context(), post.ID)
			if got.PublisherPostID != "" || got.PublisherStatus != "" {
				t.Errorf("publisher identity: got id=%q status=%q, want both cleared", got.PublisherPostID, got.PublisherStatus)
			}
		})
	}
}

func TestCancelPostGoneOnZernioTransitionsLocally(t *testing.T) {
	// A 404 means the Zernio post no longer exists — nothing will publish,
	// so the post must not sit in Scheduled waiting for a poll.
	stub := newStubZernio()
	defer stub.Close()
	stub.handle("DELETE", "/posts/z-stale", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "Post not found"})
	})
	deps, postRepo, _ := makeDeps(stub, nil)
	post := seedScheduledPost(postRepo)
	post.PublisherPostID = "z-stale"
	postRepo.put(post)

	proc := &queues.CancelZernioJobProcessor{Deps: deps}
	if err := proc.Process(t.Context(), queues.CancelZernioJobTask{
		PostID: post.ID, Target: queues.CancelTargetDraft, Actor: "user-1",
	}); err != nil {
		t.Fatalf("process: %v", err)
	}
	got, _ := postRepo.GetByID(t.Context(), post.ID)
	if got.Status != models.PostStatusDraft {
		t.Errorf("status: got %q want draft", got.Status)
	}
	if got.PublisherPostID != "" {
		t.Errorf("publisher_post_id: got %q want cleared", got.PublisherPostID)
	}
}
