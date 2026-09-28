package queues

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/riverqueue/river"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/publishers/zernio"
	"github.com/ogen-app/ogen/src/jobs"
	"github.com/ogen-app/ogen/src/kernel/activity"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
	"github.com/ogen-app/ogen/src/usecase/post_actions/logs"
)

// CancelZernioJobQueue is the River queue name.
const CancelZernioJobQueue = "cancel_zernio_job"

// CancelTarget is what the user wants the Post moved to once
// cancellation succeeds — back to the editing state (ReadyForPublish),
// all the way to Draft, or converted to manual publishing
// (ScheduledForManualPublish) which keeps scheduled_at but stops the
// auto-publish.
type CancelTarget string

const (
	CancelTargetReadyForPublish CancelTarget = "ready_for_publish"
	CancelTargetDraft           CancelTarget = "draft"
	CancelTargetManualPublish   CancelTarget = "scheduled_for_manual_publishing"
)

// landingStatus maps a CancelTarget to the Post status the worker lands
// the post on after a successful cancel. Unknown or empty targets fall back
// to ReadyForPublish.
func (t CancelTarget) landingStatus() models.PostStatus {
	switch t {
	case CancelTargetDraft:
		return models.PostStatusDraft
	case CancelTargetManualPublish:
		return models.PostStatusScheduledForManualPublish
	default:
		return models.PostStatusReadyForPublish
	}
}

// CancelZernioJobTask carries the post id and the user's chosen
// post-cancel landing state.
type CancelZernioJobTask struct {
	PostID string       `json:"post_id"`
	Target CancelTarget `json:"target"`
	// Actor is the user id that requested cancellation; recorded on
	// the PostLog entries this task emits so the audit trail traces
	// back to the human, not the queue runner.
	Actor string `json:"actor"`
}

// Kind implements river.JobArgs.
func (CancelZernioJobTask) Kind() string { return CancelZernioJobQueue }

// InsertOpts sets per-kind defaults: 3 total attempts. Per-attempt timeout
// lives on the worker (cancelZernioJobWorker.Timeout).
func (CancelZernioJobTask) InsertOpts() river.InsertOpts {
	return river.InsertOpts{MaxAttempts: 3}
}

// CancelZernioJobProcessor is the River worker for cancel_zernio_job.
type CancelZernioJobProcessor struct {
	river.WorkerDefaults[CancelZernioJobTask]
	Deps ZernioDeps
}

// Work is the River entrypoint; it delegates to Process.
func (p *CancelZernioJobProcessor) Work(ctx context.Context, job *river.Job[CancelZernioJobTask]) error {
	ctx = WithJobRequestID(ctx, job.JobRow)
	// Background jobs span tenants (interim until per-tenant, PR4).
	ctx = tenantctx.WithSystem(ctx)
	return p.Process(ctx, job.Args)
}

// Timeout is the per-attempt context deadline.
func (p *CancelZernioJobProcessor) Timeout(*river.Job[CancelZernioJobTask]) time.Duration {
	return 20 * time.Second
}

func init() {
	register(func(w *river.Workers, d Deps) {
		river.AddWorker(w, &CancelZernioJobProcessor{Deps: d.Zernio})
	})
}

// Process executes one cancellation attempt.
//
// Outcome matrix:
//   - Cancel succeeds → Post transitions to the chosen target.
//   - Cancel returns ErrAlreadyPublished → no transition; the next
//     poll cycle will land Published per the normal success path.
//   - Cancel returns transient error → return error so River
//     retries.
//   - Cancel returns terminal error → log and leave Post in
//     Scheduled; user sees a stuck post and can try again. Per
//     CON-69 §9 we surface the failure rather than guessing.
func (p *CancelZernioJobProcessor) Process(ctx context.Context, task CancelZernioJobTask) error {
	post, err := p.Deps.PostRepo.GetByID(ctx, task.PostID)
	if err != nil {
		return fmt.Errorf("cancel: load post %s: %w", task.PostID, err)
	}
	// Scope the rest of the job to the owning tenant.
	ctx = tenantctx.With(ctx, post.TenantID)
	if post.Status != models.PostStatusScheduled {
		// User raced themselves, or the poller landed Published first.
		appendLogActor(ctx, p.Deps, post.ID, task.Actor, models.PostLogEventTaskSucceeded, post.Status, post.Status,
			"cancel aborted: post is no longer Scheduled", `{"reason":"status_changed"}`)
		return nil
	}
	if post.PublisherPostID == "" {
		// Submit never landed; just transition locally per the user's
		// choice. Don't call Zernio.
		return p.transition(ctx, post, task)
	}

	apiStart := time.Now()
	cancelErr := p.Deps.Client.Cancel(ctx, post.PublisherPostID)
	jobs.ObserveZernioCall(time.Since(apiStart))
	if cancelErr == nil {
		jobs.ZernioCancelSucceeded.Add(1)
		appendLogActor(ctx, p.Deps, post.ID, task.Actor, models.PostLogEventZernioCancel, post.Status, post.Status,
			"Zernio cancel succeeded", `{}`)
		return p.transition(ctx, post, task)
	}
	if errors.Is(cancelErr, zernio.ErrAlreadyPublished) {
		// Race: Zernio published before we could cancel. Don't move
		// the Post locally — let the next poll land Published.
		jobs.ZernioCancelAlreadyPublished.Add(1)
		appendLogActor(ctx, p.Deps, post.ID, task.Actor, models.PostLogEventZernioCancel, post.Status, post.Status,
			"cancel race: Zernio reports already published", `{"resolution":"poll_will_land_published"}`)
		return nil
	}
	if zernio.IsTerminalAPIError(cancelErr) {
		jobs.ZernioCancelFailed.Add(1)
		appendLogActor(ctx, p.Deps, post.ID, task.Actor, models.PostLogEventZernioCancel, post.Status, post.Status,
			"Zernio cancel terminally failed", errPayload(cancelErr))
		return nil
	}
	// Transient — retry per InsertOpts.
	appendLogActor(ctx, p.Deps, post.ID, task.Actor, models.PostLogEventTaskRetried, post.Status, post.Status,
		"transient cancel error; River will retry", errPayload(cancelErr))
	return cancelErr
}

func (p *CancelZernioJobProcessor) transition(ctx context.Context, post *models.Post, task CancelZernioJobTask) error {
	from := post.Status
	to := task.Target.landingStatus()
	if !from.CanTransition(to) {
		appendLogActor(ctx, p.Deps, post.ID, task.Actor, models.PostLogEventStateTransitionBlocked, from, to,
			"cancel target rejected by state machine", `{"reason":"invalid_transition"}`)
		return nil
	}
	// Converting to manual publishing keeps scheduled_at so the intended
	// publish date still shows against the post — we only stop the
	// auto-dispatch. transition() never touches scheduled_at, so this is
	// automatic; the comment records the intent.
	post.Status = to
	if err := p.Deps.PostRepo.Update(ctx, post); err != nil {
		return fmt.Errorf("cancel: persist new status: %w", err)
	}
	summary, action := "post cancelled and transitioned", "publish_cancelled"
	if task.Target == CancelTargetManualPublish {
		summary, action = "converted to manual publishing", "converted_to_manual"
	}
	appendLogActor(ctx, p.Deps, post.ID, task.Actor, models.PostLogEventStateTransition, from, to,
		summary, logs.MarshalCapped(map[string]any{
			"target": string(to),
		}))
	p.Deps.ActivityRecorder.Record(ctx, activity.CategoryPublish, action,
		activity.WithEntity("post", post.ID),
		activity.WithUser(task.Actor),
		activity.WithSource(activity.SourceJob),
		activity.WithStatus(string(from)+"->"+string(to)),
	)
	return nil
}

// appendLogActor is the same as appendLog but lets a queue attribute
// the entry to a specific user (the cancellation requester) rather
// than the system actor.
func appendLogActor(
	ctx context.Context,
	deps ZernioDeps,
	postID string,
	actor string,
	evt models.PostLogEventType,
	from, to models.PostStatus,
	summary, payload string,
) {
	if deps.PostLogRepo == nil {
		return
	}
	id, _ := models.NewID()
	if id == "" {
		return
	}
	if actor == "" {
		actor = models.ActorSystem
	}
	fromCopy := from
	toCopy := to
	_ = deps.PostLogRepo.Append(ctx, &models.PostLog{
		ID:         id,
		PostID:     postID,
		EventType:  evt,
		Actor:      actor,
		FromStatus: &fromCopy,
		ToStatus:   &toCopy,
		Summary:    summary,
		Payload:    logs.SanitizeAndCap(payload),
	})
}
