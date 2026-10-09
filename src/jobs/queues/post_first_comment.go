package queues

import (
	"cmp"
	"context"
	"database/sql"
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

// Work is the River entrypoint; it delegates to Process.
func (p *PostFirstCommentProcessor) Work(ctx context.Context, job *river.Job[PostFirstCommentTask]) error {
	ctx = WithJobRequestID(ctx, job.JobRow)
	ctx = tenantctx.WithSystem(ctx)
	return p.Process(ctx, job.Args, job.Attempt >= job.MaxAttempts)
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

// Process runs one attempt. It returns an error only for a transient failure
// River should retry; lastAttempt turns that failure into a settled one, since
// no retry follows.
func (p *PostFirstCommentProcessor) Process(ctx context.Context, task PostFirstCommentTask, lastAttempt bool) error {
	post, err := p.Deps.PostRepo.GetByID(ctx, task.PostID)
	if err != nil {
		return fmt.Errorf("first comment: load post %s: %w", task.PostID, err)
	}
	ctx = tenantctx.With(ctx, post.TenantID)
	if post.Status != models.PostStatusPublished ||
		post.PublisherPostID != task.PublisherPostID ||
		post.FirstCommentStatus == nil || *post.FirstCommentStatus != models.FirstCommentPending {
		return nil
	}
	accountID := cmp.Or(post.SocialAccountID, publishedAccountID(post))
	if accountID == "" {
		return p.fail(ctx, post, "no Zernio account recorded for the published post")
	}

	message := platforms.FlattenSocialText(post.FirstComment)
	commentID, err := p.Deps.Client.PostComment(ctx, post.PublisherPostID, accountID, message, "first-comment:"+post.ID)
	if err != nil {
		if zernio.IsTransientCommentError(err) && !lastAttempt {
			jobs.ZernioFirstCommentRetried.Add(1)
			appendLog(ctx, p.Deps, post.ID, models.PostLogEventTaskRetried, post.Status, post.Status,
				"first comment: transient Zernio error; River will retry", errPayload(err))
			return err
		}
		return p.fail(ctx, post, commentErrorMessage(err))
	}

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
func enqueueFirstComment(ctx context.Context, post *models.Post, publishedAt time.Time) error {
	client, err := river.ClientFromContextSafely[*sql.Tx](ctx)
	if err != nil {
		return err
	}
	task := PostFirstCommentTask{PostID: post.ID, PublisherPostID: post.PublisherPostID}
	when := publishedAt.Add(time.Duration(post.FirstCommentDelayMinutes) * time.Minute)
	_, err = client.Insert(ctx, task, insertOptsWithRequestID(ctx, &river.InsertOpts{ScheduledAt: when}))
	return err
}
