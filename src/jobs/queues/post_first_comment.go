package queues

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/riverqueue/river"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/domain/platforms"
	"github.com/ogen-app/ogen/src/infra/publishers/zernio"
	"github.com/ogen-app/ogen/src/jobs"
	"github.com/ogen-app/ogen/src/kernel/activity"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
	"github.com/ogen-app/ogen/src/usecase/notify"
	"github.com/ogen-app/ogen/src/usecase/post_actions/logs"
)

// PostFirstCommentQueue is the River job kind.
const PostFirstCommentQueue = "post_first_comment"

// PostFirstCommentTask posts a live post's delayed first comment. It carries
// the Zernio post the comment belongs to, so a post that was republished
// under another Zernio post since doesn't get this comment.
type PostFirstCommentTask struct {
	PostID          string `json:"post_id"`
	PublisherPostID string `json:"publisher_post_id"`
}

// Kind implements river.JobArgs.
func (PostFirstCommentTask) Kind() string { return PostFirstCommentQueue }

// InsertOpts allows 5 attempts and one live job per comment.
func (PostFirstCommentTask) InsertOpts() river.InsertOpts {
	unique := periodicUniqueOpts()
	unique.ByArgs = true
	return river.InsertOpts{MaxAttempts: 5, UniqueOpts: unique}
}

// PostFirstCommentProcessor posts the first comment through Zernio's comments
// API once the post's delay has elapsed.
type PostFirstCommentProcessor struct {
	river.WorkerDefaults[PostFirstCommentTask]
	Deps ZernioDeps
	// Notifier tells the author when the comment couldn't be posted. Nil is
	// a no-op.
	Notifier *notify.Service
}

// Work is the River entrypoint: it runs the job's steps in-process.
func (p *PostFirstCommentProcessor) Work(ctx context.Context, job *river.Job[PostFirstCommentTask]) error {
	ctx = WithJobRequestID(ctx, job.JobRow)
	return runLocal(ctx, p.step, job.Kind, job.Args, job.Attempt, job.MaxAttempts, localActions{zernio: p.Deps.Client})
}

// Timeout is the per-attempt context deadline.
func (p *PostFirstCommentProcessor) Timeout(*river.Job[PostFirstCommentTask]) time.Duration {
	return 20 * time.Second
}

func init() {
	register(func(w *river.Workers, d Deps) {
		river.AddWorker(w, &PostFirstCommentProcessor{Deps: d.Zernio, Notifier: d.Notifier})
	})
}

// Process runs one attempt in-process. It returns an error only for a
// transient failure River should retry; lastAttempt turns that failure into a
// settled one, since no retry follows.
func (p *PostFirstCommentProcessor) Process(ctx context.Context, task PostFirstCommentTask, lastAttempt bool) error {
	attempt, maxAttempts := 1, 2
	if lastAttempt {
		attempt = 2
	}
	return runLocal(ctx, p.step, PostFirstCommentQueue, task, attempt, maxAttempts, localActions{zernio: p.Deps.Client})
}

// firstCommentState carries the post's comment between the call and its
// result.
type firstCommentState struct {
	AccountID string `json:"account_id"`
	Message   string `json:"message"`
}

// step posts the comment in one call, then settles it.
func (p *PostFirstCommentProcessor) step(ctx context.Context, in StepInput) (StepOutput, error) {
	var task PostFirstCommentTask
	if err := json.Unmarshal(in.Args, &task); err != nil {
		return done("skipped_bad_args")
	}
	ctx = tenantctx.WithSystem(ctx)
	post, err := p.Deps.PostRepo.GetByID(ctx, task.PostID)
	if err != nil {
		return retry("first comment: load post "+task.PostID, err)
	}
	ctx = tenantctx.With(ctx, post.TenantID)
	if post.Status != models.PostStatusPublished ||
		post.PublisherPostID != task.PublisherPostID ||
		post.FirstCommentStatus == nil || *post.FirstCommentStatus != models.FirstCommentPending {
		return done("skipped_settled")
	}

	var st firstCommentState
	if err := decodeState(in.State, &st); err != nil {
		return done("skipped_bad_state")
	}
	comment := func(c *zernio.Client) (string, error) {
		return c.PostComment(ctx, post.PublisherPostID, st.AccountID, st.Message, "first-comment:"+post.ID)
	}
	if in.Result == nil {
		st.AccountID = cmp.Or(post.SocialAccountID, publishedAccountID(post))
		if st.AccountID == "" {
			return p.settle(p.fail(ctx, post, "no Zernio account recorded for the published post"))
		}
		st.Message = platforms.FlattenSocialText(post.FirstComment)
		call, err := zernio.Plan(func(c *zernio.Client) error { _, err := comment(c); return err })
		if err != nil {
			return StepOutput{}, err
		}
		return nextZernio(st, call)
	}
	if zernioDisabled(in.Result) {
		return p.settle(p.fail(ctx, post, "the Zernio integration is off"))
	}

	var commentID string
	err = zernio.Decode(zernioResult(in.Result), func(c *zernio.Client) error {
		var err error
		commentID, err = comment(c)
		return err
	})
	if err != nil {
		if zernio.IsTransientCommentError(err) && !in.lastAttempt() {
			jobs.ZernioFirstCommentRetried.Add(1)
			appendLog(ctx, p.Deps, post.ID, models.PostLogEventTaskRetried, post.Status, post.Status,
				"first comment: transient Zernio error; River will retry", errPayload(err))
			return retry("first comment", err)
		}
		return p.settle(p.fail(ctx, post, commentErrorMessage(err)))
	}
	return p.settle(p.posted(ctx, post, commentID))
}

// settle ends the job, or retries it when persisting the outcome failed.
func (p *PostFirstCommentProcessor) settle(err error) (StepOutput, error) {
	if err != nil {
		return retry("first comment", err)
	}
	return done("settled")
}

// posted records the posted comment.
func (p *PostFirstCommentProcessor) posted(ctx context.Context, post *models.Post, commentID string) error {
	now := time.Now().UTC()
	post.FirstCommentStatus = new(models.FirstCommentPosted)
	post.FirstCommentID = commentID
	post.FirstCommentPostedAt = &now
	post.FirstCommentError = ""
	ok, err := p.Deps.PostRepo.SettleFirstComment(ctx, post,
		"first_comment_status", "first_comment_id", "first_comment_posted_at", "first_comment_error")
	if err != nil {
		return fmt.Errorf("first comment: persist posted: %w", err)
	}
	if !ok {
		return nil
	}
	jobs.ZernioFirstCommentPosted.Add(1)
	appendLog(ctx, p.Deps, post.ID, models.PostLogEventZernioFirstComment, post.Status, post.Status,
		"first comment posted", logs.MarshalCapped(map[string]string{"comment_id": commentID}))
	p.Deps.ActivityRecorder.Record(ctx, activity.CategoryPublish, "first_comment_posted",
		activity.WithEntity("post", post.ID),
		activity.WithSource(activity.SourceJob),
		activity.WithStatus(string(models.FirstCommentPosted)),
	)
	return nil
}

// fail settles the comment as failed and tells the author. The post itself
// stays published.
func (p *PostFirstCommentProcessor) fail(ctx context.Context, post *models.Post, reason string) error {
	post.FirstCommentStatus = new(models.FirstCommentFailed)
	post.FirstCommentError = reason
	ok, err := p.Deps.PostRepo.SettleFirstComment(ctx, post, "first_comment_status", "first_comment_error")
	if err != nil {
		return fmt.Errorf("first comment: persist failed: %w", err)
	}
	if !ok {
		return nil
	}
	jobs.ZernioFirstCommentFailed.Add(1)
	appendLog(ctx, p.Deps, post.ID, models.PostLogEventZernioFirstComment, post.Status, post.Status,
		"first comment failed", logs.MarshalCapped(map[string]string{"error": reason}))
	p.Deps.ActivityRecorder.Record(ctx, activity.CategoryPublish, "first_comment_failed",
		activity.WithEntity("post", post.ID),
		activity.WithSource(activity.SourceJob),
		activity.WithStatus(string(models.FirstCommentFailed)),
	)
	_ = p.Notifier.EmitToUsers(ctx, authorOnly(post.CreatedBy), notify.Spec{
		Level:      models.NotificationLevelError,
		Type:       "post.first_comment_failed",
		Title:      "First comment not posted",
		Body:       "Your post is live, but its first comment couldn't be posted.",
		EntityType: "post",
		EntityID:   post.ID,
		ActionURL:  "/posts/" + post.ID,
		Data:       map[string]any{"error": reason},
		DedupeKey:  "post.first_comment_failed:" + post.ID,
	})
	return nil
}

// commentErrorMessage explains a failed comment to the user. A 403 is almost
// always an account connected without the comments permission.
func commentErrorMessage(err error) string {
	if zernio.IsStatus(err, 403) {
		return "the account lacks permission to comment; reconnect it and allow comments"
	}
	return err.Error()
}

// publishedAccountID is the Zernio account the post went out on, read from the
// per-platform outcomes Zernio reported at publish.
func publishedAccountID(post *models.Post) string {
	var outcomes []zernio.PlatformOutcome
	if json.Unmarshal([]byte(post.PublishedResults), &outcomes) != nil {
		return ""
	}
	for _, o := range outcomes {
		if o.AccountID != "" {
			return o.AccountID
		}
	}
	return ""
}

// enqueueFirstComment schedules the delayed first comment of a post that just
// went live, delay minutes after Zernio published it.
func enqueueFirstComment(ctx context.Context, jobs *Enqueuer, post *models.Post, publishedAt time.Time) error {
	task := PostFirstCommentTask{PostID: post.ID, PublisherPostID: post.PublisherPostID}
	when := publishedAt.Add(time.Duration(post.FirstCommentDelayMinutes) * time.Minute)
	return jobs.enqueue(ctx, task, &river.InsertOpts{ScheduledAt: when})
}
