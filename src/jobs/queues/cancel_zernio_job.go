package queues

import (
	"context"
	"encoding/json"
	"errors"

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

// Kind implements JobArgs.
func (CancelZernioJobTask) Kind() string { return CancelZernioJobQueue }

// CancelZernioJobProcessor runs cancel_zernio_job.
type CancelZernioJobProcessor struct {
	Deps ZernioDeps
}

// Process executes one cancellation attempt in-process.
func (p *CancelZernioJobProcessor) Process(ctx context.Context, task CancelZernioJobTask) error {
	return runLocal(ctx, p.step, CancelZernioJobQueue, task, 0, 0, localActions{zernio: p.Deps.Client})
}

// step cancels the post's Zernio submission in one call, then lands the post.
//
// Outcome matrix:
//   - Cancel succeeds → Post transitions to the chosen target and drops
//     its publisher_post_id.
//   - Cancel returns ErrPostNotFound → same as success: the Zernio post
//     is already gone.
//   - Cancel returns ErrAlreadyPublished → no transition; the next
//     poll cycle will land Published per the normal success path.
//   - Cancel returns transient error → retry.
//   - Cancel returns terminal error → log and leave Post in
//     Scheduled; user sees a stuck post and can try again. Per
//     CON-69 §9 we surface the failure rather than guessing.
func (p *CancelZernioJobProcessor) step(ctx context.Context, in StepInput) (StepOutput, error) {
	var task CancelZernioJobTask
	if err := json.Unmarshal(in.Args, &task); err != nil {
		return done("skipped_bad_args")
	}
	ctx = tenantctx.WithSystem(ctx)
	post, err := p.Deps.PostRepo.GetRowByID(ctx, task.PostID)
	if err != nil {
		return retry("cancel: load post "+task.PostID, err)
	}
	// Scope the rest of the job to the owning tenant.
	ctx = tenantctx.With(ctx, post.TenantID)
	if post.Status != models.PostStatusScheduled {
		// User raced themselves, or the poller landed Published first.
		appendLogActor(ctx, p.Deps, post.ID, task.Actor, models.PostLogEventTaskSucceeded, post.Status, post.Status,
			"cancel aborted: post is no longer Scheduled", `{"reason":"status_changed"}`)
		return done("aborted_status_changed")
	}
	if post.PublisherPostID == "" {
		// Submit never landed; just transition locally per the user's
		// choice. Don't call Zernio.
		return p.transition(ctx, post, task)
	}

	cancel := func(c *zernio.Client) error { return c.Cancel(ctx, post.PublisherPostID) }
	if in.Result == nil {
		call, err := zernio.Plan(cancel)
		if err != nil {
			return StepOutput{}, err
		}
		return nextZernio(nil, call)
	}
	if zernioDisabled(in.Result) {
		appendLogActor(ctx, p.Deps, post.ID, task.Actor, models.PostLogEventZernioCancel, post.Status, post.Status,
			"Zernio cancel not attempted: the integration is off", `{"reason":"zernio_disabled"}`)
		return done("skipped_zernio_disabled")
	}
	cancelErr := zernio.Decode(zernioResult(in.Result), cancel)
	switch {
	case cancelErr == nil:
		jobs.ZernioCancelSucceeded.Add(1)
		appendLogActor(ctx, p.Deps, post.ID, task.Actor, models.PostLogEventZernioCancel, post.Status, post.Status,
			"Zernio cancel succeeded", publisherPostIDPayload(post.PublisherPostID))
		return p.transition(ctx, post, task)
	case errors.Is(cancelErr, zernio.ErrPostNotFound):
		// Nothing left on Zernio to cancel or to publish: the local
		// transition is all that remains.
		jobs.ZernioCancelSucceeded.Add(1)
		appendLogActor(ctx, p.Deps, post.ID, task.Actor, models.PostLogEventZernioCancel, post.Status, post.Status,
			"Zernio post already gone; cancelling locally", publisherPostIDPayload(post.PublisherPostID))
		return p.transition(ctx, post, task)
	case errors.Is(cancelErr, zernio.ErrAlreadyPublished):
		// Race: Zernio published before we could cancel. Don't move
		// the Post locally — let the next poll land Published.
		jobs.ZernioCancelAlreadyPublished.Add(1)
		appendLogActor(ctx, p.Deps, post.ID, task.Actor, models.PostLogEventZernioCancel, post.Status, post.Status,
			"cancel race: Zernio reports already published", `{"resolution":"poll_will_land_published"}`)
		return done("already_published")
	case zernio.IsTerminalAPIError(cancelErr):
		jobs.ZernioCancelFailed.Add(1)
		appendLogActor(ctx, p.Deps, post.ID, task.Actor, models.PostLogEventZernioCancel, post.Status, post.Status,
			"Zernio cancel terminally failed", errPayload(cancelErr))
		return done("failed")
	default:
		appendLogActor(ctx, p.Deps, post.ID, task.Actor, models.PostLogEventTaskRetried, post.Status, post.Status,
			"transient cancel error; River will retry", errPayload(cancelErr))
		return retry("cancel", cancelErr)
	}
}

// transition lands the post on the cancel target. It is reached only once the
// Zernio post is deleted (or never existed), so it also drops the publisher
// identity: a later schedule must create a fresh Zernio post, not retry a
// deleted one. The deleted submission's pending status polls are cancelled
// with it.
func (p *CancelZernioJobProcessor) transition(ctx context.Context, post *models.Post, task CancelZernioJobTask) (StepOutput, error) {
	from := post.Status
	to := task.Target.landingStatus()
	heldID := post.PublisherPostID
	post.PublisherPostID = ""
	post.PublisherStatus = ""
	if !from.CanTransition(to) {
		appendLogActor(ctx, p.Deps, post.ID, task.Actor, models.PostLogEventStateTransitionBlocked, from, to,
			"cancel target rejected by state machine", `{"reason":"invalid_transition"}`)
		if heldID == "" {
			return done("blocked")
		}
		if err := p.Deps.PostRepo.Update(ctx, post); err != nil {
			return retry("cancel: clear publisher_post_id", err)
		}
		return p.dropPolls(ctx, post, heldID, task.Actor, "blocked")
	}
	// Converting to manual publishing keeps scheduled_at so the intended
	// publish date still shows against the post — we only stop the
	// auto-dispatch. transition() never touches scheduled_at, so this is
	// automatic; the comment records the intent.
	post.Status = to
	if err := p.Deps.PostRepo.Update(ctx, post); err != nil {
		return retry("cancel: persist new status", err)
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
	if heldID == "" {
		return done("cancelled")
	}
	return p.dropPolls(ctx, post, heldID, task.Actor, "cancelled")
}

// dropPolls ends the job by cancelling the status polls still queued for the
// Zernio post the cancel just deleted. Best effort: a poll left behind exits
// on its own when it wakes to a post that is no longer scheduled under that
// submission.
func (p *CancelZernioJobProcessor) dropPolls(ctx context.Context, post *models.Post, heldID, actor, outcome string) (StepOutput, error) {
	appendLogActor(ctx, p.Deps, post.ID, actor, models.PostLogEventTaskSucceeded, post.Status, post.Status,
		"pending status polls cancelled", publisherPostIDPayload(heldID))
	out, _ := done(outcome)
	out.Done.Cancel = []JobMatch{pollMatch(post.ID, heldID)}
	return out, nil
}

func publisherPostIDPayload(id string) string {
	return logs.MarshalCapped(map[string]string{"publisher_post_id": id})
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
