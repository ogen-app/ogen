package queues

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"time"

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

// Kind implements JobArgs.
func (PollZernioStatusTask) Kind() string { return PollZernioStatusQueue }

// PollZernioStatusProcessor implements the recurring poll. The
// cadence per CON-69 §7: every 30s for the first 5 minutes after
// scheduled_at, then every 60s.
type PollZernioStatusProcessor struct {
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

// Process executes one poll cycle in-process. On a non-terminal status it
// returns river.JobSnooze to reschedule this same job per the cadence rule; it
// returns a non-nil error only on transient Zernio failures so River retries
// per its backoff policy; otherwise nil (terminal handled, or quietly exited).
func (p *PollZernioStatusProcessor) Process(ctx context.Context, task PollZernioStatusTask) error {
	return runLocal(ctx, p.step, PollZernioStatusQueue, task, 0, 0, localActions{zernio: p.Deps.Client})
}

// step reads the Zernio post's status in one call, then lands a terminal
// status or snoozes until the next poll.
func (p *PollZernioStatusProcessor) step(ctx context.Context, in StepInput) (StepOutput, error) {
	var task PollZernioStatusTask
	if err := json.Unmarshal(in.Args, &task); err != nil {
		return done("skipped_bad_args")
	}
	// Background jobs span tenants (interim until per-tenant, PR4).
	ctx = tenantctx.WithSystem(ctx)
	post, err := p.Deps.PostRepo.GetByID(ctx, task.PostID)
	if err != nil {
		return retry("poll: load post "+task.PostID, err)
	}
	// Scope the rest of the job to the owning tenant.
	ctx = tenantctx.With(ctx, post.TenantID)
	if post.Status != models.PostStatusScheduled {
		appendLog(ctx, p.Deps, post.ID, models.PostLogEventTaskSucceeded, post.Status, post.Status,
			"poll exited: post is no longer Scheduled", `{"reason":"status_changed"}`)
		return done("exited_status_changed")
	}
	if post.PublisherPostID == "" {
		appendLog(ctx, p.Deps, post.ID, models.PostLogEventTaskFailed, post.Status, post.Status,
			"poll exited: publisher_post_id is empty", `{}`)
		return done("exited_no_zernio_post")
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
		return done("exited_superseded")
	}
	held := post.PublisherPostID

	var job *zernio.Job
	status := func(c *zernio.Client) error {
		var err error
		job, err = c.Status(ctx, held)
		return err
	}
	if in.Result == nil {
		call, err := zernio.Plan(status)
		if err != nil {
			return StepOutput{}, err
		}
		return nextZernio(nil, call)
	}
	if zernioDisabled(in.Result) {
		appendLog(ctx, p.Deps, post.ID, models.PostLogEventZernioPoll, post.Status, post.Status,
			"poll skipped: the Zernio integration is off; awaiting reconcile", `{"reason":"zernio_disabled"}`)
		return done("skipped_zernio_disabled")
	}
	statusErr := zernio.Decode(zernioResult(in.Result), status)
	return p.land(ctx, post, held, job, statusErr)
}

// land applies one poll's answer.
func (p *PollZernioStatusProcessor) land(ctx context.Context, post *models.Post, held string, job *zernio.Job, statusErr error) (StepOutput, error) {
	if statusErr != nil {
		// Transient → retry. Terminal API errors during polling are
		// odd (404 if Zernio dropped the row) — log once and stop;
		// the reconciler will eventually time out the post.
		if zernio.IsTerminalAPIError(statusErr) {
			jobs.ZernioPollFailed.Add(1)
			appendLog(ctx, p.Deps, post.ID, models.PostLogEventZernioPoll, post.Status, post.Status,
				"poll terminal API error; awaiting reconcile", errPayload(statusErr))
			return done("failed")
		}
		jobs.ZernioPollRetried.Add(1)
		appendLog(ctx, p.Deps, post.ID, models.PostLogEventZernioPoll, post.Status, post.Status,
			"poll transient error; River will retry", errPayload(statusErr))
		return retry("poll", statusErr)
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
			return retry("poll: persist Published", err)
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
			return retry("poll: persist Failed", err)
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
	return done(string(job.Status))
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
	if err := enqueueFirstComment(ctx, p.Deps.Jobs, post, *publishedAt); err != nil {
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
func (p *PollZernioStatusProcessor) snoozeNonTerminal(ctx context.Context, post *models.Post, held string, statusChanged bool) (StepOutput, error) {
	if statusChanged {
		ok, err := p.Deps.PostRepo.UpdateSubmission(ctx, post, held, "publisher_status", "updated_at")
		if err == nil && !ok {
			return p.movedOn(ctx, post)
		}
	}
	return StepOutput{Retry: &StepRetry{Reason: "not yet published", After: p.intervalFor(post)}}, nil
}

func (p *PollZernioStatusProcessor) movedOn(ctx context.Context, post *models.Post) (StepOutput, error) {
	appendLog(ctx, p.Deps, post.ID, models.PostLogEventTaskSucceeded, models.PostStatusScheduled, models.PostStatusScheduled,
		"poll exited: post moved on while polling", `{"reason":"status_changed"}`)
	return done("exited_moved_on")
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
