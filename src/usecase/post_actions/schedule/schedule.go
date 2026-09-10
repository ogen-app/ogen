// Package schedule implements the shared "schedule a Post for
// publishing" operation (CON-78). It is the single source of truth used
// by the REST endpoint (POST /api/posts/:id/schedule), the Post
// Assistant's schedulePost tool, AND the existing PUT /api/posts/:id
// scheduling path — so all three can never drift on the rule that
// matters: how a post is routed to auto- vs manual-publish and how that
// decision is persisted transactionally with the Zernio enqueue.
//
// The instant a post is scheduled for is always stored as an absolute
// UTC time; the workspace timezone (CON-78) only affects how relative
// expressions are resolved and how the time is echoed — both of which
// happen in the caller, before this service runs, keeping the service
// deterministic (mirrors the CON-59 clone pattern).
package schedule

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/domain/platforms"
	"github.com/ogen-app/ogen/src/infra/eventhub"
	"github.com/ogen-app/ogen/src/infra/publishers/zernio"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/usecase/accountselect"
)

var (
	// ErrPostNotFound is returned when the post being scheduled does not exist.
	ErrPostNotFound = errors.New("post not found")
	// ErrScheduledAtRequired is returned when no scheduled_at was supplied.
	ErrScheduledAtRequired = errors.New("scheduled_at is required")
	// ErrScheduledAtInPast is returned when scheduled_at is not strictly in
	// the future at commit time (re-checked here because time passes between
	// the assistant's confirm turn and its commit turn — CON-78 §9).
	ErrScheduledAtInPast = errors.New("scheduled_at must be in the future")
	// ErrNoPlatform is returned when the post has no platform set, so neither
	// the allowlist nor pre-publish validation can be evaluated.
	ErrNoPlatform = errors.New("post has no platform set")
	// ErrNotSchedulable is returned when the post's status is not one that
	// can be scheduled (only ready_for_publish, or draft with AllowPromote).
	ErrNotSchedulable = errors.New("post is not in a schedulable state")
)

// Trigger identifies which entry point initiated a schedule (recorded in
// the audit log for attribution).
const (
	TriggerAPI       = "api"
	TriggerAssistant = "assistant"
)

// ValidationError wraps the CON-74 pre-publish failures encountered when
// auto-promoting a draft. The REST layer maps it to 422; the assistant
// declines and lists the per-platform reasons.
type ValidationError struct {
	Errors map[string][]platforms.ValidationError
}

func (e *ValidationError) Error() string {
	return "post failed pre-publish validation"
}

// AccountCandidate identifies one connected same-platform account the user
// may choose between (CON-150). Serialised into the 422 body so the client
// can render an account picker.
type AccountCandidate struct {
	ID          string `json:"id"`
	Username    string `json:"username"`
	DisplayName string `json:"displayName"`
}

// AccountSelectionError is returned by the CON-150 gate when an auto-publish
// post's account selection is missing (with 2+ candidates), unknown, or on
// the wrong platform. Reason is a stable machine code:
//   - account_selection_required — no account chosen but 2+ are connected
//     (Candidates lists them)
//   - account_unavailable — the chosen account is not connected
//   - account_platform_mismatch — the chosen account is on another platform
//
// The REST layer maps it to 422; the assistant declines with the reason.
type AccountSelectionError struct {
	Reason     string
	Platform   string
	Candidates []AccountCandidate
}

func (e *AccountSelectionError) Error() string { return e.Reason }

// Options controls a single schedule.
type Options struct {
	// ScheduledAt is the absolute instant to publish at. Required; relative
	// references are resolved by the caller. Stored as UTC.
	ScheduledAt time.Time
	// AllowPromote permits auto-promoting a draft (run CON-74 validation,
	// then Draft → ReadyForPublish) before scheduling.
	AllowPromote bool
	// Actor is the user id recorded on the audit entries. Required.
	Actor string
	// Trigger is TriggerAPI or TriggerAssistant.
	Trigger string
}

// Result is the outcome of a successful schedule.
type Result struct {
	// Post is the post after scheduling (status + scheduled_at applied).
	Post *models.Post
	// ScheduledAt is the absolute UTC instant the post was scheduled for.
	ScheduledAt time.Time
	// Status is the routed status: Scheduled (auto) or
	// ScheduledForManualPublish (not allowlisted).
	Status models.PostStatus
	// AutoPublish reports whether the platform was allowlisted (and a
	// Zernio submit was enqueued).
	AutoPublish bool
	// Promoted is true when a draft was auto-promoted to ready_for_publish
	// as part of this schedule.
	Promoted bool
}

// SubmitEnqueuer enqueues a Zernio submit task inside the caller's
// transaction, so the enqueue commits atomically with the post status
// change (CON-78 §9). Implemented by *queues.Enqueuer; kept as a narrow
// interface here so this package doesn't depend on the queue runtime.
type SubmitEnqueuer interface {
	EnqueueSubmitTx(ctx context.Context, tx *sql.Tx, postID string) error
}

// Service performs schedules. Construct with New.
type Service struct {
	db          *bun.DB
	posts       repository.PostRepository
	platforms   repository.PlatformRepository
	attachments repository.PostAttachmentRepository
	allowlist   repository.AutoPublishAllowlistRepository
	logs        repository.PostLogRepository
	jobs        SubmitEnqueuer
	hub         eventhub.Hub

	// accounts + profileID back the CON-150 account-selection gate. Both nil
	// (unset) disables the gate — the submit worker remains the authoritative
	// backstop. Wired via SetAccountGate.
	accounts  repository.SocialAccountRepository
	profileID func(ctx context.Context) (string, error)

	// versions snapshots the content Ogen submits to Zernio at schedule time,
	// so a published post keeps a durable record of "what actually went out"
	// instead of an assumption (CON-251). nil disables it; wired via
	// SetVersionSnapshot.
	versions repository.PostVersionRepository

	// now is injectable so tests can pin "current time" for future-time
	// validation. nil → time.Now().UTC().
	now func() time.Time
}

// New wires a schedule Service. allowlist/jobs/logs/hub may be nil
// (degrades gracefully: nil allowlist → everything routes to manual
// publish; nil jobs → no enqueue; nil logs → no audit; nil hub → silent).
func New(
	db *bun.DB,
	posts repository.PostRepository,
	platformRepo repository.PlatformRepository,
	attachments repository.PostAttachmentRepository,
	allowlist repository.AutoPublishAllowlistRepository,
	logs repository.PostLogRepository,
	jobs SubmitEnqueuer,
	hub eventhub.Hub,
) *Service {
	return &Service{
		db:          db,
		posts:       posts,
		platforms:   platformRepo,
		attachments: attachments,
		allowlist:   allowlist,
		logs:        logs,
		jobs:        jobs,
		hub:         hub,
	}
}

// SetAccountGate wires the CON-150 account-selection gate: when a post
// routes to auto-publish, verify an explicit account selection (if any) is
// valid and require an explicit choice when the platform has more than one
// connected account. accounts resolves the tenant's connected accounts;
// profileID resolves the tenant's Zernio profile id. Leaving either nil
// disables the gate (the submit worker still enforces it at publish time).
func (s *Service) SetAccountGate(accounts repository.SocialAccountRepository, profileID func(ctx context.Context) (string, error)) {
	s.accounts = accounts
	s.profileID = profileID
}

// SetVersionSnapshot wires the CON-251 "what went out" recorder: when a post
// routes to auto-publish (Scheduled), the exact content submitted to Zernio
// is captured as a system-authored version. Leaving it nil disables the
// snapshot; it is best-effort and never fails a schedule.
func (s *Service) SetVersionSnapshot(versions repository.PostVersionRepository) {
	s.versions = versions
}

// SetClock overrides the time source (tests only).
func (s *Service) SetClock(fn func() time.Time) { s.now = fn }

func (s *Service) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now().UTC()
}

// Schedule schedules the post identified by postID for opts.ScheduledAt.
// It validates the post's state and the future time, optionally
// auto-promotes a draft (CON-74), routes to auto- vs manual-publish via
// the allowlist, and persists the status change, scheduled_at, audit
// entries, and (for auto-publish) the Zernio submit enqueue in a single
// transaction. Relative-time resolution is the caller's job — opts.
// ScheduledAt must already be absolute.
func (s *Service) Schedule(ctx context.Context, postID string, opts Options) (*Result, error) {
	if opts.ScheduledAt.IsZero() {
		return nil, ErrScheduledAtRequired
	}

	post, err := s.posts.GetByID(ctx, postID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrPostNotFound
		}
		return nil, err
	}

	scheduledAt := opts.ScheduledAt.UTC()
	// Re-validate the future time here (not just in the handler): on the
	// assistant path several seconds pass between confirm and commit.
	if !scheduledAt.After(s.clock()) {
		return nil, ErrScheduledAtInPast
	}

	if post.PlatformID == "" {
		return nil, ErrNoPlatform
	}

	promoted := false
	var extraLogs []*models.PostLog

	switch post.Status {
	case models.PostStatusReadyForPublish:
		// Already publishable — schedule directly.
	case models.PostStatusDraft:
		if !opts.AllowPromote {
			return nil, fmt.Errorf("%w: %s (promotion not requested)", ErrNotSchedulable, post.Status)
		}
		platform, atts, lerr := s.loadForValidation(ctx, post)
		if lerr != nil {
			return nil, lerr
		}
		if errsByPlatform := s.validateForPublish(post, platform, atts); errsByPlatform != nil {
			return nil, &ValidationError{Errors: errsByPlatform}
		}
		promoted = true
		// Record the Draft → ReadyForPublish promote as its own audit edge
		// so the history reads correctly (validation passed, then promote).
		extraLogs = s.promoteLogs(post.ID, opts.Actor)
	default:
		return nil, fmt.Errorf("%w: %s", ErrNotSchedulable, post.Status)
	}

	// Route + persist from the (possibly just-promoted) ready_for_publish
	// state so every state-machine edge in the log is valid.
	autoPublish, target, supported, err := s.route(ctx, post.PlatformID)
	if err != nil {
		return nil, err
	}

	// CON-150: only auto-publish posts submit through Zernio, so only they
	// need an unambiguous account. Runs before we mutate/persist the post.
	if autoPublish {
		if err := s.checkAccountSelection(ctx, post, supported.ZernioID); err != nil {
			return nil, err
		}
	}

	post.Status = target
	post.ScheduledAt = &scheduledAt
	post.UpdatedAt = s.clock()

	logs := append(extraLogs,
		s.decisionLog(post.ID, models.PostStatusReadyForPublish, target, post.PlatformID, supported, autoPublish, opts.Actor),
		s.transitionLog(post.ID, models.PostStatusReadyForPublish, target, "post scheduled", opts.Actor),
		s.scheduleActionLog(post.ID, scheduledAt, target, autoPublish, promoted, opts),
	)

	if err := s.persist(ctx, post, autoPublish, logs); err != nil {
		return nil, err
	}

	s.publishScheduled(post.ID, opts.Actor, scheduledAt, target, autoPublish, promoted)

	return &Result{
		Post:        post,
		ScheduledAt: scheduledAt,
		Status:      target,
		AutoPublish: autoPublish,
		Promoted:    promoted,
	}, nil
}

// RouteAndPersist is the thin entry used by the PUT /api/posts/:id
// scheduling path. The caller has already applied its full field set and
// set ScheduledAt on `post`; this routes the platform through the
// allowlist, sets the routed status, and writes the post row + the
// allowlist-decision + state-transition logs + (auto only) Zernio enqueue
// in one transaction. Returns the routed status string for the
// X-Auto-Publish-Decision response header. It deliberately does NOT
// re-validate state/time or emit the user_schedule action log — the PUT
// path keeps its historical, narrower behaviour; only the routing rule
// and the transactional write are shared.
func (s *Service) RouteAndPersist(ctx context.Context, post *models.Post, prevStatus models.PostStatus, actor string) (string, error) {
	autoPublish, target, supported, err := s.route(ctx, post.PlatformID)
	if err != nil {
		return "", err
	}
	// CON-150 gate (see Schedule): auto-publish posts need an unambiguous account.
	if autoPublish {
		if err := s.checkAccountSelection(ctx, post, supported.ZernioID); err != nil {
			return "", err
		}
	}
	post.Status = target
	post.UpdatedAt = s.clock()

	logs := []*models.PostLog{
		s.decisionLog(post.ID, prevStatus, target, post.PlatformID, supported, autoPublish, actor),
		s.transitionLog(post.ID, prevStatus, target, "status changed via PUT /api/posts/:id (schedule)", actor),
	}
	if err := s.persist(ctx, post, autoPublish, logs); err != nil {
		return "", err
	}
	return string(target), nil
}

// snapshotSubmitted records a system-authored version of the content just
// submitted to Zernio (CON-251), so a published post keeps a durable record
// of what actually went out rather than an assumption. Deduped only against a
// prior submit snapshot of identical content, so re-scheduling unchanged
// content adds no noise — but a matching-content user/assistant edit at the
// head does NOT suppress it (that edit isn't the record of what was submitted).
// Best-effort: a nil repo or any error is swallowed after a warn.
func (s *Service) snapshotSubmitted(ctx context.Context, post *models.Post) {
	if s.versions == nil {
		return
	}
	latest, err := s.versions.GetLatestByPostID(ctx, post.ID)
	if err != nil {
		slog.WarnContext(ctx, "schedule: load latest version for submit snapshot",
			logging.AttrComponent, "schedule", "post_id", post.ID, logging.AttrError, err)
		return
	}
	// CON-284 R2: content is the canonical full thread body, so SnapshotContent is
	// simply post.Content for every post type — the snapshot captures the whole
	// chain by construction.
	content := post.SnapshotContent()
	if latest != nil && latest.IsSystemSnapshot() &&
		latest.Note == models.PostVersionNoteSubmitted && latest.Content == content {
		return // already snapshotted this exact submission — nothing new went out
	}
	nextNum := 1
	if latest != nil {
		nextNum = latest.VersionNumber + 1
	}
	id, err := models.NewID()
	if err != nil {
		return
	}
	if err := s.versions.Create(ctx, &models.PostVersion{
		ID:            id,
		PostID:        post.ID,
		VersionNumber: nextNum,
		Content:       content,
		Note:          models.PostVersionNoteSubmitted,
		Creator:       models.PostVersionCreatorSystem,
	}); err != nil {
		slog.WarnContext(ctx, "schedule: create submit snapshot",
			logging.AttrComponent, "schedule", "post_id", post.ID, logging.AttrError, err)
	}
}

// route reads the auto-publish allowlist for the post's platform and
// returns the resulting decision: autoPublish + the routed target status
// (Scheduled vs ScheduledForManualPublish) + the matched Zernio platform
// (nil when the platform isn't Zernio-supported). Pure read — no writes —
// so callers can build their audit log before opening the transaction.
func (s *Service) route(ctx context.Context, platformID string) (autoPublish bool, target models.PostStatus, supported *zernio.SupportedPlatform, err error) {
	supported = zernio.LookupSupportedBySqid(platformID)
	if supported != nil && s.allowlist != nil {
		ok, aerr := s.allowlist.Contains(ctx, supported.ZernioID)
		if aerr != nil {
			return false, "", nil, aerr
		}
		autoPublish = ok
	}
	target = models.PostStatusScheduledForManualPublish
	if autoPublish {
		target = models.PostStatusScheduled
	}
	return autoPublish, target, supported, nil
}

// checkAccountSelection is the CON-150 write-time gate. It only runs for
// auto-publish posts (manual-publish posts never route through our submit
// worker, so the account is irrelevant). Returns an *AccountSelectionError
// (→ 422) when an explicit selection is invalid or when the platform has
// 2+ accounts and none was chosen. A missing gate or profile is a no-op —
// the submit worker stays the authoritative backstop.
func (s *Service) checkAccountSelection(ctx context.Context, post *models.Post, zernioPlatform string) error {
	if s.accounts == nil || s.profileID == nil {
		return nil
	}
	profileID, err := s.profileID(ctx)
	if err != nil || profileID == "" {
		return nil // no profile yet — can't validate here; submit worker will.
	}

	res, err := accountselect.Resolve(ctx, s.accounts, profileID, post, zernioPlatform)
	if err != nil {
		return err
	}
	// The gate only blocks the schedule for the cases the user must fix now: a
	// bad explicit account, or an ambiguous 2+. Resolved and NoAccount fall
	// through (return nil) — the submit worker auto-selects the one or terminally
	// fails "no_account_connected", preserving prior behaviour.
	switch res.Outcome {
	case accountselect.Unavailable:
		return &AccountSelectionError{Reason: "account_unavailable", Platform: zernioPlatform}
	case accountselect.PlatformMismatch:
		return &AccountSelectionError{Reason: "account_platform_mismatch", Platform: zernioPlatform}
	case accountselect.Ambiguous:
		candidates := make([]AccountCandidate, 0, len(res.Candidates))
		for _, c := range res.Candidates {
			candidates = append(candidates, AccountCandidate{ID: c.ID, Username: c.Username, DisplayName: c.DisplayName})
		}
		return &AccountSelectionError{Reason: "account_selection_required", Platform: zernioPlatform, Candidates: candidates}
	default:
		return nil
	}
}

// persist writes the post row, every supplied log entry, and (when
// autoPublish) the Zernio submit enqueue in one transaction so a failure
// in any of them rolls the whole schedule back — no half-scheduled post,
// no enqueue without a status change (CON-78 §9).
func (s *Service) persist(ctx context.Context, post *models.Post, autoPublish bool, logs []*models.PostLog) error {
	if err := s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		if _, err := tx.NewUpdate().Model(post).WherePK().Exec(ctx); err != nil {
			return err
		}
		if s.logs != nil {
			for _, l := range logs {
				if l == nil {
					continue
				}
				if err := s.logs.AppendTx(ctx, tx, l); err != nil {
					return err
				}
			}
		}
		if autoPublish && s.jobs != nil {
			if err := s.jobs.EnqueueSubmitTx(ctx, tx.Tx, post.ID); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}
	// CON-251: an auto-publish post's content is now committed for submission to
	// Zernio (which snapshots it at schedule time) and locked in Ogen — record
	// what goes out so the published post carries a durable version rather than
	// an assumption. Runs for every scheduling entry point (POST /schedule, the
	// assistant, PUT) because all three funnel through here. Manual-publish
	// posts hold no external copy yet, so they get none. Best-effort, outside
	// the tx — the schedule has already committed.
	if autoPublish {
		s.snapshotSubmitted(ctx, post)
	}
	return nil
}

// loadForValidation fetches the post's platform and attachments for the
// CON-74 promote gate. A missing platform row is treated as ErrNoPlatform.
func (s *Service) loadForValidation(ctx context.Context, post *models.Post) (*models.Platform, []models.PostAttachment, error) {
	platform := post.Platform
	if platform == nil || platform.ID != post.PlatformID {
		if s.platforms == nil {
			return nil, nil, ErrNoPlatform
		}
		p, err := s.platforms.GetByID(ctx, post.PlatformID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil, nil, ErrNoPlatform
			}
			return nil, nil, err
		}
		platform = p
	}
	var atts []models.PostAttachment
	if s.attachments != nil {
		a, err := s.attachments.ListByPostID(ctx, post.ID)
		if err != nil {
			return nil, nil, err
		}
		atts = a
	}
	return platform, atts, nil
}

// validateForPublish runs the CON-74 readiness rules (attachment rules +
// per-post-type rules) for the promote gate. Returns nil when the post
// passes, or the populated per-platform error map when it does not.
func (s *Service) validateForPublish(post *models.Post, platform *models.Platform, atts []models.PostAttachment) map[string][]platforms.ValidationError {
	// CON-284: ValidatePublishReadiness runs the per-segment gate for thread
	// posts and the whole-post gate otherwise, so the schedule path never
	// double-counts a thread's media against the per-post cap.
	errsByPlatform := platforms.ValidatePublishReadiness(post, platform, atts)
	if !hasAnyErrors(errsByPlatform) {
		return nil
	}
	return errsByPlatform
}

func hasAnyErrors(m map[string][]platforms.ValidationError) bool {
	for _, v := range m {
		if len(v) > 0 {
			return true
		}
	}
	return false
}

func zernioName(s *zernio.SupportedPlatform) string {
	if s == nil {
		return ""
	}
	return s.ZernioID
}
