package queues_test

import (
	"encoding/json"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/publishers/zernio"
	"github.com/ogen-app/ogen/src/jobs/queues"
)

// linkedinWithComments is LinkedIn taking a first comment of up to 1250 chars.
func linkedinWithComments() *models.Platform {
	return &models.Platform{
		ID: "AXqWG7U2qnpt", Name: "LinkedIn",
		TextConstraints: models.TextConstraints{MaxContentChars: 3000, MaxFirstCommentChars: 1250},
	}
}

// submitFirstComment submits a LinkedIn post carrying a first comment and
// returns the variant's platformSpecificData as sent.
func submitFirstComment(t *testing.T, platform *models.Platform, comment string, delay int) map[string]any {
	t.Helper()
	stub := newStubZernio()
	defer stub.Close()
	var body struct {
		Platforms []struct {
			Data map[string]any `json:"platformSpecificData"`
		} `json:"platforms"`
	}
	stub.handle("POST", "/posts", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		writeJSON(w, http.StatusCreated, zernio.PostEnvelope{Post: zernio.Job{ID: "z-1", Status: zernio.JobStatusScheduled}})
	})
	deps, postRepo, _ := makeDeps(stub, map[string][]models.SocialAccount{
		"p_test": {{ID: "acc-1", Platform: "linkedin"}},
	})
	post := seedScheduledPost(postRepo)
	post.Platform = platform
	post.PlatformPostType = "text-post"
	post.FirstComment = comment
	post.FirstCommentDelayMinutes = delay
	postRepo.put(post)

	proc := &queues.SubmitPostProcessor{Deps: deps}
	if err := proc.Process(t.Context(), queues.SubmitPostTask{PostID: post.ID}); err != nil {
		t.Fatalf("process: %v", err)
	}
	if len(body.Platforms) != 1 {
		t.Fatalf("platforms = %d, want 1", len(body.Platforms))
	}
	return body.Platforms[0].Data
}

func TestSubmitSendsUndelayedFirstComment(t *testing.T) {
	data := submitFirstComment(t, linkedinWithComments(), "Read it **here**: https://x.y", 0)
	// Flattened like the body: networks render no Markdown.
	if got := data["firstComment"]; got != "Read it here: https://x.y" {
		t.Errorf("platformSpecificData.firstComment = %v", got)
	}
}

func TestSubmitLeavesDelayedOrUnsupportedFirstCommentOut(t *testing.T) {
	if data := submitFirstComment(t, linkedinWithComments(), "later", 5); data != nil {
		t.Errorf("a delayed comment must not ride the publish request, got %v", data)
	}
	if data := submitFirstComment(t, &models.Platform{ID: "AXqWG7U2qnpt", Name: "LinkedIn"}, "nope", 0); data != nil {
		t.Errorf("a platform without first comments must get none, got %v", data)
	}
}

// pollToTerminal runs one poll that finds the post's Zernio job in status.
func pollToTerminal(t *testing.T, status zernio.JobStatus, delay int) *models.Post {
	t.Helper()
	stub := newStubZernio()
	defer stub.Close()
	stub.handle("GET", "/posts/z-1", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, zernio.PostEnvelope{Post: zernio.Job{
			ID: "z-1", Status: status,
			Platforms: []zernio.PlatformOutcome{{Platform: "linkedin", AccountID: "acc-1", Status: string(status)}},
		}})
	})
	deps, postRepo, _ := makeDeps(stub, nil)
	post := seedScheduledPost(postRepo)
	post.Platform = linkedinWithComments()
	post.PlatformPostType = "text-post"
	post.PublisherPostID = "z-1"
	post.FirstComment = "link"
	post.FirstCommentDelayMinutes = delay
	postRepo.put(post)

	proc := &queues.PollZernioStatusProcessor{Deps: deps}
	if err := proc.Process(t.Context(), queues.PollZernioStatusTask{PostID: post.ID}); err != nil {
		t.Fatalf("process: %v", err)
	}
	got, _ := postRepo.GetByID(t.Context(), post.ID)
	return got
}

func firstCommentStatus(p *models.Post) models.FirstCommentStatus {
	if p.FirstCommentStatus == nil {
		return ""
	}
	return *p.FirstCommentStatus
}

func TestPollSettlesFirstCommentOnTerminalStatus(t *testing.T) {
	if got := firstCommentStatus(pollToTerminal(t, zernio.JobStatusPublished, 0)); got != models.FirstCommentDelegated {
		t.Errorf("published, no delay: status = %q, want delegated", got)
	}
	if got := firstCommentStatus(pollToTerminal(t, zernio.JobStatusFailed, 3)); got != models.FirstCommentSkipped {
		t.Errorf("failed: status = %q, want skipped", got)
	}
	// Outside a River worker the delayed comment can't be queued; it fails
	// rather than staying pending forever.
	got := pollToTerminal(t, zernio.JobStatusPublished, 3)
	if firstCommentStatus(got) != models.FirstCommentFailed || got.FirstCommentError == "" {
		t.Errorf("published, delay unqueued: status = %q error = %q, want failed", firstCommentStatus(got), got.FirstCommentError)
	}
}

// seedPendingComment stores a published post whose delayed comment is due.
func seedPendingComment(repo *fakePostRepo) *models.Post {
	published := time.Now().Add(-3 * time.Minute).UTC()
	post := &models.Post{
		ID: "post-1", Status: models.PostStatusPublished, PublishedAt: &published,
		Content: "hello", FirstComment: "More at **https://x.y**", FirstCommentDelayMinutes: 3,
		FirstCommentStatus: new(models.FirstCommentPending),
		Publisher:          models.PublisherZernio, PublisherPostID: "z-1",
		PublishedResults: `[{"platform":"linkedin","accountId":"acc-1","status":"published"}]`,
	}
	repo.put(post)
	return post
}

func TestFirstCommentPostedUnderLivePost(t *testing.T) {
	stub := newStubZernio()
	defer stub.Close()
	var body map[string]string
	var key string
	stub.handle("POST", "/inbox/comments/z-1", func(w http.ResponseWriter, r *http.Request) {
		key = r.Header.Get("Idempotency-Key")
		_ = json.NewDecoder(r.Body).Decode(&body)
		writeJSON(w, http.StatusOK, map[string]any{"success": true, "data": map[string]any{"commentId": "c-1"}})
	})
	deps, postRepo, logRepo := makeDeps(stub, nil)
	seedPendingComment(postRepo)

	proc := &queues.PostFirstCommentProcessor{Deps: deps}
	if err := proc.Process(t.Context(), queues.PostFirstCommentTask{PostID: "post-1", PublisherPostID: "z-1"}, false); err != nil {
		t.Fatalf("process: %v", err)
	}
	// The account comes from Zernio's publish outcome; the text is flattened.
	if body["accountId"] != "acc-1" || body["message"] != "More at https://x.y" {
		t.Errorf("comment body = %v", body)
	}
	if key != "first-comment:post-1" {
		t.Errorf("Idempotency-Key = %q", key)
	}
	got, _ := postRepo.GetByID(t.Context(), "post-1")
	if firstCommentStatus(got) != models.FirstCommentPosted || got.FirstCommentID != "c-1" || got.FirstCommentPostedAt == nil {
		t.Errorf("post = status %q id %q posted_at %v", firstCommentStatus(got), got.FirstCommentID, got.FirstCommentPostedAt)
	}
	if !logRepo.hasSummary("first comment posted") {
		t.Errorf("expected a first-comment log entry, got %v", logRepo.eventTypes())
	}

	// A second run finds nothing pending and doesn't comment again.
	stub.handle("POST", "/inbox/comments/z-1", func(w http.ResponseWriter, r *http.Request) {
		t.Error("comment posted twice")
	})
	if err := proc.Process(t.Context(), queues.PostFirstCommentTask{PostID: "post-1", PublisherPostID: "z-1"}, false); err != nil {
		t.Fatalf("rerun: %v", err)
	}
}

func TestFirstCommentRetriesTransientErrorsThenFails(t *testing.T) {
	stub := newStubZernio()
	defer stub.Close()
	var calls atomic.Int32
	stub.handle("POST", "/inbox/comments/z-1", func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		writeJSON(w, http.StatusConflict, map[string]string{"error": "request in progress"})
	})
	deps, postRepo, _ := makeDeps(stub, nil)
	seedPendingComment(postRepo)
	proc := &queues.PostFirstCommentProcessor{Deps: deps}
	task := queues.PostFirstCommentTask{PostID: "post-1", PublisherPostID: "z-1"}

	if err := proc.Process(t.Context(), task, false); err == nil {
		t.Fatal("a 409 (key in flight) should be retried")
	}
	got, _ := postRepo.GetByID(t.Context(), "post-1")
	if firstCommentStatus(got) != models.FirstCommentPending {
		t.Errorf("after a retryable error: status = %q, want pending", firstCommentStatus(got))
	}

	if err := proc.Process(t.Context(), task, true); err != nil {
		t.Fatalf("last attempt should settle, got %v", err)
	}
	got, _ = postRepo.GetByID(t.Context(), "post-1")
	if firstCommentStatus(got) != models.FirstCommentFailed {
		t.Errorf("after the last attempt: status = %q, want failed", firstCommentStatus(got))
	}
	if calls.Load() != 2 {
		t.Errorf("calls = %d, want 2", calls.Load())
	}
}

func TestFirstCommentRejectionFailsWithoutRetry(t *testing.T) {
	stub := newStubZernio()
	defer stub.Close()
	stub.handle("POST", "/inbox/comments/z-1", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "missing scope"})
	})
	deps, postRepo, _ := makeDeps(stub, nil)
	seedPendingComment(postRepo)
	proc := &queues.PostFirstCommentProcessor{Deps: deps}

	if err := proc.Process(t.Context(), queues.PostFirstCommentTask{PostID: "post-1", PublisherPostID: "z-1"}, false); err != nil {
		t.Fatalf("a 403 is terminal, got %v", err)
	}
	got, _ := postRepo.GetByID(t.Context(), "post-1")
	if firstCommentStatus(got) != models.FirstCommentFailed || got.FirstCommentError == "" {
		t.Errorf("status = %q error = %q, want failed with a reason", firstCommentStatus(got), got.FirstCommentError)
	}
}

func TestFirstCommentSkipsAPostThatMovedOn(t *testing.T) {
	stub := newStubZernio()
	defer stub.Close()
	stub.handle("POST", "/inbox/comments/z-1", func(w http.ResponseWriter, r *http.Request) {
		t.Error("commented on a post that moved on")
	})
	deps, postRepo, _ := makeDeps(stub, nil)
	seedPendingComment(postRepo)
	proc := &queues.PostFirstCommentProcessor{Deps: deps}

	// The post was republished under another Zernio post since.
	if err := proc.Process(t.Context(), queues.PostFirstCommentTask{PostID: "post-1", PublisherPostID: "z-old"}, false); err != nil {
		t.Fatalf("process: %v", err)
	}
}
