package handlers

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/domain/platforms"
	"github.com/ogen-app/ogen/src/infra/publishers/zernio"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/infra/storage"
	"github.com/ogen-app/ogen/src/jobs/queues"
	"github.com/ogen-app/ogen/src/kernel/activity"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/usecase/post_actions/logs"
	"github.com/ogen-app/ogen/src/usecase/post_actions/schedule"
	"github.com/ogen-app/ogen/src/usecase/post_actions/update"
)

var validPostStatuses = map[models.PostStatus]bool{
	models.PostStatusDraft:                     true,
	models.PostStatusReadyForPublish:           true,
	models.PostStatusScheduled:                 true,
	models.PostStatusScheduledForManualPublish: true,
	models.PostStatusFailed:                    true,
	models.PostStatusPublished:                 true,
	models.PostStatusNotPublished:              true,
}

var validCTATypes = map[models.PostCTAType]bool{
	models.CTATypeLink:   true,
	models.CTATypeButton: true,
	models.CTATypeNone:   true,
}

type PostsHandler struct {
	repo           repository.PostRepository
	versionRepo    repository.PostVersionRepository
	platformRepo   repository.PlatformRepository
	attachmentRepo repository.PostAttachmentRepository
	// brandRepo validates a post's brand_voice_id/brand_audience_id belong to
	// the tenant. nil skips validation.
	brandRepo repository.BrandRepository
	auth      fiber.Handler
	// onBeforeDelete runs before the post row is deleted. The server
	// wires this to clean up S3 objects belonging to the post's
	// attachments ("all of its attachments are deleted
	// from S3 immediately as part of the same operation"). nil is
	// treated as no-op for fixtures that don't care about attachments.
	onBeforeDelete func(ctx context.Context, postID string) error
	// postLogRepo records every meaningful operation against a Post.
	// nil makes log writes no-ops so legacy fixtures stay
	// green.
	postLogRepo repository.PostLogRepository
	// allowlistRepo answers "is this Zernio platform allowed to
	// auto-publish?" when the user moves a post to Scheduled.
	// nil disables the allowlist branch entirely;
	// posts go straight to Scheduled with no River enqueue.
	allowlistRepo repository.AutoPublishAllowlistRepository
	// jobsClient enqueues the Zernio cancellation task. nil
	// disables the cancel endpoint (503).
	jobsClient CancelEnqueuer
	// db is the Bun DB handle. Held so the schedule path can run the
	// status update + PostLog write + River enqueue in a single
	// transaction.
	db *bun.DB
	// scheduleSvc schedules a post for publishing: the single
	// source of truth for allowlist routing + transactional persist +
	// Zernio enqueue, shared by POST /:id/schedule, the assistant's
	// schedulePost tool, and the PUT scheduling branch. nil disables the
	// schedule endpoint (503) and makes the PUT branch fall back to a
	// plain status update (test fixtures that don't wire scheduling).
	scheduleSvc *schedule.Service
	// activity records CON-125 user-activity events (post_created,
	// post_scheduled, …) to the analytics store. nil is a no-op (analytics
	// disabled / fixtures).
	activity *activity.Recorder
	// campaignRepo answers "is this phase one of the campaign's type's
	// phases?" so a mismatched campaign_type_phase_id gets a clean
	// 400. nil leaves it to the DB trigger, whose
	// rejection is mapped to the same 400.
	campaignRepo repository.CampaignRepository
	// storage mints the public cover_url on read responses. nil leaves
	// cover_url unset.
	storage storage.Storage
}

// checkPhase reports whether a post's campaign_type_phase_id (when set) is a
// phase of its campaign's type. Without a wired campaign repo it
// passes and the DB trigger decides.
func (h *PostsHandler) checkPhase(ctx context.Context, campaignID string, phaseID *string) (bool, error) {
	return h.updater().PhaseBelongs(ctx, campaignID, phaseID)
}

// CancelEnqueuer enqueues a Zernio cancellation task. Implemented by
// *queues.Enqueuer; kept as a narrow interface so the handler depends on a
// tiny method set rather than the queue runtime directly.
type CancelEnqueuer interface {
	EnqueueCancel(ctx context.Context, postID string, target queues.CancelTarget, actor string) error
}

// recordActivity emits a best-effort CON-125 "post" activity event. Tenant +
// user are resolved from the request context (set by the auth middleware);
// source defaults to api and can be overridden by a later option.
func (h *PostsHandler) recordActivity(c *fiber.Ctx, typ string, opts ...activity.Option) {
	h.activity.Record(reqCtx(c), activity.CategoryPost, typ,
		append([]activity.Option{activity.WithSource(activity.SourceAPI)}, opts...)...)
}

// logEvent appends a PostLog entry, swallowing repo errors so logging
// failures never leak through to the user-facing response. Emitting
// an audit entry is best-effort by design — losing one log line
// matters less than failing the operation it describes.
func (h *PostsHandler) logEvent(c *fiber.Ctx, postID string, eventType models.PostLogEventType, fromStatus, toStatus *models.PostStatus, summary, payload string) {
	h.updater().LogEvent(reqCtx(c), cmp.Or(actorID(c), models.ActorSystem), postID, eventType, fromStatus, toStatus, summary, payload)
}

// updater builds the post update use case over the handler's wired
// dependencies (several are set after construction, so it is built per call).
func (h *PostsHandler) updater() *update.Service {
	return &update.Service{
		Posts:       h.repo,
		Platforms:   h.platformRepo,
		Attachments: h.attachmentRepo,
		Campaigns:   h.campaignRepo,
		Logs:        h.postLogRepo,
		Schedule:    h.scheduleSvc,
		Activity:    h.activity,
	}
}

// deriveThreadSegments materialises post.ThreadSegments from the canonical body.
func (h *PostsHandler) deriveThreadSegments(ctx context.Context, post *models.Post) {
	h.updater().DeriveThreadSegments(ctx, post)
}

// writeAccountSelectionError renders a CON-150 account-selection failure as a
// 422 carrying the stable machine reason, the platform, and — for the
// ambiguous case — the connected accounts the client can offer as a picker.
func writeAccountSelectionError(c *fiber.Ctx, e *schedule.AccountSelectionError) error {
	body := fiber.Map{"error": e.Reason, "platform": e.Platform}
	if e.Candidates != nil {
		body["candidates"] = e.Candidates
	}
	return c.Status(fiber.StatusUnprocessableEntity).JSON(body)
}

// scheduleRequest is the body for POST /api/posts/:id/schedule. The
// instant is absolute (relative expressions like "tomorrow 9am" are
// resolved by the assistant before it calls the shared service; the
// REST endpoint takes the resolved value directly).
type scheduleRequest struct {
	ScheduledAt  *time.Time `json:"scheduled_at"`
	AllowPromote bool       `json:"allow_promote"`
}

// Schedule godoc
// @Summary      Schedule a post for publishing
// @Description  Schedules a `ready_for_publish` post for the given absolute
// @Description  time (CON-78). Allowlisted platforms route to `scheduled`
// @Description  (auto-publish, Zernio submit enqueued); others route to
// @Description  `scheduled_for_manual_publishing`. Pass `allow_promote` to
// @Description  auto-promote a `draft` (runs CON-74 pre-publish validation,
// @Description  then Draft → ReadyForPublish) before scheduling. The status
// @Description  change, `scheduled_at`, audit entries, and Zernio enqueue
// @Description  commit in one transaction.
// @Tags         posts
// @Accept       json
// @Produce      json
// @Security     CookieAuth
// @Param        id    path      string           true  "Post Sqid"
// @Param        body  body      scheduleRequest  true  "Schedule payload"
// @Success      200   {object}  map[string]interface{}
// @Failure      400   {object}  map[string]string
// @Failure      401   {object}  map[string]string
// @Failure      404   {object}  map[string]string
// @Failure      422   {object}  map[string]interface{}
// @Failure      503   {object}  map[string]string
// @Router       /api/posts/{id}/schedule [post]
func (h *PostsHandler) Schedule(c *fiber.Ctx) error {
	if h.scheduleSvc == nil {
		return fiber.NewError(fiber.StatusServiceUnavailable, "scheduling is not available")
	}
	var req scheduleRequest
	if err := c.BodyParser(&req); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, err.Error())
	}
	if req.ScheduledAt == nil {
		return fiber.NewError(fiber.StatusBadRequest, "scheduled_at is required")
	}

	session, err := sessionFrom(c)
	if err != nil {
		return err
	}
	res, err := h.scheduleSvc.Schedule(reqCtx(c), c.Params("id"), schedule.Options{
		ScheduledAt:  *req.ScheduledAt,
		AllowPromote: req.AllowPromote,
		Actor:        session.UserID,
		Trigger:      schedule.TriggerAPI,
	})
	if err != nil {
		var verr *schedule.ValidationError
		var aerr *schedule.AccountSelectionError
		switch {
		case errors.Is(err, schedule.ErrPostNotFound):
			return fiber.NewError(fiber.StatusNotFound, "post not found")
		case errors.As(err, &aerr):
			return writeAccountSelectionError(c, aerr)
		case errors.As(err, &verr):
			return c.Status(fiber.StatusUnprocessableEntity).JSON(fiber.Map{
				"error":               "post is not ready for publish",
				"platform_validation": verr.Errors,
			})
		case errors.Is(err, schedule.ErrScheduledAtRequired),
			errors.Is(err, schedule.ErrScheduledAtInPast),
			errors.Is(err, schedule.ErrNoPlatform),
			errors.Is(err, schedule.ErrNotSchedulable):
			return fiber.NewError(fiber.StatusBadRequest, err.Error())
		}
		return err
	}

	// Re-fetch so the response carries a fully hydrated post (campaign /
	// platform / assets), matching the Update/Restore handlers' contract.
	updated, err := h.repo.GetByID(reqCtx(c), c.Params("id"))
	if err != nil {
		return err
	}
	h.recordActivity(c, "post_scheduled",
		activity.WithEntity("post", updated.ID),
		activity.WithStatus(string(res.Status)),
		activity.WithPayload(map[string]any{
			"scheduled_at": req.ScheduledAt,
			"auto_publish": res.AutoPublish,
			"promoted":     res.Promoted,
		}),
	)
	return c.JSON(fiber.Map{
		"post":         updated,
		"status":       string(res.Status),
		"auto_publish": res.AutoPublish,
		"promoted":     res.Promoted,
	})
}

// cancelRequest is the body shape for POST /api/posts/:id/cancel.
type cancelRequest struct {
	// Target is the status the user wants the post moved to once
	// Zernio confirms the cancellation. Defaults to "ready_for_publish"
	// when omitted (ReadyForPublish is the most common
	// "cancel and edit" landing state).
	Target string `json:"target"`
}

// Cancel godoc
// @Summary      Cancel a Scheduled post
// @Description  Enqueues a River cancel_zernio_job task. The Post
// @Description  remains in `Scheduled` until Zernio confirms the
// @Description  cancellation; on confirmation it transitions to the
// @Description  requested target (`ready_for_publish` or `draft`).
// @Description  If Zernio reports the job has already been published
// @Description  (race), the cancellation is a no-op and the next poll
// @Description  cycle lands `Published` per the normal success path
// @Description  (CON-69 §9).
// @Tags         posts
// @Accept       json
// @Produce      json
// @Security     CookieAuth
// @Param        id    path      string         true  "Post Sqid"
// @Param        body  body      cancelRequest  false "Cancellation target"
// @Success      202   {object}  map[string]string
// @Failure      400   {object}  map[string]string
// @Failure      401   {object}  map[string]string
// @Failure      404   {object}  map[string]string
// @Failure      409   {object}  map[string]string
// @Failure      503   {object}  map[string]string
// @Router       /api/posts/{id}/cancel [post]
func (h *PostsHandler) Cancel(c *fiber.Ctx) error {
	if h.jobsClient == nil {
		return fiber.NewError(fiber.StatusServiceUnavailable, "background job runtime not configured")
	}
	post, err := load(c, h.repo.GetByID, "post not found")
	if err != nil {
		return err
	}
	if post.Status != models.PostStatusScheduled {
		return fiber.NewError(fiber.StatusConflict,
			"only Scheduled posts can be cancelled (current status: "+string(post.Status)+")")
	}

	var req cancelRequest
	_ = c.BodyParser(&req) // body is optional
	target := queues.CancelTargetReadyForPublish
	if req.Target != "" {
		switch queues.CancelTarget(req.Target) {
		case queues.CancelTargetReadyForPublish, queues.CancelTargetDraft:
			target = queues.CancelTarget(req.Target)
		default:
			return fiber.NewError(fiber.StatusBadRequest,
				`target must be "ready_for_publish" or "draft"`)
		}
	}

	actor := cmp.Or(actorID(c), models.ActorSystem)

	if err := h.jobsClient.EnqueueCancel(reqCtx(c), post.ID, target, actor); err != nil {
		return fmt.Errorf("cancel: enqueue: %w", err)
	}

	h.logEvent(c, post.ID, models.PostLogEventUserCancel, &post.Status, &post.Status,
		"user requested cancellation; cancel_zernio_job enqueued",
		logs.MarshalCapped(map[string]any{"target": string(target)}),
	)
	h.recordActivity(c, "post_schedule_cancelled",
		activity.WithEntity("post", post.ID),
		activity.WithPayload(map[string]any{"target": string(target)}),
	)

	return c.Status(fiber.StatusAccepted).JSON(fiber.Map{
		"status":  "cancellation_enqueued",
		"target":  string(target),
		"post_id": post.ID,
	})
}

// convertToManualRequest is the body for POST /api/posts/convert-to-manual.
// Exactly one of Platform / PostIDs must be set. Platform is a Zernio
// platform id (e.g. "linkedin"), matching the auto-publish allowlist's
// vocabulary; it selects every scheduled post for that platform.
type convertToManualRequest struct {
	Platform string   `json:"platform"`
	PostIDs  []string `json:"post_ids"`
}

// convertFailure is one rejected post in the convert-to-manual response.
type convertFailure struct {
	ID     string `json:"id"`
	Reason string `json:"reason"`
}

// ConvertToManual godoc
// @Summary      Convert scheduled posts to manual publishing
// @Description  Converts a set of posts — or every scheduled post for a
// @Description  platform — from auto-publish (`scheduled`) to
// @Description  `scheduled_for_manual_publishing`, keeping `scheduled_at`.
// @Description  Each post's live Zernio job is cancelled server-side by a
// @Description  cancel_zernio_job task, which then lands the post directly
// @Description  on `scheduled_for_manual_publishing` — no detour through
// @Description  `ready_for_publish`, so the post is never left unscheduled
// @Description  if the client disconnects mid-flight. Returns per-post
// @Description  outcomes: `converted` (accepted / already manual) and
// @Description  `failed` (not found, or not in a convertible state). A
// @Description  post that publishes before its cancel lands is handled by
// @Description  the worker and recorded in the Post Log (CON-130).
// @Tags         posts
// @Accept       json
// @Produce      json
// @Security     CookieAuth
// @Param        body  body      convertToManualRequest  true  "Platform or explicit post ids"
// @Success      202   {object}  map[string]interface{}
// @Failure      400   {object}  map[string]string
// @Failure      401   {object}  map[string]string
// @Failure      503   {object}  map[string]string
// @Router       /api/posts/convert-to-manual [post]
func (h *PostsHandler) ConvertToManual(c *fiber.Ctx) error {
	if h.jobsClient == nil {
		return fiber.NewError(fiber.StatusServiceUnavailable, "background job runtime not configured")
	}
	var req convertToManualRequest
	if err := c.BodyParser(&req); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, err.Error())
	}
	if (req.Platform != "") == (len(req.PostIDs) > 0) {
		return fiber.NewError(fiber.StatusBadRequest, `provide exactly one of "platform" or "post_ids"`)
	}

	conv := &manualConverter{h: h, ctx: reqCtx(c), actor: cmp.Or(actorID(c), models.ActorSystem),
		converted: make([]string, 0), failed: make([]convertFailure, 0)}
	if req.Platform != "" {
		if err := conv.byPlatform(req.Platform); err != nil {
			return err
		}
	} else {
		conv.byIDs(req.PostIDs)
	}

	h.recordActivity(c, "posts_converted_to_manual",
		activity.WithPayload(map[string]any{
			"platform":  req.Platform,
			"converted": len(conv.converted),
			"failed":    len(conv.failed),
		}),
	)
	return c.Status(fiber.StatusAccepted).JSON(fiber.Map{
		"converted": conv.converted,
		"failed":    conv.failed,
	})
}

// manualConverter collects the per-post outcomes of one convert-to-manual
// request.
type manualConverter struct {
	h         *PostsHandler
	ctx       context.Context
	actor     string
	converted []string
	failed    []convertFailure
}

// convert enqueues one durable cancel→manual task per still-scheduled post.
// Once enqueued the worker owns the cancel, the wait and the final status, so
// the request never holds the post in an unscheduled limbo.
func (m *manualConverter) convert(post *models.Post) {
	switch post.Status {
	case models.PostStatusScheduledForManualPublish:
		m.converted = append(m.converted, post.ID) // already there: idempotent success
	case models.PostStatusScheduled:
		if err := m.h.jobsClient.EnqueueCancel(m.ctx, post.ID, queues.CancelTargetManualPublish, m.actor); err != nil {
			m.fail(post.ID, "could not enqueue conversion: "+err.Error())
			return
		}
		m.converted = append(m.converted, post.ID)
	default:
		m.fail(post.ID, "post is not scheduled (status: "+string(post.Status)+")")
	}
}

func (m *manualConverter) fail(id, reason string) {
	m.failed = append(m.failed, convertFailure{ID: id, Reason: reason})
}

// byPlatform converts every scheduled post of a Zernio platform id.
func (m *manualConverter) byPlatform(platform string) error {
	sqid := zernio.LookupSqidByZernioID(platform)
	if sqid == "" {
		return fiber.NewError(fiber.StatusBadRequest,
			fmt.Sprintf("platform %q is not in the Ogen-supported set", platform))
	}
	posts, err := m.h.repo.ListScheduledByPlatform(m.ctx, sqid)
	if err != nil {
		return err
	}
	for i := range posts {
		m.convert(&posts[i])
	}
	return nil
}

// byIDs converts each distinct id. A post that can't be loaded is reported and
// skipped: one bad id must not sink the batch.
func (m *manualConverter) byIDs(ids []string) {
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		post, err := m.h.repo.GetByID(m.ctx, id)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			m.fail(id, "post not found")
		case err != nil:
			m.fail(id, "could not load post: "+err.Error())
		default:
			m.convert(post)
		}
	}
}

func NewPostsHandler(
	repo repository.PostRepository,
	versionRepo repository.PostVersionRepository,
	platformRepo repository.PlatformRepository,
	attachmentRepo repository.PostAttachmentRepository,
	auth fiber.Handler,
	opts PostsOptions,
) *PostsHandler {
	return &PostsHandler{
		repo:           repo,
		versionRepo:    versionRepo,
		platformRepo:   platformRepo,
		attachmentRepo: attachmentRepo,
		auth:           auth,
		brandRepo:      opts.Brands,
		campaignRepo:   opts.Campaigns,
		postLogRepo:    opts.PostLogs,
		allowlistRepo:  opts.Allowlist,
		jobsClient:     opts.Jobs,
		db:             opts.DB,
		scheduleSvc:    opts.Schedule,
		activity:       opts.Activity,
		onBeforeDelete: opts.OnBeforeDelete,
		storage:        opts.Storage,
	}
}

// PostsOptions carries the handler's optional collaborators. Every field is
// nil-safe: a nil one disables the branch it backs (see the matching
// PostsHandler field).
type PostsOptions struct {
	Brands    repository.BrandRepository
	Campaigns repository.CampaignRepository
	PostLogs  repository.PostLogRepository
	// Allowlist, Jobs and DB back the Scheduled transition: the allowlist
	// chooses Scheduled vs ScheduledForManualPublish, Jobs enqueues
	// cancellation tasks, and DB runs the status change in one transaction.
	Allowlist      repository.AutoPublishAllowlistRepository
	Jobs           CancelEnqueuer
	DB             *bun.DB
	Schedule       *schedule.Service
	Activity       *activity.Recorder
	OnBeforeDelete func(ctx context.Context, postID string) error
	Storage        storage.Storage
}

// decorateCovers fills each post's CoverURL from its first drawable
// attachment, served from the public key the way assets are (decorateFile) —
// a stored copy keeps working, unlike a presigned GET. Best-effort: a failed
// lookup is logged and leaves the posts without covers rather than failing the
// read, since the card simply falls back to no picture.
func (h *PostsHandler) decorateCovers(c *fiber.Ctx, posts []models.Post) {
	if h.storage == nil || h.attachmentRepo == nil || len(posts) == 0 {
		return
	}
	ids := make([]string, len(posts))
	for i := range posts {
		ids[i] = posts[i].ID
	}
	keys, err := h.attachmentRepo.CoverKeysByPostIDs(reqCtx(c), ids)
	if err != nil {
		slog.WarnContext(reqCtx(c), "post cover lookup failed", logging.AttrComponent, "handlers.posts",
			"posts", len(ids), logging.AttrError, err)
		return
	}
	for i := range posts {
		if key, ok := keys[posts[i].ID]; ok {
			u := h.storage.PublicURL(key)
			posts[i].CoverURL = &u
		}
	}
}

// decorateCover is decorateCovers for a single-post response, so a client that
// replaces its cached row with the returned post keeps the picture.
func (h *PostsHandler) decorateCover(c *fiber.Ctx, post *models.Post) {
	if post == nil {
		return
	}
	posts := []models.Post{*post}
	h.decorateCovers(c, posts)
	post.CoverURL = posts[0].CoverURL
}

// postBrandRequest is the body of PUT /api/posts/:id/brand — a targeted set of
// just the post's voice + audience, so the editor's picker need not
// round-trip the whole resource.
type postBrandRequest struct {
	BrandVoiceID    Optional[string] `json:"brand_voice_id"`
	BrandAudienceID Optional[string] `json:"brand_audience_id"`
}

// SetBrand sets a post's own brand voice + audience. Both are tenant-checked and
// nullable (null clears the ref). It changes only the two refs — no status
// machine, no publish gate — so it is a plain scoped update.
func (h *PostsHandler) SetBrand(c *fiber.Ctx) error {
	var req postBrandRequest
	if err := c.BodyParser(&req); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, err.Error())
	}
	post, err := load(c, h.repo.GetByID, "post not found")
	if err != nil {
		return err
	}
	if err := validateBrandRefs(reqCtx(c), h.brandRepo, req.BrandVoiceID.Value, req.BrandAudienceID.Value); err != nil {
		return err
	}
	// Presence-aware: an omitted field leaves the post's existing ref
	// alone; an explicit null clears it. The two are independent, so sending only
	// brand_voice_id never wipes the audience.
	req.BrandVoiceID.applyTo(&post.BrandVoiceID)
	req.BrandAudienceID.applyTo(&post.BrandAudienceID)
	post.UpdatedAt = time.Now().UTC()
	if err := h.repo.Update(reqCtx(c), post); err != nil {
		return err
	}
	h.decorateCover(c, post)
	return c.JSON(post)
}

// AddAssets godoc
// @Summary      Attach source assets to a post
// @Description  Unions the given asset ids into the post's sources
// @Description  (posts.used_asset_ids), touching no other field. The write is a
// @Description  single atomic UPDATE, so concurrent attaches of different sources
// @Description  both survive and omitted fields are never reset (CON-233).
// @Description  Adding an already-present id is a no-op. A source change to a
// @Description  submitted post (scheduled/published) is content-locked (409),
// @Description  mirroring PUT (CON-251). Returns the updated post.
// @Tags         posts
// @Accept       json
// @Produce      json
// @Security     CookieAuth
// @Param        id    path      string                 true  "Post Sqid"
// @Param        body  body      assetMembershipRequest true  "Asset ids to attach"
// @Success      200   {object}  models.Post
// @Failure      400   {object}  map[string]string
// @Failure      401   {object}  map[string]string
// @Failure      404   {object}  map[string]string
// @Failure      409   {object}  map[string]string
// @Router       /api/posts/{id}/assets [post]
func (h *PostsHandler) AddAssets(c *fiber.Ctx) error {
	var req assetMembershipRequest
	if err := c.BodyParser(&req); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, err.Error())
	}
	post, err := load(c, h.repo.GetByID, "post not found")
	if err != nil {
		return err
	}
	// A submitted post's sources are frozen — but only a real change is
	// rejected; re-adding ids it already has is a harmless no-op (matches PUT's
	// mutatesLockedContent / no-op-save rule).
	if post.Status.IsSubmitted() && addsNewID(post.UsedAssetIDs, req.AssetIDs) {
		return submittedLockError(post.Status)
	}
	updated, err := h.repo.AddUsedAssetIDs(reqCtx(c), c.Params("id"), req.AssetIDs)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fiber.NewError(fiber.StatusNotFound, "post not found")
		}
		// The repo re-checks the lock atomically, so a submit that won the race
		// against the pre-check above still surfaces as a 409.
		if submitted, ok := errors.AsType[*repository.PostSubmittedError](err); ok {
			return submittedLockError(submitted.Status)
		}
		return err
	}
	h.recordActivity(c, "post_updated",
		activity.WithEntity("post", updated.ID),
		activity.WithStatus(string(updated.Status)),
	)
	h.decorateCover(c, updated)
	return c.JSON(updated)
}

// RemoveAsset godoc
// @Summary      Detach a source asset from a post
// @Description  Removes one asset id from the post's sources
// @Description  (posts.used_asset_ids), touching no other field (CON-233).
// @Description  Removing an id that is not present is a no-op. Detaching a source
// @Description  from a submitted post (scheduled/published) is content-locked
// @Description  (409), mirroring PUT (CON-251). Returns the updated post.
// @Tags         posts
// @Produce      json
// @Security     CookieAuth
// @Param        id       path      string  true  "Post Sqid"
// @Param        assetId  path      string  true  "Asset Sqid to detach"
// @Success      200      {object}  models.Post
// @Failure      401      {object}  map[string]string
// @Failure      404      {object}  map[string]string
// @Failure      409      {object}  map[string]string
// @Router       /api/posts/{id}/assets/{assetId} [delete]
func (h *PostsHandler) RemoveAsset(c *fiber.Ctx) error {
	assetID := c.Params("assetId")
	post, err := load(c, h.repo.GetByID, "post not found")
	if err != nil {
		return err
	}
	if post.Status.IsSubmitted() && slices.Contains(post.UsedAssetIDs, assetID) {
		return submittedLockError(post.Status)
	}
	updated, err := h.repo.RemoveUsedAssetID(reqCtx(c), c.Params("id"), assetID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fiber.NewError(fiber.StatusNotFound, "post not found")
		}
		// The repo re-checks the lock atomically, so a submit that won the race
		// against the pre-check above still surfaces as a 409.
		if submitted, ok := errors.AsType[*repository.PostSubmittedError](err); ok {
			return submittedLockError(submitted.Status)
		}
		return err
	}
	h.recordActivity(c, "post_updated",
		activity.WithEntity("post", updated.ID),
		activity.WithStatus(string(updated.Status)),
	)
	h.decorateCover(c, updated)
	return c.JSON(updated)
}

// addsNewID reports whether any incoming id is absent from existing — i.e.
// whether an add would actually change the set. Used to let a no-op add through
// the CON-251 submitted-post lock while still rejecting a real source change.
func addsNewID(existing, incoming models.StringSlice) bool {
	for _, id := range incoming {
		if !slices.Contains(existing, id) {
			return true
		}
	}
	return false
}

// submittedLockError is the shared CON-251 409 for editing a locked (submitted)
// post's content, reused by the membership endpoints so the message can't drift
// from PUT's.
func submittedLockError(status models.PostStatus) error {
	return fiber.NewError(fiber.StatusConflict,
		"post has been submitted ("+string(status)+") and its content is locked; unschedule to edit")
}

func (h *PostsHandler) Register(app *fiber.App) {
	g := app.Group("/api/posts")
	g.Get("/", h.auth, h.List)
	g.Post("/", h.auth, h.Create)
	// Static route registered before "/:id/..." so it isn't shadowed by
	// the id-parametrised routes.
	g.Post("/convert-to-manual", h.auth, h.ConvertToManual)
	// Stateless split preview for the thread composer. Static route,
	// registered before "/:id/..." so it isn't shadowed (same reason as above).
	g.Post("/thread/preview", h.auth, h.PreviewThread)
	g.Get("/:id", h.auth, h.Get)
	g.Put("/:id", h.auth, h.Update)
	// Targeted set of a post's own brand voice + audience.
	g.Put("/:id/brand", h.auth, h.SetBrand)
	// Targeted membership writes for a post's sources, so attaching or
	// detaching one source touches only used_asset_ids. Respects the CON-251
	// content-lock: a real change to a submitted post's sources is a 409.
	g.Post("/:id/assets", h.auth, h.AddAssets)
	g.Delete("/:id/assets/:assetId", h.auth, h.RemoveAsset)
	g.Delete("/:id", h.auth, h.Delete)
	g.Get("/:id/versions", h.auth, h.ListVersions)
	g.Post("/:id/versions", h.auth, h.CreateVersion)
	g.Post("/:id/schedule", h.auth, h.Schedule)
	g.Post("/:id/cancel", h.auth, h.Cancel)

	app.Get("/api/campaigns/:campaign_id/posts", h.auth, h.ListByCampaign)
}

type postRequest struct {
	CampaignID string `json:"campaign_id"             validate:"required"`
	// PlatformID + PlatformPostType are required only when status is not
	// "draft" — see update.RequirePlatformIfNotDraft. Drafts can be saved
	// before the user has picked a platform.
	PlatformID       string `json:"platform_id"`
	PlatformPostType string `json:"platform_post_type"`
	// SocialAccountID names which same-platform account the post
	// publishes to. Optional: omit it and the submit worker auto-selects the
	// platform's single account, or requires a choice when there are several.
	SocialAccountID string `json:"social_account_id"`
	Title           string `json:"title"`
	Content         string `json:"content"`
	// ThreadSegments is DERIVED, not authored (R2): a thread is written
	// as a single body in Content (with "---" delimiter lines, or auto-split by the
	// per-segment char limit), and the server materialises the segment list from it.
	// A client-sent value here is IGNORED — the field is retained only so existing
	// callers that still send it don't error, and GET responses carry the derived
	// list back on the Post model (not this request type).
	ThreadSegments      models.ThreadSegments `json:"thread_segments"`
	MediaURLs           models.StringSlice    `json:"media_urls"`
	ScheduledAt         *time.Time            `json:"scheduled_at"`
	PublishedAt         *time.Time            `json:"published_at"`
	Status              models.PostStatus     `json:"status"`
	CTAType             models.PostCTAType    `json:"cta_type"`
	CTAUrl              string                `json:"cta_url"`
	TargetAudienceNotes string                `json:"target_audience_notes"`
	// UsedAssetIDs is presence-aware: the sources have their own
	// membership endpoints (POST/DELETE /posts/:id/assets), so an ordinary
	// whole-record save that omits the key must leave the stored set alone rather
	// than restate it and race the membership write. A present array replaces it;
	// an explicit null clears it. See Optional and applyOptionalSlice.
	UsedAssetIDs        Optional[models.StringSlice] `json:"used_asset_ids"`
	CampaignTypePhaseID *string                      `json:"campaign_type_phase_id"`
	// BrandVoiceID / BrandAudienceID are presence-aware: unlike the
	// other client-authored fields on this full-replace body, these are stamped
	// server-side by content_plan / draft_post, so an omitted key must leave the
	// stored ref untouched rather than null it. See Optional and apply.
	BrandVoiceID    Optional[string] `json:"brand_voice_id"`
	BrandAudienceID Optional[string] `json:"brand_audience_id"`
	// ContentFormat is presence-aware so an autosave from a client that predates
	// the field can't wipe it: omitted leaves it alone, null clears it.
	ContentFormat Optional[models.ContentFormat] `json:"content_format" swaggertype:"string" enums:"how-to,explainer,listicle,story,digest,opinion,question,announcement"`
	// PublishedURL lets the front-end record a permalink for posts
	// Zernio cannot verify (the CON-149 skip path — e.g. LinkedIn personal
	// accounts) or correct a wrong one. Like every field on this whole-resource
	// PUT, the FE round-trips the current value; an empty string clears it.
	PublishedURL string `json:"published_url"`
}

func (r *postRequest) toStatus() models.PostStatus {
	if r.Status == "" {
		return models.PostStatusDraft
	}
	return r.Status
}

func (r *postRequest) toCTAType() models.PostCTAType {
	if r.CTAType == "" {
		return models.CTATypeNone
	}
	return r.CTAType
}

// apply copies the mutable fields from a parsed request onto an
// existing Post, including the resolved status/ctaType (passed
// separately because they're already normalized by toStatus/toCTAType
// and re-validated by the caller) and a fresh UpdatedAt.
func (r *postRequest) apply(post *models.Post, status models.PostStatus, ctaType models.PostCTAType) {
	post.CampaignID = r.CampaignID
	post.PlatformID = r.PlatformID
	post.PlatformPostType = r.PlatformPostType
	post.SocialAccountID = r.SocialAccountID
	post.Title = r.Title
	post.Content = r.Content
	// Content is the canonical thread body — thread_segments is DERIVED
	// from it, not authored as an array, so apply only carries the body. The
	// Create/Update handlers call deriveThreadSegments right after this to
	// materialise (or, for a non-thread, clear) the segments with the resolved
	// per-segment limit. A client-sent thread_segments is ignored.
	post.MediaURLs = nullSlice(r.MediaURLs)
	post.ScheduledAt = r.ScheduledAt
	post.PublishedAt = r.PublishedAt
	post.Status = status
	post.CTAType = ctaType
	post.CTAUrl = r.CTAUrl
	post.CampaignTypePhaseID = r.CampaignTypePhaseID
	post.TargetAudienceNotes = r.TargetAudienceNotes
	// published_url stays writable after publish on purpose — recording
	// a permalink is a post-publish affordance. When CON-251's content-lock
	// lands, keep this field on the allowed-after-submit list.
	post.PublishedURL = r.PublishedURL
	// Presence-aware: omitting these leaves the server-stamped refs in
	// place; an explicit null clears them.
	r.BrandVoiceID.applyTo(&post.BrandVoiceID)
	r.BrandAudienceID.applyTo(&post.BrandAudienceID)
	r.ContentFormat.applyTo(&post.ContentFormat)
	// Presence-aware: omit to leave the sources alone (the membership
	// endpoints own them), a present array to replace, an explicit null to clear.
	applyOptionalSlice(r.UsedAssetIDs, &post.UsedAssetIDs)
	post.UpdatedAt = time.Now().UTC()
}

// mutatesLockedContent reports whether the request would change any of the
// content-identity fields CON-251 freezes once a post is submitted: the
// body, title, media, platform, post type, format, or the sources it was
// built from. The date and account are locked by the schedule/cancel flows that
// own them, and a status-only transition (e.g. unschedule to edit) leaves
// every field below equal, so neither is compared here — this gates the
// silent-divergence edit, not the legitimate move off a submitted state.
func (r *postRequest) mutatesLockedContent(post *models.Post) bool {
	return r.Content != post.Content ||
		r.Title != post.Title ||
		r.PlatformID != post.PlatformID ||
		r.PlatformPostType != post.PlatformPostType ||
		!slices.Equal(nullSlice(r.MediaURLs), post.MediaURLs) ||
		// A thread's message list is locked content too, but content is
		// now the canonical thread body (thread_segments is derived from it), so the
		// r.Content != post.Content check above already covers any thread edit — no
		// separate segment comparison is needed.
		// Sources are presence-aware: an omitted key preserves the set,
		// so only a present-and-different value is a mutation of the locked content.
		(r.UsedAssetIDs.Present && !slices.Equal(nullSlice(r.UsedAssetIDs.orZero()), post.UsedAssetIDs)) ||
		// The shape a submitted post took is a fact about what went out.
		(r.ContentFormat.Present && !equalPtr(r.ContentFormat.Value, post.ContentFormat))
}

// validateContentFormat rejects a present content_format that is not a known
// slug. Absent and explicit null are both fine; "" is refused because "no
// format" is null, never the empty string.
func (r *postRequest) validateContentFormat() error {
	if v := r.ContentFormat.Value; v != nil && !v.IsValid() {
		return fiber.NewError(fiber.StatusBadRequest, "invalid content_format")
	}
	return nil
}

// equalPtr reports whether two optional values are both nil or both point to
// equal values.
func equalPtr[T comparable](a, b *T) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// omitColumns lists the presence-aware columns an omitted field must keep out
// of the whole-record UPDATE: apply already left the hydrated value in place,
// but writing it back would clobber a concurrent membership write
// (AddUsedAssetIDs/RemoveUsedAssetID) that landed after the read.
func (r *postRequest) omitColumns() []string {
	return omitAbsent(columnPresence{"used_asset_ids", r.UsedAssetIDs.Present})
}

// validateForCreate runs the publish gate (CON-69 §4 attachment rules
// + CON-74 §post-type rules) when a post is being created directly in
// a non-draft state. Returns done=true after writing a 422 so the
// caller stops; done=false means the gate passed (or did not apply)
// and the caller may continue. New posts have no attachments yet, so
// only the structural rules can fire.
func (h *PostsHandler) validateForCreate(c *fiber.Ctx, post *models.Post) (done bool, err error) {
	if post.Status == models.PostStatusDraft || post.PlatformID == "" || h.platformRepo == nil {
		return false, nil
	}
	platform, err := h.platformRepo.GetByID(reqCtx(c), post.PlatformID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, fiber.NewError(fiber.StatusBadRequest, "platform not found")
		}
		return false, err
	}
	// New posts carry no attachments yet, so only the structural rules fire —
	// including the per-segment thread checks (a create-as-thread with < 2
	// segments is rejected). CON-284.
	errsByPlatform := platforms.ValidatePublishReadiness(post, platform, nil)
	if !update.HasValidationErrors(errsByPlatform) {
		return false, nil
	}
	h.recordActivity(c, "post_validation_failed",
		activity.WithEntity("post", post.ID),
		activity.WithStatus("failed"),
	)
	if err := c.Status(fiber.StatusUnprocessableEntity).JSON(fiber.Map{
		"error":               "post is not ready for publish",
		"platform_validation": errsByPlatform,
	}); err != nil {
		return true, err
	}
	return true, nil
}

// List godoc
// @Summary      List posts
// @Description  Returns all posts ordered by creation date, with campaign, platform, and assets hydrated.
// @Tags         posts
// @Produce      json
// @Security     CookieAuth
// @Success      200  {array}   models.Post
// @Failure      401  {object}  map[string]string
// @Router       /api/posts [get]
func (h *PostsHandler) List(c *fiber.Ctx) error {
	posts, err := h.repo.List(reqCtx(c))
	if err != nil {
		return err
	}
	h.decorateCovers(c, posts)
	return c.JSON(posts)
}

// ListByCampaign godoc
// @Summary      List posts by campaign
// @Description  Returns all posts for the given campaign ID, with campaign, platform, and assets hydrated.
// @Tags         posts
// @Produce      json
// @Security     CookieAuth
// @Param        campaign_id  path      string  true  "Campaign Sqid"
// @Success      200          {array}   models.Post
// @Failure      401          {object}  map[string]string
// @Router       /api/campaigns/{campaign_id}/posts [get]
func (h *PostsHandler) ListByCampaign(c *fiber.Ctx) error {
	posts, err := h.repo.ListByCampaign(reqCtx(c), c.Params("campaign_id"))
	if err != nil {
		return err
	}
	h.decorateCovers(c, posts)
	return c.JSON(posts)
}

// previewThreadRequest is the body for POST /api/posts/thread/preview (CON-284
// R2): the single authored body plus the target platform, whose per-segment char
// limit drives the auto-split.
type previewThreadRequest struct {
	Content    string `json:"content"`
	PlatformID string `json:"platform_id"`
}

type previewSegment struct {
	Content   string `json:"content"`
	CharCount int    `json:"char_count"`
}

// previewThreadResponse mirrors what a thread write would derive+validate, so the
// composer can render the segment breakdown, per-segment counts, and any publish-
// gate errors before saving. Nothing is persisted.
type previewThreadResponse struct {
	Segments []previewSegment            `json:"segments"`
	Limit    int                         `json:"limit"`
	Valid    bool                        `json:"valid"`
	Errors   []platforms.ValidationError `json:"errors"`
}

// PreviewThread godoc
// @Summary      Preview a thread split
// @Description  Splits a single authored body into thread segments (manual "---"
// @Description  delimiters, else auto-split by the platform's per-segment limit)
// @Description  and validates them exactly as the publish gate will. Stateless.
// @Tags         posts
// @Security     CookieAuth
// @Accept       json
// @Produce      json
// @Param        body  body      previewThreadRequest  true  "Body + target platform"
// @Success      200   {object}  previewThreadResponse
// @Failure      400   {object}  map[string]string
// @Failure      404   {object}  map[string]string
// @Router       /api/posts/thread/preview [post]
func (h *PostsHandler) PreviewThread(c *fiber.Ctx) error {
	var req previewThreadRequest
	if err := c.BodyParser(&req); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, err.Error())
	}
	if req.PlatformID == "" {
		return fiber.NewError(fiber.StatusBadRequest, "platform_id is required")
	}

	var platform *models.Platform
	if h.platformRepo != nil {
		p, err := h.platformRepo.GetByID(reqCtx(c), req.PlatformID)
		if err != nil {
			return notFound(err, "platform not found")
		}
		platform = p
	}

	limit := update.ThreadLimitOf(platform)
	segs := platforms.SplitThread(req.Content, limit)

	out := make([]previewSegment, len(segs))
	for i, s := range segs {
		// char_count is the flattened, visible length — the same number the
		// publish gate enforces (VisibleLen), so the composer's per-message counter
		// matches what actually gets validated. Counting raw Markdown here would
		// overstate a segment with bold/links/headings.
		out[i] = previewSegment{Content: s.Content, CharCount: platforms.VisibleLen(s.Content)}
	}

	// Validate the derived thread exactly as the publish gate will (per-segment
	// count/limit/empty rules). No attachments are considered in a preview.
	errs := []platforms.ValidationError{}
	if platform != nil {
		post := &models.Post{PlatformPostType: models.PostTypeThread, Content: req.Content, ThreadSegments: segs}
		if byPlatform := platforms.ValidatePublishReadiness(post, platform, nil); byPlatform[platform.ID] != nil {
			errs = byPlatform[platform.ID]
		}
	}

	return c.JSON(previewThreadResponse{
		Segments: out,
		Limit:    limit,
		Valid:    len(errs) == 0,
		Errors:   errs,
	})
}

// Create godoc
// @Summary      Create post
// @Description  Creates a new post. The created_by field is set from the authenticated session.
// @Description  The `content` field is a Markdown string; the frontend renders it via BlockNote.
// @Description  `platform_id` and `platform_post_type` are required only when `status` is not `draft`;
// @Description  drafts can be saved without a platform chosen up front.
// @Tags         posts
// @Accept       json
// @Produce      json
// @Security     CookieAuth
// @Param        body  body      postRequest  true  "Post payload"
// @Success      201   {object}  models.Post
// @Failure      400   {object}  map[string]string
// @Failure      401   {object}  map[string]string
// @Router       /api/posts [post]
func (h *PostsHandler) Create(c *fiber.Ctx) error {
	var req postRequest
	if err := c.BodyParser(&req); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, err.Error())
	}
	if err := validate.Struct(&req); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, validationError(err).Error())
	}
	status := req.toStatus()
	if !validPostStatuses[status] {
		return fiber.NewError(fiber.StatusBadRequest, "invalid status")
	}
	ctaType := req.toCTAType()
	if !validCTATypes[ctaType] {
		return fiber.NewError(fiber.StatusBadRequest, "invalid cta_type")
	}
	if err := req.validateContentFormat(); err != nil {
		return err
	}
	if err := update.RequirePlatformIfNotDraft(status, req.PlatformID, req.PlatformPostType); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, err.Error())
	}

	session, err := sessionFrom(c)
	if err != nil {
		return err
	}

	id, err := models.NewID()
	if err != nil {
		return err
	}

	post := &models.Post{
		ID:                  id,
		CampaignID:          req.CampaignID,
		PlatformID:          req.PlatformID,
		PlatformPostType:    req.PlatformPostType,
		Title:               req.Title,
		Content:             req.Content,
		MediaURLs:           nullSlice(req.MediaURLs),
		ScheduledAt:         req.ScheduledAt,
		PublishedAt:         req.PublishedAt,
		Status:              status,
		CTAType:             ctaType,
		CTAUrl:              req.CTAUrl,
		TargetAudienceNotes: req.TargetAudienceNotes,
		UsedAssetIDs:        nullSlice(req.UsedAssetIDs.orZero()),
		CampaignTypePhaseID: req.CampaignTypePhaseID,
		ContentFormat:       req.ContentFormat.Value,
		CreatedBy:           session.UserID,
		UsedAssets:          []models.Asset{},
	}
	// Derive the thread's segment list from the canonical body (a
	// non-thread post gets an empty list). Runs before validateForCreate so the
	// create-time publish gate sees the same segments submit will publish.
	h.deriveThreadSegments(reqCtx(c), post)

	if ok, err := h.checkPhase(reqCtx(c), post.CampaignID, post.CampaignTypePhaseID); err != nil {
		return err
	} else if !ok {
		return rejectInvalidPhase(c)
	}

	if done, err := h.validateForCreate(c, post); err != nil {
		return err
	} else if done {
		return nil
	}

	if err := h.repo.Create(reqCtx(c), post); err != nil {
		if repository.IsConstraintViolation(err, repository.ConstraintPhaseMatchesCampaignType) {
			return rejectInvalidPhase(c)
		}
		return err
	}
	h.recordActivity(c, "post_created",
		activity.WithEntity("post", post.ID),
		activity.WithPayload(map[string]any{"status": string(post.Status), "campaign_id": post.CampaignID}),
	)
	return c.Status(fiber.StatusCreated).JSON(post)
}

// Get godoc
// @Summary      Get post
// @Description  Returns a single post by Sqid with campaign, platform, and assets hydrated.
// @Tags         posts
// @Produce      json
// @Security     CookieAuth
// @Param        id   path      string  true  "Post Sqid"
// @Success      200  {object}  models.Post
// @Failure      401  {object}  map[string]string
// @Failure      404  {object}  map[string]string
// @Router       /api/posts/{id} [get]
func (h *PostsHandler) Get(c *fiber.Ctx) error {
	post, err := load(c, h.repo.GetByID, "post not found")
	if err != nil {
		return err
	}
	h.decorateCover(c, post)
	return c.JSON(post)
}

// Update godoc
// @Summary      Update post
// @Description  Replaces all mutable fields of an existing post.
// @Description  The `content` field is a Markdown string; the frontend renders it via BlockNote.
// @Description  `platform_id` and `platform_post_type` are required only when the new `status` is
// @Description  not `draft`; transitioning a draft to `ready_for_publish` (or beyond) without a
// @Description  platform chosen returns 400.
// @Tags         posts
// @Accept       json
// @Produce      json
// @Security     CookieAuth
// @Param        id    path      string      true  "Post Sqid"
// @Param        body  body      postRequest true  "Post payload"
// @Success      200   {object}  models.Post
// @Failure      400   {object}  map[string]string
// @Failure      401   {object}  map[string]string
// @Failure      404   {object}  map[string]string
// @Router       /api/posts/{id} [put]
func (h *PostsHandler) Update(c *fiber.Ctx) error {
	var req postRequest
	if err := bindAndValidate(c, &req); err != nil {
		return err
	}
	status := req.toStatus()
	if !validPostStatuses[status] {
		return fiber.NewError(fiber.StatusBadRequest, "invalid status")
	}
	ctaType := req.toCTAType()
	if !validCTATypes[ctaType] {
		return fiber.NewError(fiber.StatusBadRequest, "invalid cta_type")
	}
	if err := req.validateContentFormat(); err != nil {
		return err
	}

	if err := validateBrandRefs(reqCtx(c), h.brandRepo, req.BrandVoiceID.Value, req.BrandAudienceID.Value); err != nil {
		return err
	}

	post, err := load(c, h.repo.GetByID, "post not found")
	if err != nil {
		return err
	}

	res, err := h.updater().Update(reqCtx(c), update.Input{
		Post:                 post,
		Status:               status,
		Apply:                func(p *models.Post) { req.apply(p, status, ctaType) },
		CampaignID:           req.CampaignID,
		PhaseID:              req.CampaignTypePhaseID,
		PlatformID:           req.PlatformID,
		PlatformPostType:     req.PlatformPostType,
		Content:              req.Content,
		MutatesLockedContent: req.mutatesLockedContent(post),
		Omit:                 req.omitColumns(),
		Actor:                cmp.Or(actorID(c), models.ActorSystem),
	})
	if res.AutoPublishDecision != "" {
		c.Set("X-Auto-Publish-Decision", res.AutoPublishDecision)
	}
	if err != nil {
		return postUpdateError(c, err)
	}
	h.recordActivity(c, "post_updated",
		activity.WithEntity("post", post.ID),
		activity.WithStatus(string(res.Post.Status)),
	)
	h.decorateCover(c, res.Post)
	return c.JSON(res.Post)
}

// postUpdateError maps a post update use-case error onto its HTTP response.
func postUpdateError(c *fiber.Ctx, err error) error {
	if e, ok := errors.AsType[*update.TransitionError](err); ok {
		return fiber.NewError(fiber.StatusBadRequest, e.Error())
	}
	if e, ok := errors.AsType[*update.ContentLockedError](err); ok {
		return submittedLockError(e.Status)
	}
	if e, ok := errors.AsType[*update.ValidationError](err); ok {
		return fiber.NewError(fiber.StatusBadRequest, e.Msg)
	}
	if errors.Is(err, update.ErrInvalidPhase) {
		return rejectInvalidPhase(c)
	}
	if e, ok := errors.AsType[*update.NotReadyError](err); ok {
		return c.Status(fiber.StatusUnprocessableEntity).JSON(fiber.Map{
			"error":               e.Error(),
			"platform_validation": e.PlatformValidation,
		})
	}
	if e, ok := errors.AsType[*schedule.AccountSelectionError](err); ok {
		return writeAccountSelectionError(c, e)
	}
	return err
}

// Delete godoc
// @Summary      Delete post
// @Description  Deletes a post by Sqid.
// @Tags         posts
// @Security     CookieAuth
// @Param        id   path  string  true  "Post Sqid"
// @Success      204
// @Failure      401  {object}  map[string]string
// @Failure      404  {object}  map[string]string
// @Router       /api/posts/{id} [delete]
func (h *PostsHandler) Delete(c *fiber.Ctx) error {
	id := c.Params("id")
	// Run the before-delete hook (S3 cleanup) before the row is
	// dropped — the hook needs the s3_keys, which the FK cascade will
	// erase on row delete. A hook error fails the whole DELETE so the
	// caller can retry; we don't end up with an orphaned bucket.
	if h.onBeforeDelete != nil {
		if err := h.onBeforeDelete(reqCtx(c), id); err != nil {
			return err
		}
	}
	deleted, err := h.repo.Delete(reqCtx(c), id)
	if err != nil {
		return err
	}
	if !deleted {
		return fiber.NewError(fiber.StatusNotFound, "post not found")
	}
	h.recordActivity(c, "post_deleted", activity.WithEntity("post", id))
	return c.SendStatus(fiber.StatusNoContent)
}

// ── Versions ─────────────────────────────────────────────────────────────────

// ListVersions godoc
// @Summary      List post versions
// @Description  Returns all version snapshots for a post.
// @Tags         posts
// @Produce      json
// @Security     CookieAuth
// @Param        id   path      string  true  "Post Sqid"
// @Success      200  {array}   models.PostVersion
// @Failure      401  {object}  map[string]string
// @Router       /api/posts/{id}/versions [get]
func (h *PostsHandler) ListVersions(c *fiber.Ctx) error {
	versions, err := h.versionRepo.ListByPostID(reqCtx(c), c.Params("id"))
	if err != nil {
		return err
	}
	return c.JSON(versions)
}

type createVersionRequest struct {
	Note string `json:"note"`
}

// CreateVersion godoc
// @Summary      Create post version
// @Description  Manually creates a version snapshot of the current post content.
// @Tags         posts
// @Accept       json
// @Produce      json
// @Security     CookieAuth
// @Param        id    path      string               true  "Post Sqid"
// @Param        body  body      createVersionRequest true  "Version payload"
// @Success      201   {object}  models.PostVersion
// @Failure      401   {object}  map[string]string
// @Failure      404   {object}  map[string]string
// @Router       /api/posts/{id}/versions [post]
func (h *PostsHandler) CreateVersion(c *fiber.Ctx) error {
	var req createVersionRequest
	if err := c.BodyParser(&req); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, err.Error())
	}

	post, err := load(c, h.repo.GetByID, "post not found")
	if err != nil {
		return err
	}

	id, err := models.NewID()
	if err != nil {
		return err
	}

	version := &models.PostVersion{
		ID:      id,
		PostID:  post.ID,
		Content: post.Content,
		Note:    req.Note,
		Creator: "user",
	}
	if err := h.versionRepo.CreateNext(reqCtx(c), version); err != nil {
		return err
	}
	return c.Status(fiber.StatusCreated).JSON(version)
}
