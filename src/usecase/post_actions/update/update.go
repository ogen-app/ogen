// Package update applies a whole-record post edit (PUT /api/posts/:id): the
// status state machine, the submitted-content lock, phase ownership, the
// draft → ready_for_publish publish gate, and the two persist paths (plain
// update, or ready_for_publish → scheduled via the schedule service), with the
// audit log and activity events each step emits.
package update

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/domain/platforms"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/activity"
	"github.com/ogen-app/ogen/src/usecase/post_actions/logs"
	"github.com/ogen-app/ogen/src/usecase/post_actions/schedule"
)

// ErrScheduleChanged means a scheduled post was cancelled, published or
// resubmitted while the edit was in flight, so the edit was not applied.
var ErrScheduleChanged = errors.New("the post's schedule changed while it was being edited; reload it and try again")

// ErrInvalidPhase means the post's campaign_type_phase_id is not a phase of
// its campaign's type.
var ErrInvalidPhase = errors.New("campaign_type_phase_id is not a phase of the campaign's type")

// TransitionError is a status change the post state machine forbids.
type TransitionError struct{ From, To models.PostStatus }

func (e *TransitionError) Error() string {
	return "invalid status transition from " + string(e.From) + " to " + string(e.To)
}

// ContentLockedError is a content edit on a submitted (scheduled or published)
// post. A copy of such a post exists outside Ogen — Zernio's snapshot or the
// network's published post — so rewriting the body/title/media/platform/post
// type/sources would silently diverge from what goes, or went, out.
type ContentLockedError struct{ Status models.PostStatus }

func (e *ContentLockedError) Error() string {
	return "post has been submitted (" + string(e.Status) + ") and its content is locked; unschedule to edit"
}

// LeaveScheduledError is a PUT moving a scheduled post to another status. Its
// copy is queued in Zernio, so it has to leave through POST /cancel or
// /convert-to-manual, which withdraw that copy first.
type LeaveScheduledError struct{ To models.PostStatus }

func (e *LeaveScheduledError) Error() string {
	return "a scheduled post can't be moved to " + string(e.To) +
		" by an edit; unschedule it with POST /api/posts/:id/cancel (or convert it to manual publishing)"
}

// ValidationError is a request that is malformed for the target status.
type ValidationError struct{ Msg string }

func (e *ValidationError) Error() string { return e.Msg }

// NotReadyError is a draft → ready_for_publish move the platform publish gate
// rejected.
type NotReadyError struct {
	PlatformValidation map[string][]platforms.ValidationError
}

func (e *NotReadyError) Error() string { return "post is not ready for publish" }

// Service applies post edits. Every dependency except Posts is optional:
// nil Platforms leaves thread limits unknown, nil Attachments skips the
// publish gate, nil Campaigns leaves phase ownership to the DB trigger, nil
// Logs/Activity drop audit and analytics events, and nil Schedule persists a
// move to scheduled as a plain status update.
type Service struct {
	Posts       repository.PostRepository
	Platforms   repository.PlatformRepository
	Attachments repository.PostAttachmentRepository
	Campaigns   repository.CampaignRepository
	Logs        repository.PostLogRepository
	Schedule    *schedule.Service
	Activity    *activity.Recorder
}

// Input is one edit. Apply copies the request's fields (including Status) onto
// the post; the remaining fields are the incoming values the checks need
// before Apply runs.
type Input struct {
	Post   *models.Post
	Status models.PostStatus
	Apply  func(*models.Post)

	CampaignID       string
	PhaseID          *string
	PlatformID       string
	PlatformPostType string
	Content          string
	CTAUrl           string
	// ScheduledAt and SocialAccountID are the incoming date and account,
	// which Zernio has already taken for a scheduled post.
	ScheduledAt     *time.Time
	SocialAccountID string
	// FirstComment and FirstCommentDelayMinutes are the incoming first
	// comment; nil leaves the stored one.
	FirstComment             *string
	FirstCommentDelayMinutes *int

	// MutatesLockedContent reports whether the request changes content that
	// is frozen once the post is submitted.
	MutatesLockedContent bool
	// Omit lists columns the whole-record UPDATE must not write back.
	Omit []string
	// Actor is recorded on audit log entries and the schedule transition.
	Actor string
}

// Result is the outcome of a successful edit. AutoPublishDecision is set when
// the schedule service routed the post, even if the final re-fetch failed.
type Result struct {
	Post                *models.Post
	AutoPublishDecision string
}

// Update validates and persists in, returning the re-fetched, fully hydrated
// post. Errors are *TransitionError, *ContentLockedError, *ValidationError,
// ErrInvalidPhase, *NotReadyError, *schedule.AccountSelectionError, or a
// repository failure.
func (s *Service) Update(ctx context.Context, in Input) (Result, error) {
	if err := s.check(ctx, in); err != nil {
		return Result{}, err
	}
	post := in.Post
	prev := post.Status
	held := post.PublisherPostID
	in.Apply(post)
	s.DeriveThreadSegments(ctx, post)

	var res Result
	var err error
	switch {
	case prev == models.PostStatusReadyForPublish && in.Status == models.PostStatusScheduled && s.Schedule != nil:
		// The schedule service consults the auto-publish allowlist and persists
		// status, audit log and the submit job in one transaction, so the
		// REST/assistant/PUT scheduling paths can't drift. It logs the
		// transition itself.
		res.AutoPublishDecision, err = s.Schedule.RouteAndPersist(ctx, post, prev, in.Actor)
	case prev == models.PostStatusScheduled:
		// A cancel or publish can land while the request is in flight; the
		// whole-record write would then restore the stale scheduled post.
		if err = s.updateWhileScheduled(ctx, post, held, in.Omit); err == nil {
			s.logTransition(ctx, in.Actor, post, prev, in.Status)
		}
	default:
		if err = s.Posts.Update(ctx, post, in.Omit...); err == nil {
			s.logTransition(ctx, in.Actor, post, prev, in.Status)
		}
	}
	if err != nil {
		if repository.IsConstraintViolation(err, repository.ConstraintPhaseMatchesCampaignType) {
			return res, ErrInvalidPhase
		}
		return res, err
	}

	res.Post, err = s.Posts.GetByID(ctx, post.ID)
	return res, err
}

// check runs the pre-persist rules in order; the first failure wins.
func (s *Service) check(ctx context.Context, in Input) error {
	post := in.Post
	if !post.Status.CanTransition(in.Status) {
		from, to := post.Status, in.Status
		s.LogEvent(ctx, in.Actor, post.ID, models.PostLogEventStateTransitionBlocked, &from, &to,
			"transition rejected by state machine",
			logs.MarshalCapped(map[string]any{"reason": "invalid_transition"}),
		)
		return &TransitionError{From: from, To: to}
	}
	if post.Status == models.PostStatusScheduled {
		if err := s.checkScheduled(ctx, in); err != nil {
			return err
		}
	}
	// A no-op save still passes; only a real content change is rejected.
	if post.Status.IsSubmitted() && in.MutatesLockedContent {
		return &ContentLockedError{Status: post.Status}
	}
	if err := RequirePlatformIfNotDraft(in.Status, in.PlatformID, in.PlatformPostType); err != nil {
		return &ValidationError{Msg: err.Error()}
	}
	if err := s.checkPhase(ctx, in); err != nil {
		return err
	}
	return s.checkReadyForPublish(ctx, in)
}

// checkScheduled guards a scheduled post, whose copy Zernio already holds with
// its date and account. Moving it off scheduled here would leave that copy
// queued to publish, so only the cancel and convert-to-manual flows, which
// withdraw it first, may. Retiming or re-accounting it would change what Ogen
// shows and not what publishes.
func (s *Service) checkScheduled(ctx context.Context, in Input) error {
	post := in.Post
	if in.Status != models.PostStatusScheduled {
		from, to := post.Status, in.Status
		s.LogEvent(ctx, in.Actor, post.ID, models.PostLogEventStateTransitionBlocked, &from, &to,
			"leaving scheduled must go through the cancel endpoint",
			logs.MarshalCapped(map[string]any{"reason": "use_cancel_endpoint"}),
		)
		return &LeaveScheduledError{To: to}
	}
	if !sameSecond(in.ScheduledAt, post.ScheduledAt) || in.SocialAccountID != post.SocialAccountID {
		return &ContentLockedError{Status: post.Status}
	}
	return nil
}

// updateWhileScheduled writes the edit only if the post is still scheduled
// under the submission the request loaded, and reports ErrScheduleChanged
// otherwise.
func (s *Service) updateWhileScheduled(ctx context.Context, post *models.Post, held string, omit []string) error {
	ok, err := s.Posts.UpdateWhileScheduled(ctx, post, held, omit...)
	if err != nil {
		return err
	}
	if !ok {
		return ErrScheduleChanged
	}
	return nil
}

// sameSecond compares two optional instants at second precision, so a client
// echoing back a timestamp at millisecond precision isn't read as a change.
func sameSecond(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Truncate(time.Second).Equal(b.Truncate(time.Second))
}

// checkPhase verifies a (re)assigned phase — or a move to another campaign
// keeping one — is a phase of the target campaign's type.
func (s *Service) checkPhase(ctx context.Context, in Input) error {
	post := in.Post
	if in.PhaseID == nil {
		return nil
	}
	unchanged := in.CampaignID == post.CampaignID && post.CampaignTypePhaseID != nil && *in.PhaseID == *post.CampaignTypePhaseID
	if unchanged {
		return nil
	}
	ok, err := s.PhaseBelongs(ctx, in.CampaignID, in.PhaseID)
	if err != nil {
		return err
	}
	if !ok {
		return ErrInvalidPhase
	}
	return nil
}

// PhaseBelongs reports whether phaseID (when set) is a phase of the campaign's
// type. Without a campaign repository it passes and the DB trigger decides.
func (s *Service) PhaseBelongs(ctx context.Context, campaignID string, phaseID *string) (bool, error) {
	if phaseID == nil || s.Campaigns == nil {
		return true, nil
	}
	return s.Campaigns.PhaseBelongsToCampaign(ctx, campaignID, *phaseID)
}

// checkReadyForPublish runs the attachment + post-type publish gate when a
// draft moves to ready_for_publish, logging the verdict either way.
func (s *Service) checkReadyForPublish(ctx context.Context, in Input) error {
	post := in.Post
	if post.Status != models.PostStatusDraft || in.Status != models.PostStatusReadyForPublish || s.Attachments == nil {
		return nil
	}
	atts, err := s.Attachments.ListByPostID(ctx, post.ID)
	if err != nil {
		return err
	}
	// Validate against what is about to be persisted: the incoming platform
	// (a draft can switch platforms in the same save, and the row isn't
	// written yet), post type, body and link, with thread segments derived using
	// that platform's per-segment limit.
	platform := post.Platform
	if in.PlatformID != "" && (platform == nil || platform.ID != in.PlatformID) && s.Platforms != nil {
		if fresh, perr := s.Platforms.GetByID(ctx, in.PlatformID); perr == nil {
			platform = fresh
		}
	}
	incoming := *post
	incoming.PlatformPostType = in.PlatformPostType
	incoming.Content = in.Content
	incoming.CTAUrl = in.CTAUrl
	if in.FirstComment != nil {
		incoming.FirstComment = *in.FirstComment
	}
	if in.FirstCommentDelayMinutes != nil {
		incoming.FirstCommentDelayMinutes = *in.FirstCommentDelayMinutes
	}
	ApplyThreadSegments(&incoming, ThreadLimitOf(platform))
	errsByPlatform := platforms.ValidatePublishReadiness(&incoming, platform, atts)

	from, to := post.Status, in.Status
	payload := logs.MarshalCapped(map[string]any{"platform_validation": errsByPlatform})
	if HasValidationErrors(errsByPlatform) {
		s.LogEvent(ctx, in.Actor, post.ID, models.PostLogEventValidationFailed, &from, &to,
			"draft → ready_for_publish blocked by platform validation", payload)
		s.record(ctx, "post_validation_failed",
			activity.WithEntity("post", post.ID),
			activity.WithStatus("failed"),
		)
		return &NotReadyError{PlatformValidation: errsByPlatform}
	}
	s.LogEvent(ctx, in.Actor, post.ID, models.PostLogEventValidationPassed, &from, &to,
		"draft → ready_for_publish passed platform validation", payload)
	return nil
}

// logTransition writes the state-transition entry (and the manual-retry entry
// for failed → ready_for_publish) after a plain update. An unchanged status
// is not a state-machine event.
func (s *Service) logTransition(ctx context.Context, actor string, post *models.Post, prev, next models.PostStatus) {
	if prev == next {
		return
	}
	s.LogEvent(ctx, actor, post.ID, models.PostLogEventStateTransition, &prev, &next,
		"status changed via PUT /api/posts/:id", "{}")
	s.record(ctx, "post_state_transition",
		activity.WithEntity("post", post.ID),
		activity.WithStatus(string(prev)+"->"+string(next)),
	)
	if prev == models.PostStatusFailed && next == models.PostStatusReadyForPublish {
		s.LogEvent(ctx, actor, post.ID, models.PostLogEventUserRetry, &prev, &next,
			"manual retry: user moved Failed → ReadyForPublish",
			logs.MarshalCapped(map[string]any{
				"prior_failure_reason":    post.FailureReason,
				"prior_publisher_post_id": post.PublisherPostID,
			}),
		)
	}
}

// LogEvent appends a post audit-log entry. Best-effort by design: losing one
// log line matters less than failing the operation it describes, so id and
// repository errors are swallowed.
func (s *Service) LogEvent(ctx context.Context, actor, postID string, eventType models.PostLogEventType, from, to *models.PostStatus, summary, payload string) {
	if s.Logs == nil {
		return
	}
	id, err := models.NewID()
	if err != nil {
		return
	}
	_ = s.Logs.Append(ctx, &models.PostLog{
		ID:         id,
		PostID:     postID,
		EventType:  eventType,
		Actor:      actor,
		FromStatus: from,
		ToStatus:   to,
		Summary:    summary,
		Payload:    logs.SanitizeAndCap(payload),
	})
}

// record emits a best-effort "post" activity event from the API.
func (s *Service) record(ctx context.Context, typ string, opts ...activity.Option) {
	s.Activity.Record(ctx, activity.CategoryPost, typ,
		append([]activity.Option{activity.WithSource(activity.SourceAPI)}, opts...)...)
}

// DeriveThreadSegments materialises post.ThreadSegments from the canonical
// body: cleared for a non-thread (also covering demotion), split with the
// platform's per-segment limit for a thread.
func (s *Service) DeriveThreadSegments(ctx context.Context, post *models.Post) {
	limit := 0
	if post.IsThread() {
		limit = s.threadLimit(ctx, post.PlatformID)
	}
	ApplyThreadSegments(post, limit)
}

// threadLimit resolves a platform's per-segment limit, or 0 ("unknown") when
// the platform can't be loaded — e.g. a draft saved before one is picked.
func (s *Service) threadLimit(ctx context.Context, platformID string) int {
	if platformID == "" || s.Platforms == nil {
		return 0
	}
	p, err := s.Platforms.GetByID(ctx, platformID)
	if err != nil {
		return 0
	}
	return ThreadLimitOf(p)
}

// ThreadLimitOf is the per-segment thread char limit of a loaded platform
// (nil ⇒ 0, "unknown"; a 0 limit still honours manual "---" splits).
func ThreadLimitOf(p *models.Platform) int {
	if p == nil {
		return 0
	}
	return p.TextConstraints.ContentLimitFor(models.PostTypeThread)
}

// ApplyThreadSegments sets post.ThreadSegments to the segments split out of
// the canonical body for a thread, or an empty list for any other type.
func ApplyThreadSegments(post *models.Post, segmentLimit int) {
	if post.PlatformPostType == models.PostTypeThread {
		post.ThreadSegments = platforms.SplitThread(post.Content, segmentLimit)
		return
	}
	post.ThreadSegments = models.ThreadSegments{}
}

// HasValidationErrors reports whether any platform in the gate's verdict has
// at least one error (an empty list means that platform passes).
func HasValidationErrors(m map[string][]platforms.ValidationError) bool {
	for _, v := range m {
		if len(v) > 0 {
			return true
		}
	}
	return false
}

// RequirePlatformIfNotDraft enforces that platform fields are set for any
// status past draft: a draft may be written before a platform is picked, but
// a post moving toward publication needs both.
func RequirePlatformIfNotDraft(status models.PostStatus, platformID, platformPostType string) error {
	if status == models.PostStatusDraft {
		return nil
	}
	if platformID == "" {
		return fmt.Errorf("platform_id is required when status is %q", status)
	}
	if platformPostType == "" {
		return fmt.Errorf("platform_post_type is required when status is %q", status)
	}
	return nil
}
