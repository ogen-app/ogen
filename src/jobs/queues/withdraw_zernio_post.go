package queues

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/riverqueue/river"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/publishers/zernio"
	"github.com/ogen-app/ogen/src/jobs"
	"github.com/ogen-app/ogen/src/kernel/activity"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
	"github.com/ogen-app/ogen/src/usecase/notify"
	"github.com/ogen-app/ogen/src/usecase/post_actions/logs"
)

// WithdrawZernioPostQueue is the River queue name.
const WithdrawZernioPostQueue = "withdraw_zernio_post"

// Reasons a Zernio post is withdrawn, recorded on the Post Log.
const (
	WithdrawReasonPostDeleted     = "post_deleted"
	WithdrawReasonCampaignDeleted = "campaign_deleted"
	WithdrawReasonReconcile       = "reconciliation_timeout"
	WithdrawReasonUnscheduled     = "unscheduled_during_submit"
)

// WithdrawZernioPostTask deletes one Zernio post that Ogen no longer
// schedules. Unlike cancel_zernio_job it never moves the Ogen post: its caller
// has already decided the post's fate (deleted it, unscheduled it, failed it)
// in the transaction that enqueued this task. The post may be gone by the time
// the task runs, so the task carries everything it needs.
type WithdrawZernioPostTask struct {
	PublisherPostID string `json:"publisher_post_id"`
	PostID          string `json:"post_id"`
	TenantID        string `json:"tenant_id"`
	Reason          string `json:"reason"`
	Actor           string `json:"actor"`
}

// Kind implements river.JobArgs.
func (WithdrawZernioPostTask) Kind() string { return WithdrawZernioPostQueue }

// InsertOpts allows more attempts than cancel: there is no user waiting on a
// status flip, and giving up leaves a post queued in Zernio until the orphan
// sweep finds it.
func (WithdrawZernioPostTask) InsertOpts() river.InsertOpts {
	return river.InsertOpts{MaxAttempts: 8}
}

// WithdrawZernioPostProcessor is the River worker for withdraw_zernio_post.
type WithdrawZernioPostProcessor struct {
	river.WorkerDefaults[WithdrawZernioPostTask]
	Deps ZernioDeps
	// Notifier tells the author when the post went out before it could be
	// withdrawn. Nil is a no-op.
	Notifier *notify.Service
	Members  memberLister
}

// Work is the River entrypoint; it delegates to Process.
func (p *WithdrawZernioPostProcessor) Work(ctx context.Context, job *river.Job[WithdrawZernioPostTask]) error {
	ctx = WithJobRequestID(ctx, job.JobRow)
	return p.Process(ctx, job.Args)
}

// Timeout is the per-attempt context deadline.
func (p *WithdrawZernioPostProcessor) Timeout(*river.Job[WithdrawZernioPostTask]) time.Duration {
	return 20 * time.Second
}

func init() {
	register(func(w *river.Workers, d Deps) {
		river.AddWorker(w, &WithdrawZernioPostProcessor{Deps: d.Zernio, Notifier: d.Notifier, Members: d.Users})
	})
}

// Process runs one withdrawal attempt. A transient Zernio error is returned so
// River retries; every other outcome is final.
func (p *WithdrawZernioPostProcessor) Process(ctx context.Context, task WithdrawZernioPostTask) error {
	if task.PublisherPostID == "" {
		return nil
	}
	ctx = tenantctx.With(ctx, task.TenantID)
	published, err := withdraw(ctx, p.Deps, task)
	if err != nil {
		return err
	}
	if published {
		p.notifyTooLate(ctx, task)
	}
	return nil
}

// notifyTooLate tells the workspace that a post it had unscheduled or deleted
// was published anyway, so it never goes out silently. Only possible while the
// post row still exists; a deleted post is left to the Post Log and log line.
func (p *WithdrawZernioPostProcessor) notifyTooLate(ctx context.Context, task WithdrawZernioPostTask) {
	if p.Notifier == nil || task.PostID == "" {
		return
	}
	post, err := p.Deps.PostRepo.GetByID(ctx, task.PostID)
	if err != nil {
		return
	}
	emitPublishNotification(ctx, p.Notifier, p.Members, post, true)
}

// withdraw deletes the Zernio post named by task. It reports published=true
// when Zernio had already published it (nothing left to withdraw), and returns
// an error only for a transient failure worth retrying. Terminal API errors
// are logged and dropped: the orphan sweep is the backstop.
func withdraw(ctx context.Context, deps ZernioDeps, task WithdrawZernioPostTask) (published bool, err error) {
	apiStart := time.Now()
	cancelErr := deps.Client.Cancel(ctx, task.PublisherPostID)
	jobs.ObserveZernioCall(time.Since(apiStart))

	payload := withdrawPayload(task)
	switch {
	case cancelErr == nil, errors.Is(cancelErr, zernio.ErrPostNotFound):
		jobs.ZernioWithdrawSucceeded.Add(1)
		logWithdraw(ctx, deps, task, "Zernio post withdrawn", payload)
		recordWithdraw(ctx, deps, task, "publish_withdrawn")
		return false, nil
	case errors.Is(cancelErr, zernio.ErrAlreadyPublished):
		jobs.ZernioWithdrawTooLate.Add(1)
		slog.WarnContext(ctx, "post published before it could be withdrawn from Zernio",
			logging.AttrComponent, "jobs.withdraw", "post_id", task.PostID,
			"publisher_post_id", task.PublisherPostID, "reason", task.Reason)
		logWithdraw(ctx, deps, task, "withdraw too late: Zernio had already published the post", payload)
		recordWithdraw(ctx, deps, task, "publish_withdraw_too_late")
		return true, nil
	case zernio.IsTerminalAPIError(cancelErr):
		jobs.ZernioWithdrawFailed.Add(1)
		slog.ErrorContext(ctx, "Zernio refused to withdraw a post",
			logging.AttrComponent, "jobs.withdraw", "post_id", task.PostID,
			"publisher_post_id", task.PublisherPostID, logging.AttrError, cancelErr)
		logWithdraw(ctx, deps, task, "Zernio withdraw terminally failed", errPayload(cancelErr))
		return false, nil
	default:
		return false, fmt.Errorf("withdraw %s: %w", task.PublisherPostID, cancelErr)
	}
}

// logWithdraw appends the outcome to the post's log. A deleted post's log is
// gone with it, so the append is skipped for those.
func logWithdraw(ctx context.Context, deps ZernioDeps, task WithdrawZernioPostTask, summary, payload string) {
	if task.PostID == "" || task.Reason == WithdrawReasonPostDeleted {
		return
	}
	status := models.PostStatus("")
	if post, err := deps.PostRepo.GetByID(ctx, task.PostID); err == nil {
		status = post.Status
	}
	appendLogActor(ctx, deps, task.PostID, task.Actor, models.PostLogEventZernioCancel, status, status, summary, payload)
}

func recordWithdraw(ctx context.Context, deps ZernioDeps, task WithdrawZernioPostTask, action string) {
	deps.ActivityRecorder.Record(ctx, activity.CategoryPublish, action,
		activity.WithEntity("post", task.PostID),
		activity.WithUser(task.Actor),
		activity.WithSource(activity.SourceJob),
		activity.WithPayload(map[string]any{
			"reason":            task.Reason,
			"publisher_post_id": task.PublisherPostID,
		}),
	)
}

func withdrawPayload(task WithdrawZernioPostTask) string {
	return logs.MarshalCapped(map[string]string{
		"publisher_post_id": task.PublisherPostID,
		"reason":            task.Reason,
	})
}

// enqueueWithdraw inserts a withdraw task from inside a running worker, on the
// worker's own River client. It fails when the context carries no client.
func enqueueWithdraw(ctx context.Context, task WithdrawZernioPostTask) error {
	client, err := river.ClientFromContextSafely[*sql.Tx](ctx)
	if err != nil {
		return err
	}
	_, err = client.Insert(ctx, task, insertOptsWithRequestID(ctx, nil))
	return err
}
