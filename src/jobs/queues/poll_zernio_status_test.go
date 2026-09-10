package queues_test

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/riverqueue/river"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/publishers/zernio"
	"github.com/ogen-app/ogen/src/jobs/queues"
)

func TestPollTerminalPublishedTransitionsPost(t *testing.T) {
	stub := newStubZernio()
	defer stub.Close()
	stub.handle("GET", "/posts/z-1", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, zernio.PostEnvelope{Post: zernio.Job{
			ID: "z-1", Status: zernio.JobStatusPublished,
			Platforms: []zernio.PlatformOutcome{{Platform: "linkedin", Status: "published", PlatformPostURL: "https://li/x"}},
		}})
	})
	deps, postRepo, logRepo := makeDeps(stub, nil)
	post := seedScheduledPost(postRepo)
	post.PublisherPostID = "z-1"
	postRepo.put(post)

	proc := &queues.PollZernioStatusProcessor{Deps: deps}
	if err := proc.Process(context.Background(), queues.PollZernioStatusTask{PostID: post.ID}); err != nil {
		t.Fatalf("process: %v", err)
	}
	got, _ := postRepo.GetByID(context.Background(), post.ID)
	if got.Status != models.PostStatusPublished {
		t.Errorf("status: got %q want published", got.Status)
	}
	if got.PublishedAt == nil {
		t.Error("published_at should be set")
	}
	if got.PublishedResults == "" {
		t.Error("published_results should be set with platform outcomes")
	}
	// CON-165: the permalink is lifted out of the per-platform blob into a
	// first-class field.
	if got.PublishedURL != "https://li/x" {
		t.Errorf("published_url: got %q want %q", got.PublishedURL, "https://li/x")
	}
	_ = logRepo
}

func TestPollTerminalFailedTransitionsPost(t *testing.T) {
	stub := newStubZernio()
	defer stub.Close()
	stub.handle("GET", "/posts/z-1", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, zernio.PostEnvelope{Post: zernio.Job{
			ID: "z-1", Status: zernio.JobStatusFailed,
		}})
	})
	deps, postRepo, _ := makeDeps(stub, nil)
	post := seedScheduledPost(postRepo)
	post.PublisherPostID = "z-1"
	postRepo.put(post)

	proc := &queues.PollZernioStatusProcessor{Deps: deps}
	if err := proc.Process(context.Background(), queues.PollZernioStatusTask{PostID: post.ID}); err != nil {
		t.Fatalf("process: %v", err)
	}
	got, _ := postRepo.GetByID(context.Background(), post.ID)
	if got.Status != models.PostStatusFailed {
		t.Errorf("status: got %q want failed", got.Status)
	}
}

func TestPollPartialMapsToFailed(t *testing.T) {
	// CON-69 deferred PartiallyPublished, so partial → Failed for now
	// with the per-platform breakdown captured in PostLog/results.
	stub := newStubZernio()
	defer stub.Close()
	stub.handle("GET", "/posts/z-1", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, zernio.PostEnvelope{Post: zernio.Job{
			ID: "z-1", Status: zernio.JobStatusPartial,
			Platforms: []zernio.PlatformOutcome{
				{Platform: "linkedin", Status: "published"},
				{Platform: "twitter", Status: "failed", ErrorMessage: "auth"},
			},
		}})
	})
	deps, postRepo, _ := makeDeps(stub, nil)
	post := seedScheduledPost(postRepo)
	post.PublisherPostID = "z-1"
	postRepo.put(post)

	proc := &queues.PollZernioStatusProcessor{Deps: deps}
	_ = proc.Process(context.Background(), queues.PollZernioStatusTask{PostID: post.ID})
	got, _ := postRepo.GetByID(context.Background(), post.ID)
	if got.Status != models.PostStatusFailed {
		t.Errorf("status: got %q want failed (partial → Failed for MVP)", got.Status)
	}
}

func TestPollExitsCleanlyWhenPostNotScheduled(t *testing.T) {
	stub := newStubZernio()
	defer stub.Close()
	// No route registered → would 500 if the handler called Status.
	deps, postRepo, _ := makeDeps(stub, nil)
	post := seedScheduledPost(postRepo)
	post.Status = models.PostStatusFailed // user/reconciler already moved it
	postRepo.put(post)

	proc := &queues.PollZernioStatusProcessor{Deps: deps}
	if err := proc.Process(context.Background(), queues.PollZernioStatusTask{PostID: post.ID}); err != nil {
		t.Fatalf("process should return nil for non-scheduled posts: %v", err)
	}
}

func TestPollNonTerminalSnoozes(t *testing.T) {
	stub := newStubZernio()
	defer stub.Close()
	stub.handle("GET", "/posts/z-1", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, zernio.PostEnvelope{Post: zernio.Job{
			ID: "z-1", Status: zernio.JobStatusScheduled, // non-terminal
		}})
	})
	deps, postRepo, _ := makeDeps(stub, nil)
	post := seedScheduledPost(postRepo)
	post.PublisherPostID = "z-1"
	postRepo.put(post)

	proc := &queues.PollZernioStatusProcessor{Deps: deps}
	err := proc.Process(context.Background(), queues.PollZernioStatusTask{PostID: post.ID})

	// Non-terminal → reschedule via river.JobSnooze (not a failure, not a
	// completion). The job is rescheduled without consuming a retry attempt.
	snooze, ok := errors.AsType[*river.JobSnoozeError](err)
	if !ok {
		t.Fatalf("expected a river JobSnooze, got %v", err)
	}
	if snooze.Duration <= 0 {
		t.Errorf("snooze duration should be positive, got %s", snooze.Duration)
	}
	// The Post stays Scheduled; Zernio's status is persisted for visibility.
	got, _ := postRepo.GetByID(context.Background(), post.ID)
	if got.Status != models.PostStatusScheduled {
		t.Errorf("status: got %q want scheduled (unchanged)", got.Status)
	}
	if got.PublisherStatus != string(zernio.JobStatusScheduled) {
		t.Errorf("publisher_status: got %q want scheduled", got.PublisherStatus)
	}
}
