package queues_test

import (
	"context"
	"database/sql"
	"net/http"
	"testing"
	"time"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverdatabasesql"
	"github.com/riverqueue/river/rivertest"
	"github.com/riverqueue/river/rivertype"

	"github.com/ogen-app/ogen/src/jobs/queues"
	"github.com/ogen-app/ogen/src/pgtest"
)

// pollQueue is a River client on a fresh database holding three delayed
// polls: two for post's submission z-1 (one enqueued before polls carried the
// publisher id), and one each for a newer submission and another post. It
// returns a worker context bound to the client and the job ids by name.
func pollQueue(t *testing.T, postID string) (context.Context, *river.Client[*sql.Tx], map[string]int64) {
	t.Helper()
	db := pgtest.MustDB()
	t.Cleanup(func() { _ = db.Close() })
	client, err := river.NewClient[*sql.Tx](riverdatabasesql.New(db.DB), &river.Config{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := rivertest.WorkContext(t.Context(), client)
	later := &river.InsertOpts{ScheduledAt: time.Now().Add(time.Hour)}
	ids := map[string]int64{}
	for name, task := range map[string]queues.PollZernioStatusTask{
		"current": {PostID: postID, PublisherPostID: "z-1"},
		"legacy":  {PostID: postID},
		"newer":   {PostID: postID, PublisherPostID: "z-2"},
		"other":   {PostID: "other-post", PublisherPostID: "z-9"},
	} {
		res, err := client.Insert(ctx, task, later)
		if err != nil {
			t.Fatalf("insert %s: %v", name, err)
		}
		ids[name] = res.Job.ID
	}
	return ctx, client, ids
}

func assertPollStates(t *testing.T, ctx context.Context, client *river.Client[*sql.Tx], ids map[string]int64) {
	t.Helper()
	want := map[string]rivertype.JobState{
		"current": rivertype.JobStateCancelled,
		"legacy":  rivertype.JobStateCancelled,
		"newer":   rivertype.JobStateScheduled,
		"other":   rivertype.JobStateScheduled,
	}
	for name, id := range ids {
		job, err := client.JobGet(ctx, id)
		if err != nil {
			t.Fatalf("get %s: %v", name, err)
		}
		if job.State != want[name] {
			t.Errorf("%s poll: state %q, want %q", name, job.State, want[name])
		}
	}
}

func TestCancelDropsThePendingPollsOfTheWithdrawnSubmission(t *testing.T) {
	// Unschedule deletes the Zernio post; the delayed poll for it must leave
	// the queue too rather than sit there until it wakes and exits.
	stub := newStubZernio()
	defer stub.Close()
	stub.handle("DELETE", "/posts/z-1", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	deps, postRepo, logRepo := makeDeps(stub, nil)
	post := seedScheduledPost(postRepo)
	post.PublisherPostID = "z-1"
	postRepo.put(post)
	ctx, client, ids := pollQueue(t, post.ID)

	proc := &queues.CancelZernioJobProcessor{Deps: deps}
	if err := proc.Process(ctx, queues.CancelZernioJobTask{
		PostID: post.ID, Target: queues.CancelTargetReadyForPublish, Actor: "user-1",
	}); err != nil {
		t.Fatalf("process: %v", err)
	}
	assertPollStates(t, ctx, client, ids)
	if !logRepo.hasSummary("pending status polls cancelled") {
		t.Errorf("expected a poll-cancel log, got %v", logRepo.eventTypes())
	}
}

func TestWithdrawDropsThePendingPollsOfTheWithdrawnSubmission(t *testing.T) {
	// Deleting a post or its campaign withdraws through this worker; the
	// post's polls go with the Zernio post.
	stub := newStubZernio()
	defer stub.Close()
	countDeletes(stub, "z-1", http.StatusNoContent)
	deps, postRepo, _ := makeDeps(stub, nil)
	post := seedScheduledPost(postRepo)
	ctx, client, ids := pollQueue(t, post.ID)

	proc := &queues.WithdrawZernioPostProcessor{Deps: deps}
	if err := proc.Process(ctx, queues.WithdrawZernioPostTask{
		PublisherPostID: "z-1", PostID: post.ID, TenantID: post.TenantID,
		Reason: queues.WithdrawReasonPostDeleted, Actor: "user-1",
	}); err != nil {
		t.Fatalf("process: %v", err)
	}
	assertPollStates(t, ctx, client, ids)
}
