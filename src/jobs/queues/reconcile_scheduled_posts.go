package queues

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/jobs"
	"github.com/ogen-app/ogen/src/kernel/activity"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
	"github.com/ogen-app/ogen/src/usecase/post_actions/logs"
)

// ReconcileScheduledPostsQueue is the recurring sweeper queue.
// Runs every Every duration; finds Posts in Scheduled
// whose scheduled_at + Grace has passed without a terminal Zernio
// status, and forces them to Failed with a reason that distinguishes
// reconciliation_timeout from a Zernio-reported failure.
const ReconcileScheduledPostsQueue = "reconcile_scheduled_posts"

// ReconcileScheduledPostsTask is a marker payload — like the cleanup
// task, this queue carries no per-tick data.
type ReconcileScheduledPostsTask struct{}

// Kind implements JobArgs.
func (ReconcileScheduledPostsTask) Kind() string { return ReconcileScheduledPostsQueue }

// FailureReasonReconciliationTimeout is the verbatim prefix every
// reconciliation-timeout failure carries on Post.FailureReason. The
// distinct prefix is the contract CON-69 §8 calls out — "explicitly
// distinguishes reconciliation-timeout from a Zernio-reported
// failure".
const FailureReasonReconciliationTimeout = "reconciliation_timeout"

// ReconcileScheduledPostsProcessor wires the sweep handler.
//
// The sweep runs in a tight loop: load N stuck posts in one query,
// transition each in its own transaction, log per-post. river-service
// schedules it.
type ReconcileScheduledPostsProcessor struct {
	Repo     ReconcilePostRepo
	LogRepo  ReconcileLogRepo
	Activity *activity.Recorder // CON-125 reconciliation_timeout events; nil = no-op
	Grace    time.Duration      // how long after scheduled_at before timing out
	Limit    int                // max posts to process per tick (defaults to 100)
	// Withdraw queues the removal of a timed-out post's Zernio copy, so a
	// late publish can't contradict the failed status. Nil leaves the Zernio
	// post to the orphan sweep.
	Withdraw func(context.Context, WithdrawZernioPostTask) error
}

func newReconcileScheduledPosts(d Deps) *ReconcileScheduledPostsProcessor {
	return &ReconcileScheduledPostsProcessor{
		Repo:     d.Zernio.PostRepo,
		LogRepo:  d.Zernio.PostLogRepo,
		Activity: d.Zernio.ActivityRecorder,
		Grace:    d.ReconcileGrace,
		Withdraw: func(ctx context.Context, task WithdrawZernioPostTask) error {
			return d.Jobs.enqueue(ctx, task, nil)
		},
	}
}

// ReconcilePostRepo is the narrow surface the sweeper needs out of
// the post repository. Defining it here keeps the queue test-friendly
// without dragging in the full PostRepository interface.
type ReconcilePostRepo interface {
	ListStuckScheduled(ctx context.Context, cutoff time.Time, limit int) ([]models.Post, error)
	UpdateStatusAndReason(ctx context.Context, postID string, status models.PostStatus, reason string) error
}

// ReconcileLogRepo is the narrow PostLog surface.
type ReconcileLogRepo interface {
	Append(ctx context.Context, entry *models.PostLog) error
}

// Process runs one sweep tick.
func (p *ReconcileScheduledPostsProcessor) Process(ctx context.Context, _ ReconcileScheduledPostsTask) error {
	if p.Repo == nil {
		return nil
	}
	if p.Grace <= 0 {
		p.Grace = time.Hour
	}
	if p.Limit <= 0 {
		p.Limit = 100
	}

	cutoff := time.Now().UTC().Add(-p.Grace)
	stuck, err := p.Repo.ListStuckScheduled(ctx, cutoff, p.Limit)
	if err != nil {
		return fmt.Errorf("reconcile: list stuck: %w", err)
	}

	for i := range stuck {
		post := &stuck[i]
		// Reconcile each stuck post within its own tenant; the
		// list above ran cross-tenant under the job's system context.
		pctx := tenantctx.With(ctx, post.TenantID)
		reason := fmt.Sprintf("%s: scheduled_at=%s elapsed=%s last_publisher_status=%q",
			FailureReasonReconciliationTimeout,
			fmtTime(post.ScheduledAt),
			fmtElapsedSince(post.ScheduledAt),
			post.PublisherStatus,
		)
		if err := p.Repo.UpdateStatusAndReason(pctx, post.ID, models.PostStatusFailed, reason); err != nil {
			slog.ErrorContext(pctx, "failed to mark post failed", logging.AttrComponent, "jobs.reconcile", "post_id", post.ID, logging.AttrError, err)
			continue
		}
		from := models.PostStatusScheduled
		to := models.PostStatusFailed
		id, _ := models.NewID()
		_ = p.LogRepo.Append(pctx, &models.PostLog{
			ID:         id,
			PostID:     post.ID,
			EventType:  models.PostLogEventReconciliationTimeout,
			Actor:      models.ActorSystem,
			FromStatus: &from,
			ToStatus:   &to,
			Summary:    "reconciliation timeout: forcing Failed",
			Payload: logs.SanitizeAndCap(logs.MarshalCapped(map[string]any{
				"reason":                FailureReasonReconciliationTimeout,
				"scheduled_at":          post.ScheduledAt,
				"elapsed":               fmtElapsedSince(post.ScheduledAt),
				"last_publisher_status": post.PublisherStatus,
				"publisher_post_id":     post.PublisherPostID,
			})),
		})
		p.Activity.Record(pctx, activity.CategoryPublish, "reconciliation_timeout",
			activity.WithEntity("post", post.ID),
			activity.WithSource(activity.SourceJob),
			activity.WithStatus(string(from)+"->"+string(to)),
		)
		p.withdraw(pctx, post)
	}
	if len(stuck) > 0 {
		jobs.ReconciliationTimeouts.Add(int64(len(stuck)))
		slog.InfoContext(ctx, "forced posts failed on reconciliation timeout", logging.AttrComponent, "jobs.reconcile", "count", len(stuck))
	}
	return nil
}

// withdraw queues the deletion of a timed-out post's Zernio copy. The id stays
// on the row for diagnosis; once the Zernio post is gone, a reopen and
// reschedule takes the fresh-create path instead of retrying it. A failed
// enqueue is logged and left to the orphan sweep.
func (p *ReconcileScheduledPostsProcessor) withdraw(ctx context.Context, post *models.Post) {
	if post.PublisherPostID == "" {
		return
	}
	enqueue := p.Withdraw
	if enqueue == nil {
		return
	}
	task := WithdrawZernioPostTask{
		PublisherPostID: post.PublisherPostID,
		PostID:          post.ID,
		TenantID:        post.TenantID,
		Reason:          WithdrawReasonReconcile,
		Actor:           models.ActorSystem,
	}
	if err := enqueue(ctx, task); err != nil {
		slog.ErrorContext(ctx, "failed to queue Zernio withdrawal for timed-out post", logging.AttrComponent, "jobs.reconcile",
			"post_id", post.ID, "publisher_post_id", post.PublisherPostID, logging.AttrError, err)
	}
}

func fmtTime(t *time.Time) string {
	if t == nil {
		return "<nil>"
	}
	return t.UTC().Format(time.RFC3339)
}

func fmtElapsedSince(t *time.Time) string {
	if t == nil {
		return "<nil>"
	}
	return time.Since(*t).Truncate(time.Second).String()
}
