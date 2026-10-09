package queues

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/publishers/zernio"
	"github.com/ogen-app/ogen/src/jobs"
	"github.com/ogen-app/ogen/src/kernel/activity"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
	"github.com/ogen-app/ogen/src/usecase/notify"
	"github.com/ogen-app/ogen/src/usecase/post_actions/logs"
)

// PollZernioStatusQueue is the River queue name.
const PollZernioStatusQueue = "poll_zernio_status"

// PollZernioStatusTask carries the Ogen post id and the Zernio post the poll
// was enqueued for; the worker re-loads the row each cycle. A post returns to
// scheduled on every reschedule, so status alone can't tell a poll its
// submission is gone: PublisherPostID can. Empty only on polls enqueued before
// the field existed.
type PollZernioStatusTask struct {
	PostID          string `json:"post_id"`
	PublisherPostID string `json:"publisher_post_id,omitempty"`
}

// Kind implements river.JobArgs.
func (PollZernioStatusTask) Kind() string { return PollZernioStatusQueue }

// InsertOpts sets per-kind defaults: 3 total attempts for transient errors.
// The non-terminal cadence is driven by river.JobSnooze in Process, not by
// retries (snooze bumps max_attempts so it never consumes the retry budget).
// Per-attempt timeout lives on the worker. Uniqueness by args over the active
// states keeps one live poll per submission however often submit re-enqueues.
func (PollZernioStatusTask) InsertOpts() river.InsertOpts {
	unique := periodicUniqueOpts()
	unique.ByArgs = true
	return river.InsertOpts{MaxAttempts: 3, UniqueOpts: unique}
}

// PollZernioStatusProcessor implements the recurring poll. The
// cadence per CON-69 §7: every 30s for the first 5 minutes after
// scheduled_at, then every 60s.
type PollZernioStatusProcessor struct {
	river.WorkerDefaults[PollZernioStatusTask]
	Deps ZernioDeps
	// Notifier drops a publish-outcome notification when Zernio reports a terminal
	// status. Nil is a no-op.
	Notifier *notify.Service
	// Members fans the publish outcome across the workspace. Nil falls
	// back to the author.
	Members      memberLister
	FastInterval time.Duration // default 30s
	SlowInterval time.Duration // default 60s
	FastWindow   time.Duration // default 5m after scheduled_at
}

// Work is the River entrypoint; it delegates to Process.
func (p *PollZernioStatusProcessor) Work(ctx context.Context, job *river.Job[PollZernioStatusTask]) error {
	ctx = WithJobRequestID(ctx, job.JobRow)
	// Background jobs span tenants (interim until per-tenant, PR4).
	ctx = tenantctx.WithSystem(ctx)
	return p.Process(ctx, job.Args)
}

// Timeout is the per-attempt context deadline.
func (p *PollZernioStatusProcessor) Timeout(*river.Job[PollZernioStatusTask]) time.Duration {
	return 20 * time.Second
}

func init() {
	register(func(w *river.Workers, d Deps) {
		river.AddWorker(w, &PollZernioStatusProcessor{Deps: d.Zernio, Notifier: d.Notifier, Members: d.Users})
	})
}

// Process executes one poll cycle. On a non-terminal status it returns
// river.JobSnooze to reschedule this same job per the cadence rule; it returns
// a non-nil error only on transient Zernio failures so River retries per its
// backoff policy; otherwise nil (terminal handled, or quietly exited).
func (p *PollZernioStatusProcessor) Process(ctx context.Context, task PollZernioStatusTask) error {
	post, err := p.Deps.PostRepo.GetByID(ctx, task.PostID)
	if err != nil {
		return fmt.Errorf("poll: load post %s: %w", task.PostID, err)
	}
	// Scope the rest of the job to the owning tenant.
	ctx = tenantctx.With(ctx, post.TenantID)
	if post.Status != models.PostStatusScheduled {
		appendLog(ctx, p.Deps, post.ID, models.PostLogEventTaskSucceeded, post.Status, post.Status,
			"poll exited: post is no longer Scheduled", `{"reason":"status_changed"}`)
		return nil
	}
	if post.PublisherPostID == "" {
		appendLog(ctx, p.Deps, post.ID, models.PostLogEventTaskFailed, post.Status, post.Status,
			"poll exited: publisher_post_id is empty", `{}`)
		return nil
	}
	if task.PublisherPostID != "" && task.PublisherPostID != post.PublisherPostID {
		// The post was unscheduled and scheduled again: this poll belongs to a
		// Zernio post that no longer exists, and the new submission has its own.
		jobs.ZernioPollSuperseded.Add(1)
		appendLog(ctx, p.Deps, post.ID, models.PostLogEventTaskSucceeded, post.Status, post.Status,
			"poll exited: superseded by a newer submission", logs.MarshalCapped(map[string]string{
				"reason":                    "superseded",
				"task_publisher_post_id":    task.PublisherPostID,
				"current_publisher_post_id": post.PublisherPostID,
			}))
		return nil
	}
	held := post.PublisherPostID

	apiStart := time.Now()
	job, statusErr := p.Deps.Client.Status(ctx, post.PublisherPostID)
	jobs.ObserveZernioCall(time.Since(apiStart))
	if statusErr != nil {
		// Transient → retry. Terminal API errors during polling are
		// odd (404 if Zernio dropped the row) — log once and stop;
		// the reconciler will eventually time out the post.
		if zernio.IsTerminalAPIError(statusErr) {
			jobs.ZernioPollFailed.Add(1)
			appendLog(ctx, p.Deps, post.ID, models.PostLogEventZernioPoll, post.Status, post.Status,
				"poll terminal API error; awaiting reconcile", errPayload(statusErr))
			return nil
		}
		jobs.ZernioPollRetried.Add(1)
		appendLog(ctx, p.Deps, post.ID, models.PostLogEventZernioPoll, post.Status, post.Status,
			"poll transient error; River will retry", errPayload(statusErr))
		return statusErr
	}
	jobs.ZernioPollSucceeded.Add(1)

	// Persist Zernio's view of status so subsequent polls / debugging can see
	// what we last saw.
	statusChanged := post.PublisherStatus != string(job.Status)
	post.PublisherStatus = string(job.Status)
	post.UpdatedAt = time.Now().UTC()
	if !job.Status.IsTerminal() {
		return p.snoozeNonTerminal(ctx, post, held, statusChanged)
	}

	// Terminal state: published / failed / partial. Map to Ogen status
	// and persist per-platform results.
	from := post.Status
	switch job.Status {
	case zernio.JobStatusPublished:
		now := time.Now().UTC()
		post.PublishedAt = &now
		post.Status = models.PostStatusPublished
		results, _ := json.Marshal(job.Platforms)
		post.PublishedResults = string(results)
		// Lift the platform permalink out of the per-platform blob
		// into a first-class field so the front-end can render "View post"
		// without parsing published_results. A post targets a single platform,
		// so the first outcome carrying a URL is the one.
		for _, pl := range job.Platforms {
			if pl.PlatformPostURL != "" {
				post.PublishedURL = pl.PlatformPostURL
				break
			}
		}
		post.FirstCommentStatus = firstCommentOnPublish(post)
		ok, err := p.Deps.PostRepo.UpdateSubmission(ctx, post, held,
			"status", "published_at", "published_results", "published_url", "publisher_status",
			"first_comment_status", "updated_at")
		if err != nil {
			return fmt.Errorf("poll: persist Published: %w", err)
		}
		if !ok {
			return p.movedOn(ctx, post)
		}
		appendLog(ctx, p.Deps, post.ID, models.PostLogEventStateTransition, from, post.Status,
			"Zernio reported published", logs.MarshalCapped(map[string]any{
				"published_at": now,
				"platforms":    job.Platforms,
			}))
		p.Deps.ActivityRecorder.Record(ctx, activity.CategoryPublish, "publish_succeeded",
			activity.WithEntity("post", post.ID),
			activity.WithSource(activity.SourceJob),
			activity.WithStatus(string(from)+"->"+string(post.Status)),
		)
		// Tell the whole workspace it's live.
		emitPublishNotification(ctx, p.Notifier, p.Members, post, true)
		p.scheduleFirstComment(ctx, post, cmp.Or(job.PublishedAt, &now))
	case zernio.JobStatusFailed, zernio.JobStatusPartial:
		// `partial` = some platforms succeeded, others failed. Per
		// CON-69 we treat this as Failed for the MVP; per-platform
		// detail goes to the Post Log so the user can see what
		// landed and what didn't.
		post.Status = models.PostStatusFailed
		post.FailureReason = "zernio_terminal: " + string(job.Status)
		results, _ := json.Marshal(job.Platforms)
		post.PublishedResults = string(results)
		post.FirstCommentStatus = nil
		if post.SendsFirstComment() {
			post.FirstCommentStatus = new(models.FirstCommentSkipped)
		}
		ok, err := p.Deps.PostRepo.UpdateSubmission(ctx, post, held,
			"status", "failure_reason", "published_results", "publisher_status",
			"first_comment_status", "updated_at")
		if err != nil {
			return fmt.Errorf("poll: persist Failed: %w", err)
		}
		if !ok {
			return p.movedOn(ctx, post)
		}
		appendLog(ctx, p.Deps, post.ID, models.PostLogEventStateTransition, from, post.Status,
			fmt.Sprintf("Zernio reported %s", job.Status), logs.MarshalCapped(map[string]any{
				"zernio_status": job.Status,
				"platforms":     job.Platforms,
			}))
		p.Deps.ActivityRecorder.Record(ctx, activity.CategoryPublish, "publish_failed",
			activity.WithEntity("post", post.ID),
			activity.WithSource(activity.SourceJob),
			activity.WithStatus(string(from)+"->"+string(post.Status)),
			activity.WithPayload(map[string]any{"zernio_status": string(job.Status)}),
		)
		// Tell the whole workspace it failed to publish.
		emitPublishNotification(ctx, p.Notifier, p.Members, post, false)
	default:
		// Defensive: unknown terminal state.
		appendLog(ctx, p.Deps, post.ID, models.PostLogEventZernioPoll, post.Status, post.Status,
			"unknown Zernio terminal status — ignoring", logs.MarshalCapped(map[string]string{"zernio_status": string(job.Status)}))
	}
	return nil
}

// firstCommentOnPublish is the first-comment status of a post going live: sent
// with the post when it has no delay, pending when Ogen posts it later, nil
// when the post sends none.
func firstCommentOnPublish(post *models.Post) *models.FirstCommentStatus {
	switch {
	case !post.SendsFirstComment():
		return nil
	case post.FirstCommentDelayMinutes == 0:
		return new(models.FirstCommentDelegated)
	default:
		return new(models.FirstCommentPending)
	}
}

// scheduleFirstComment queues the delayed first comment of a post that just
// went live. A comment that can't be queued is settled as failed rather than
// left pending forever.
func (p *PollZernioStatusProcessor) scheduleFirstComment(ctx context.Context, post *models.Post, publishedAt *time.Time) {
	if post.FirstCommentStatus == nil || *post.FirstCommentStatus != models.FirstCommentPending {
		return
	}
	if err := enqueueFirstComment(ctx, post, *publishedAt); err != nil {
		fc := &PostFirstCommentProcessor{Deps: p.Deps, Notifier: p.Notifier}
		if ferr := fc.fail(ctx, post, "could not schedule the first comment: "+err.Error()); ferr != nil {
			appendLog(ctx, p.Deps, post.ID, models.PostLogEventTaskFailed, post.Status, post.Status,
				"failed to settle unscheduled first comment", errPayload(ferr))
		}
	}
}

// movedOn ends a poll whose write found the post no longer scheduled under
// this submission: it was cancelled, deleted, or another poll already landed
// the outcome. Writing anyway would restore a stale copy of the post.
// snoozeNonTerminal handles a poll that found the Zernio post not yet
// terminal: it records the new publisher status only when it moved (a
// scheduled post is polled every 30-60s until it publishes, and rewriting an
// unchanged status touched the row and its updated_at every poll), then
// snoozes this same job per the cadence rule. JobSnooze reschedules without
// consuming a retry attempt.
func (p *PollZernioStatusProcessor) snoozeNonTerminal(ctx context.Context, post *models.Post, held string, statusChanged bool) error {
	if statusChanged {
		ok, err := p.Deps.PostRepo.UpdateSubmission(ctx, post, held, "publisher_status", "updated_at")
		if err == nil && !ok {
			return p.movedOn(ctx, post)
		}
	}
	return river.JobSnooze(p.intervalFor(post))
}

func (p *PollZernioStatusProcessor) movedOn(ctx context.Context, post *models.Post) error {
	appendLog(ctx, p.Deps, post.ID, models.PostLogEventTaskSucceeded, models.PostStatusScheduled, models.PostStatusScheduled,
		"poll exited: post moved on while polling", `{"reason":"status_changed"}`)
	return nil
}

// pollLiveStates are the states a poll job can still run (or snooze back) from.
var pollLiveStates = []rivertype.JobState{
	rivertype.JobStateAvailable,
	rivertype.JobStatePending,
	rivertype.JobStateRetryable,
	rivertype.JobStateRunning,
	rivertype.JobStateScheduled,
}

// cancelPolls cancels the live poll_zernio_status jobs of one submission, so a
// withdrawn Zernio post leaves no poll waiting in the queue for it. Polls of a
// newer submission (another publisher_post_id) are kept; polls enqueued before
// the task carried publisher_post_id match any. A running poll is cancelled
// once its attempt ends. It runs inside a worker, on that worker's River
// client, and reports how many jobs it cancelled.
func cancelPolls(ctx context.Context, postID, publisherPostID string) (int, error) {
	client, err := river.ClientFromContextSafely[*sql.Tx](ctx)
	if err != nil {
		return 0, err
	}
	params := river.NewJobListParams().
		Kinds(PollZernioStatusQueue).
		States(pollLiveStates...).
		Where("args->>'post_id' = @post_id AND COALESCE(args->>'publisher_post_id', '') IN ('', @publisher_post_id)",
			river.NamedArgs{"post_id": postID, "publisher_post_id": publisherPostID}).
		First(100)
	res, err := client.JobList(ctx, params)
	if err != nil {
		return 0, fmt.Errorf("list polls of post %s: %w", postID, err)
	}
	cancelled := 0
	defer func() { jobs.ZernioPollCancelled.Add(int64(cancelled)) }()
	for _, job := range res.Jobs {
		row, err := client.JobCancel(ctx, job.ID)
		if errors.Is(err, rivertype.ErrNotFound) {
			continue
		}
		if err != nil {
			return cancelled, fmt.Errorf("cancel poll job %d: %w", job.ID, err)
		}
		// A running poll stays running until its attempt ends; only count
		// the ones River cancelled outright.
		if row.State == rivertype.JobStateCancelled {
			cancelled++
		}
	}
	return cancelled, nil
}

func (p *PollZernioStatusProcessor) intervalFor(post *models.Post) time.Duration {
	fast := p.FastInterval
	if fast <= 0 {
		fast = 30 * time.Second
	}
	slow := p.SlowInterval
	if slow <= 0 {
		slow = 60 * time.Second
	}
	window := p.FastWindow
	if window <= 0 {
		window = 5 * time.Minute
	}
	if post.ScheduledAt == nil {
		return fast
	}
	if time.Since(*post.ScheduledAt) <= window {
		return fast
	}
	return slow
}
