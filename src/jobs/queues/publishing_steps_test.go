package queues_test

import (
	"testing"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/publishers/zernio"
	"github.com/ogen-app/ogen/src/jobs/queues"
)

// TestWithdrawRunsAsSteps drives a withdrawal the way river-service does:
// one planned DELETE, then a done step that cancels the submission's polls.
func TestWithdrawRunsAsSteps(t *testing.T) {
	stub := newStubZernio()
	defer stub.Close()
	deps, postRepo, _ := makeDeps(stub, nil)
	post := seedScheduledPost(postRepo)
	z := &scriptedZernio{replies: map[string]zernio.Result{"DELETE /posts/z-1": {Status: 204}}}

	out := runSteps(t, queues.NewJobSteps(queues.Deps{Zernio: deps}), queues.WithdrawZernioPostQueue, queues.WithdrawZernioPostTask{
		PublisherPostID: "z-1", PostID: post.ID, TenantID: post.TenantID, Reason: queues.WithdrawReasonUnscheduled,
	}, z)
	if len(z.calls) != 1 || out.Done == nil || out.Done.Outcome != "withdrawn" {
		t.Fatalf("calls %v, out %+v", z.calls, out.Done)
	}
	if len(out.Done.Cancel) != 1 || out.Done.Cancel[0].Kind != queues.PollZernioStatusQueue {
		t.Errorf("cancel = %+v, want the post's polls", out.Done.Cancel)
	}
}

// TestPollRunsAsSteps checks a not-yet-published poll snoozes through a
// delayed retry, and a published one lands the post.
func TestPollRunsAsSteps(t *testing.T) {
	stub := newStubZernio()
	defer stub.Close()
	deps, postRepo, _ := makeDeps(stub, nil)
	post := seedScheduledPost(postRepo)
	post.PublisherPostID = "z-1"
	postRepo.put(post)
	steps := queues.NewJobSteps(queues.Deps{Zernio: deps})

	z := &scriptedZernio{replies: map[string]zernio.Result{"GET /posts/z-1": {Status: 200, Body: []byte(`{"post":{"_id":"z-1","status":"scheduled"}}`)}}}
	out := runSteps(t, steps, queues.PollZernioStatusQueue, queues.PollZernioStatusTask{PostID: post.ID, PublisherPostID: "z-1"}, z)
	if out.Retry == nil || out.Retry.After <= 0 {
		t.Fatalf("scheduled = %+v, want a delayed retry", out)
	}

	z = &scriptedZernio{replies: map[string]zernio.Result{"GET /posts/z-1": {Status: 200, Body: []byte(`{"post":{"_id":"z-1","status":"published"}}`)}}}
	out = runSteps(t, steps, queues.PollZernioStatusQueue, queues.PollZernioStatusTask{PostID: post.ID, PublisherPostID: "z-1"}, z)
	got, _ := postRepo.GetByID(t.Context(), post.ID)
	if out.Done == nil || got.Status != models.PostStatusPublished {
		t.Errorf("published: out %+v, post %s", out, got.Status)
	}
}

func TestCredentialMissingEndsPublishingJobs(t *testing.T) {
	stub := newStubZernio()
	defer stub.Close()
	deps, postRepo, _ := makeDeps(stub, nil)
	post := seedScheduledPost(postRepo)
	post.PublisherPostID = "z-1"
	postRepo.put(post)
	steps := queues.NewJobSteps(queues.Deps{Zernio: deps})
	out, err := steps.Step(t.Context(), queues.StepInput{
		Kind: queues.CancelZernioJobQueue, Args: []byte(`{"post_id":"` + post.ID + `"}`),
		Result: &queues.StepResult{CredentialMissing: true},
	})
	if err != nil || out.Done == nil || out.Done.Outcome != "skipped_zernio_disabled" {
		t.Errorf("out %+v, %v", out, err)
	}
}
