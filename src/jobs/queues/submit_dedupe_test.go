package queues_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/jobs/queues"
)

// zernioPost is the raw JSON shape of a Zernio job as FindByContent decodes it
// (the client's listEnvelope/Job are unexported, so tests build the map).
func zernioListWith(jobs ...map[string]any) map[string]any {
	posts := make([]any, 0, len(jobs))
	for _, j := range jobs {
		posts = append(posts, j)
	}
	return map[string]any{
		"posts":      posts,
		"pagination": map[string]any{"page": 1, "limit": 100, "total": len(jobs), "pages": 1},
	}
}

// on a 409 dedupe, a still-pending earlier job (scheduled) is adopted.
func TestSubmitDedupeAdoptsPendingJob(t *testing.T) {
	stub := newStubZernio()
	defer stub.Close()
	stub.handle("POST", "/posts", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "duplicate content"})
	})
	stub.handle("GET", "/posts", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, zernioListWith(
			map[string]any{"_id": "z-adopt", "status": "scheduled", "content": "hello world"},
		))
	})

	deps, postRepo, _ := makeDeps(stub, map[string][]models.SocialAccount{
		"p_test": {{ID: "acc-1", Platform: "linkedin"}},
	})
	post := seedScheduledPost(postRepo)

	proc := &queues.SubmitPostProcessor{Deps: deps}
	if err := proc.Process(t.Context(), queues.SubmitPostTask{PostID: post.ID}); err != nil {
		t.Fatalf("process: %v", err)
	}
	got, _ := postRepo.GetByID(t.Context(), post.ID)
	if got.PublisherPostID != "z-adopt" {
		t.Errorf("pending match should be adopted: publisher_post_id = %q want z-adopt", got.PublisherPostID)
	}
	if got.Status == models.PostStatusFailed {
		t.Errorf("adopting a pending job must not fail the post")
	}
}

// a terminal earlier job (published) is NOT adopted; the post fails with the
// cause named — never zernio_dedupe_unrecoverable.
func TestSubmitDedupeTerminalMatchFailsWithCause(t *testing.T) {
	stub := newStubZernio()
	defer stub.Close()
	stub.handle("POST", "/posts", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "duplicate content"})
	})
	stub.handle("GET", "/posts", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, zernioListWith(
			map[string]any{"_id": "z-pub", "status": "published", "content": "hello world"},
		))
	})

	deps, postRepo, _ := makeDeps(stub, map[string][]models.SocialAccount{
		"p_test": {{ID: "acc-1", Platform: "linkedin"}},
	})
	post := seedScheduledPost(postRepo)

	proc := &queues.SubmitPostProcessor{Deps: deps}
	if err := proc.Process(t.Context(), queues.SubmitPostTask{PostID: post.ID}); err != nil {
		t.Fatalf("process should swallow terminal err: %v", err)
	}
	got, _ := postRepo.GetByID(t.Context(), post.ID)
	if got.Status != models.PostStatusFailed {
		t.Fatalf("status: got %q want failed", got.Status)
	}
	if got.PublisherPostID != "" {
		t.Errorf("a finished job must not be adopted: publisher_post_id = %q", got.PublisherPostID)
	}
	if !strings.Contains(got.FailureReason, "zernio_duplicate_content") || !strings.Contains(got.FailureReason, "published") {
		t.Errorf("failure_reason should name the cause + status, got %q", got.FailureReason)
	}
	if strings.Contains(got.FailureReason, "dedupe_unrecoverable") {
		t.Errorf("must not use the old zernio_dedupe_unrecoverable reason: %q", got.FailureReason)
	}
}

// no matching job found: the post still fails with an actionable message, not
// the cryptic zernio_dedupe_unrecoverable.
func TestSubmitDedupeNoMatchFailsWithCause(t *testing.T) {
	stub := newStubZernio()
	defer stub.Close()
	stub.handle("POST", "/posts", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "duplicate content"})
	})
	stub.handle("GET", "/posts", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, zernioListWith()) // no posts
	})

	deps, postRepo, _ := makeDeps(stub, map[string][]models.SocialAccount{
		"p_test": {{ID: "acc-1", Platform: "linkedin"}},
	})
	post := seedScheduledPost(postRepo)

	proc := &queues.SubmitPostProcessor{Deps: deps}
	if err := proc.Process(t.Context(), queues.SubmitPostTask{PostID: post.ID}); err != nil {
		t.Fatalf("process should swallow terminal err: %v", err)
	}
	got, _ := postRepo.GetByID(t.Context(), post.ID)
	if got.Status != models.PostStatusFailed {
		t.Fatalf("status: got %q want failed", got.Status)
	}
	if !strings.Contains(got.FailureReason, "zernio_duplicate_content") {
		t.Errorf("failure_reason should name the duplicate cause, got %q", got.FailureReason)
	}
	if strings.Contains(got.FailureReason, "dedupe_unrecoverable") {
		t.Errorf("must not use the old zernio_dedupe_unrecoverable reason: %q", got.FailureReason)
	}
}
