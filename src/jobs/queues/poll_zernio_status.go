package queues

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/riverqueue/river"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/publishers/zernio"
	"github.com/ogen-app/ogen/src/jobs"
	"github.com/ogen-app/ogen/src/kernel/activity"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
	"github.com/ogen-app/ogen/src/usecase/notify"
	"github.com/ogen-app/ogen/src/usecase/post_actions/logs"
)

// PollZernioStatusQueue is the River queue name (CON-69 §3, §7).
const PollZernioStatusQueue = "poll_zernio_status"

// PollZernioStatusTask carries the Ogen post id; the worker re-loads
// the row each cycle.
type PollZernioStatusTask struct {
	PostID string `json:"post_id"`
}

// Kind implements river.JobArgs.
func (PollZernioStatusTask) Kind() string { return PollZernioStatusQueue }

// InsertOpts sets per-kind defaults: 3 total attempts for transient errors.
// The non-terminal cadence is driven by river.JobSnooze in Process, not by
// retries (snooze bumps max_attempts so it never consumes the retry budget).
// Per-attempt timeout lives on the worker.
func (PollZernioStatusTask) InsertOpts() river.InsertOpts {
	return river.InsertOpts{MaxAttempts: 3}
}

// PollZernioStatusProcessor implements the recurring poll. The
// cadence per CON-69 §7: every 30s for the first 5 minutes after
// scheduled_at, then every 60s.
type PollZernioStatusProcessor struct {
	river.WorkerDefaults[PollZernioStatusTask]
	Deps ZernioDeps
	// Notifier drops a publish-outcome notification when Zernio reports a terminal
	// status (CON-242). Nil is a no-op.
	Notifier *notify.Service
	// Members fans the publish outcome across the workspace (CON-285). Nil falls
	// back to the author.
	Members      memberLister
	FastInterval time.Duration // default 30s
	SlowInterval time.Duration // default 60s
	FastWindow   time.Duration // default 5m after scheduled_at
}

// Work is the River entrypoint; it delegates to Process.
func (p *PollZernioStatusProcessor) Work(ctx context.Context, job *river.Job[PollZernioStatusTask]) error {
	ctx = WithJobRequestID(ctx, job.JobRow)
	// CON-97: background jobs span tenants (interim until per-tenant, PR4).
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
	// Scope the rest of the job to the owning tenant (CON-97 PR4).
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
				"poll terminal API error; awaiting reconcile", `{"error":"`+statusErr.Error()+`"}`)
			return nil
		}
		jobs.ZernioPollRetried.Add(1)
		appendLog(ctx, p.Deps, post.ID, models.PostLogEventZernioPoll, post.Status, post.Status,
			"poll transient error; River will retry", `{"error":"`+statusErr.Error()+`"}`)
		return statusErr
	}
	jobs.ZernioPollSucceeded.Add(1)

	// Persist Zernio's view of status so subsequent polls / debugging
	// can see what we last saw. Always write — cheap and useful.
	post.PublisherStatus = string(job.Status)
	if !job.Status.IsTerminal() {
		_ = p.Deps.PostRepo.Update(ctx, post)
		// Not terminal yet — snooze this same job per the cadence rule.
		// JobSnooze reschedules without consuming a retry attempt.
		return river.JobSnooze(p.intervalFor(post))
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
		// CON-165: lift the platform permalink out of the per-platform blob
		// into a first-class field so the front-end can render "View post"
		// without parsing published_results. A post targets a single platform,
		// so the first outcome carrying a URL is the one.
		for _, pl := range job.Platforms {
			if pl.PlatformPostURL != "" {
				post.PublishedURL = pl.PlatformPostURL
				break
			}
		}
		if err := p.Deps.PostRepo.Update(ctx, post); err != nil {
			return fmt.Errorf("poll: persist Published: %w", err)
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
		// CON-242/CON-285: tell the whole workspace it's live.
		emitPublishNotification(ctx, p.Notifier, p.Members, post, true)
	case zernio.JobStatusFailed, zernio.JobStatusPartial:
		// `partial` = some platforms succeeded, others failed. Per
		// CON-69 we treat this as Failed for the MVP; per-platform
		// detail goes to the Post Log so the user can see what
		// landed and what didn't.
		post.Status = models.PostStatusFailed
		post.FailureReason = "zernio_terminal: " + string(job.Status)
		results, _ := json.Marshal(job.Platforms)
		post.PublishedResults = string(results)
		if err := p.Deps.PostRepo.Update(ctx, post); err != nil {
			return fmt.Errorf("poll: persist Failed: %w", err)
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
		// CON-242/CON-285: tell the whole workspace it failed to publish.
		emitPublishNotification(ctx, p.Notifier, p.Members, post, false)
	default:
		// Defensive: unknown terminal state.
		appendLog(ctx, p.Deps, post.ID, models.PostLogEventZernioPoll, post.Status, post.Status,
			"unknown Zernio terminal status — ignoring", `{"zernio_status":"`+string(job.Status)+`"}`)
	}
	return nil
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
