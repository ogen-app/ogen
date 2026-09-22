package handlers

import (
	"errors"

	"github.com/gofiber/fiber/v2"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/activity"
	"github.com/ogen-app/ogen/src/usecase/post_actions/clone"
	"github.com/ogen-app/ogen/src/usecase/post_actions/restore"
)

// PostActionsHandler owns the post derivation actions — clone (CON-59: duplicate
// a post as a new draft) and restore (CON-68: roll a post back to an earlier
// version). Split out of the PostsHandler god-object (CON-291): a focused handler
// over the two action services, each nil-disabling its endpoint with a 503.
type PostActionsHandler struct {
	repo       repository.PostRepository
	cloneSvc   *clone.Service
	restoreSvc *restore.Service
	activity   *activity.Recorder
	auth       fiber.Handler
}

// NewPostActionsHandler wires the clone + restore endpoints. cloneSvc /
// restoreSvc are optional — a nil one leaves its endpoint at 503. activity nil
// is a no-op.
func NewPostActionsHandler(
	repo repository.PostRepository,
	cloneSvc *clone.Service,
	restoreSvc *restore.Service,
	activityRec *activity.Recorder,
	auth fiber.Handler,
) *PostActionsHandler {
	return &PostActionsHandler{
		repo:       repo,
		cloneSvc:   cloneSvc,
		restoreSvc: restoreSvc,
		activity:   activityRec,
		auth:       auth,
	}
}

func (h *PostActionsHandler) Register(app *fiber.App) {
	app.Post("/api/posts/:id/clone", h.auth, h.Clone)
	app.Post("/api/posts/:id/restore", h.auth, h.Restore)
}

// recordActivity emits a CON-125 post-category activity event with the API
// source pre-set. A private copy of the PostsHandler helper of the same name.
func (h *PostActionsHandler) recordActivity(c *fiber.Ctx, typ string, opts ...activity.Option) {
	h.activity.Record(reqCtx(c), activity.CategoryPost, typ,
		append([]activity.Option{activity.WithSource(activity.SourceAPI)}, opts...)...)
}

type cloneRequest struct {
	TargetPlatformID string  `json:"target_platform_id"`
	TargetPostType   string  `json:"target_post_type"`
	Title            *string `json:"title"`
}

// Clone godoc
// @Summary      Clone post
// @Description  Duplicates a post as a new draft in the same campaign and phase, copying
// @Description  title, content, media/attachments, used assets, CTA, and audience notes.
// @Description  Attachments are deep-copied in object storage so the clone is independent
// @Description  of its source. Pass target_platform_id/target_post_type to retarget the
// @Description  clone (verbatim content — the assistant path adapts content for you).
// @Tags         posts
// @Accept       json
// @Produce      json
// @Security     CookieAuth
// @Param        id    path      string        true   "Source post Sqid"
// @Param        body  body      cloneRequest  false  "Optional clone overrides"
// @Success      201   {object}  models.Post
// @Failure      400   {object}  map[string]string
// @Failure      401   {object}  map[string]string
// @Failure      404   {object}  map[string]string
// @Failure      503   {object}  map[string]string
// @Router       /api/posts/{id}/clone [post]
func (h *PostActionsHandler) Clone(c *fiber.Ctx) error {
	if h.cloneSvc == nil {
		return fiber.NewError(fiber.StatusServiceUnavailable, "clone is not available")
	}

	var req cloneRequest
	// The body is optional; only parse when one was sent so an empty
	// POST (the common "just duplicate it" case) doesn't 400.
	if len(c.Body()) > 0 {
		if err := c.BodyParser(&req); err != nil {
			return fiber.NewError(fiber.StatusBadRequest, err.Error())
		}
	}

	session := c.Locals("session").(*models.Session)
	opts := clone.DefaultOptions(session.UserID, clone.TriggerAPI)
	opts.TargetPlatformID = req.TargetPlatformID
	opts.TargetPostType = req.TargetPostType
	opts.TitleOverride = req.Title

	res, err := h.cloneSvc.Clone(reqCtx(c), c.Params("id"), opts)
	if err != nil {
		switch {
		case errors.Is(err, clone.ErrSourceNotFound):
			return fiber.NewError(fiber.StatusNotFound, "post not found")
		case errors.Is(err, clone.ErrInvalidPlatform):
			return fiber.NewError(fiber.StatusBadRequest, err.Error())
		}
		return err
	}
	h.recordActivity(c, "post_cloned",
		activity.WithEntity("post", res.Post.ID),
		activity.WithPayload(map[string]any{"source_post_id": c.Params("id")}),
	)
	return c.Status(fiber.StatusCreated).JSON(res.Post)
}

type restoreRequest struct {
	VersionNumber int `json:"version_number"`
}

// Restore godoc
// @Summary      Restore post to a version
// @Description  Restore is non-destructive: the target version's content is
// @Description  copied into a brand-new version that becomes the new HEAD, so
// @Description  the full history is preserved and the restore is itself
// @Description  reversible. If the live post has unsnapshotted edits, they are
// @Description  auto-saved as a version first so nothing is lost. Restoring to
// @Description  the version that already matches the current content is a no-op.
// @Tags         posts
// @Accept       json
// @Produce      json
// @Security     CookieAuth
// @Param        id    path      string          true  "Post Sqid"
// @Param        body  body      restoreRequest  true  "Target version"
// @Success      200   {object}  map[string]interface{}
// @Failure      400   {object}  map[string]string
// @Failure      401   {object}  map[string]string
// @Failure      404   {object}  map[string]string
// @Failure      503   {object}  map[string]string
// @Router       /api/posts/{id}/restore [post]
func (h *PostActionsHandler) Restore(c *fiber.Ctx) error {
	if h.restoreSvc == nil {
		return fiber.NewError(fiber.StatusServiceUnavailable, "restore is not available")
	}
	var req restoreRequest
	if err := c.BodyParser(&req); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, err.Error())
	}
	if req.VersionNumber <= 0 {
		return fiber.NewError(fiber.StatusBadRequest, "version_number is required and must be positive")
	}

	session := c.Locals("session").(*models.Session)
	res, err := h.restoreSvc.Restore(reqCtx(c), c.Params("id"), restore.Options{
		Actor:         session.UserID,
		Trigger:       restore.TriggerAPI,
		VersionNumber: req.VersionNumber,
	})
	if err != nil {
		switch {
		case errors.Is(err, restore.ErrPostNotFound):
			return fiber.NewError(fiber.StatusNotFound, "post not found")
		case errors.Is(err, restore.ErrVersionNotFound):
			return fiber.NewError(fiber.StatusNotFound, "version not found")
		case errors.Is(err, restore.ErrNotEditable):
			return fiber.NewError(fiber.StatusBadRequest, err.Error())
		}
		return err
	}

	// Re-fetch so the response carries a fully hydrated post (campaign /
	// platform / assets), matching the Update handler's contract.
	updated, err := h.repo.GetByID(reqCtx(c), c.Params("id"))
	if err != nil {
		return err
	}
	h.recordActivity(c, "post_restored",
		activity.WithEntity("post", c.Params("id")),
		activity.WithPayload(map[string]any{
			"restored_from_version": res.RestoredFromVersion,
			"new_version_number":    res.NewVersionNumber,
			"no_op":                 res.NoOp,
		}),
	)
	return c.JSON(fiber.Map{
		"post":                  updated,
		"restored_from_version": res.RestoredFromVersion,
		"new_version_number":    res.NewVersionNumber,
		"auto_snapshot_created": res.AutoSnapshotCreated,
		"no_op":                 res.NoOp,
	})
}
