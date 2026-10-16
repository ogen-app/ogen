package queues

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

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

// Kind implements JobArgs.
func (WithdrawZernioPostTask) Kind() string { return WithdrawZernioPostQueue }

// WithdrawZernioPostProcessor runs withdraw_zernio_post.
type WithdrawZernioPostProcessor struct {
	Deps ZernioDeps
	// Notifier tells the author when the post went out before it could be
	// withdrawn. Nil is a no-op.
	Notifier *notify.Service
	Members  memberLister
}

// Process runs one withdrawal attempt in-process. A transient Zernio error is
// returned so River retries; every other outcome is final.
func (p *WithdrawZernioPostProcessor) Process(ctx context.Context, task WithdrawZernioPostTask) error {
	return runLocal(ctx, p.step, WithdrawZernioPostQueue, task, 0, 0, localActions{zernio: p.Deps.Client})
}

// step deletes the Zernio post in one call, then settles the outcome and
// drops the post's pending status polls.
func (p *WithdrawZernioPostProcessor) step(ctx context.Context, in StepInput) (StepOutput, error) {
	var task WithdrawZernioPostTask
	if err := json.Unmarshal(in.Args, &task); err != nil {
		return done("skipped_bad_args")
	}
	if task.PublisherPostID == "" {
		return done("skipped_no_zernio_post")
	}
	ctx = tenantctx.With(ctx, task.TenantID)
	cancel := func(c *zernio.Client) error { return c.Cancel(ctx, task.PublisherPostID) }
	if in.Result == nil {
		call, err := zernio.Plan(cancel)
		if err != nil {
			return StepOutput{}, err
		}
		return nextZernio(nil, call)
	}
	if zernioDisabled(in.Result) {
		return done("skipped_zernio_disabled")
	}
	published, err := withdraw(ctx, p.Deps, task, zernio.Decode(zernioResult(in.Result), cancel))
	if err != nil {
		return retry("withdraw", err)
	}
	if published {
		p.notifyTooLate(ctx, task)
	}
	out, _ := done("withdrawn")
	if task.PostID != "" {
		out.Done.Cancel = []JobMatch{pollMatch(task.PostID, task.PublisherPostID)}
	}
	return out, nil
}

// pollMatch selects the status polls of one Zernio submission of a post,
// including polls enqueued before they carried the submission's id.
func pollMatch(postID, publisherPostID string) JobMatch {
	return JobMatch{Kind: PollZernioStatusQueue, Args: []ArgMatch{
		{Key: "post_id", Values: []string{postID}},
		{Key: "publisher_post_id", Values: []string{"", publisherPostID}},
	}}
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

// withdraw settles the deletion of the Zernio post named by task, given
// Zernio's answer. It reports published=true when Zernio had already
// published it (nothing left to withdraw), and returns an error only for a
// transient failure worth retrying. Terminal API errors are logged and
// dropped: the orphan sweep is the backstop.
func withdraw(ctx context.Context, deps ZernioDeps, task WithdrawZernioPostTask, cancelErr error) (published bool, err error) {
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
	if post, err := deps.PostRepo.GetRowByID(ctx, task.PostID); err == nil {
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
