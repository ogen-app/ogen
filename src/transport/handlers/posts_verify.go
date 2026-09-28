package handlers

import (
	"context"

	"github.com/gofiber/fiber/v2"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/eventhub"
	"github.com/ogen-app/ogen/src/infra/publishers/zernio"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/jobs"
	"github.com/ogen-app/ogen/src/usecase/accountselect"
	"github.com/ogen-app/ogen/src/usecase/post_actions/schedule"
	"github.com/ogen-app/ogen/src/usecase/post_actions/verify"
)

// PostVerificationHandler owns POST /api/posts/:id/verify-external:
// confirm a manually-published post via Zernio's sync-external, then back-fill
// its publisher linkage + a first analytics snapshot. It was split out of the
// PostsHandler god-object — a focused handler with only the deps this
// one flow needs (external client, account resolution, analytics, versions).
type PostVerificationHandler struct {
	repo              repository.PostRepository
	zernioClient      *zernio.Client
	socialAccountRepo repository.SocialAccountRepository
	profileID         func(ctx context.Context) (string, error)
	analyticsRepo     repository.PostAnalyticsRepository
	versionRepo       repository.PostVersionRepository
	analyticsHub      eventhub.Hub
	auth              fiber.Handler
}

// NewPostVerificationHandler wires the CON-153 external-post verification path.
// A nil zernioClient leaves POST /:id/verify-external at 503.
func NewPostVerificationHandler(
	repo repository.PostRepository,
	zernioClient *zernio.Client,
	accounts repository.SocialAccountRepository,
	profileID func(ctx context.Context) (string, error),
	analyticsRepo repository.PostAnalyticsRepository,
	versionRepo repository.PostVersionRepository,
	hub eventhub.Hub,
	auth fiber.Handler,
) *PostVerificationHandler {
	return &PostVerificationHandler{
		repo:              repo,
		zernioClient:      zernioClient,
		socialAccountRepo: accounts,
		profileID:         profileID,
		analyticsRepo:     analyticsRepo,
		versionRepo:       versionRepo,
		analyticsHub:      hub,
		auth:              auth,
	}
}

func (h *PostVerificationHandler) Register(app *fiber.App) {
	app.Post("/api/posts/:id/verify-external", h.auth, h.VerifyExternal)
}

// verifyExternalRequest is the body for POST /:id/verify-external. At least
// one of url / post_id must be present.
type verifyExternalRequest struct {
	URL    string `json:"url"`
	PostID string `json:"post_id"`
}

// VerifyExternal godoc
// @Summary      Verify a manually-published post
// @Description  Confirms a manually-published post exists on the platform via Zernio sync-external, back-fills its publisher linkage + permalink, marks it published, and records a first analytics snapshot (CON-153).
// @Tags         posts
// @Accept       json
// @Produce      json
// @Security     CookieAuth
// @Param        id    path      string                true  "Post Sqid"
// @Param        body  body      verifyExternalRequest true  "Verification payload"
// @Success      200   {object}  map[string]any
// @Failure      404   {object}  map[string]string
// @Router       /api/posts/{id}/verify-external [post]
func (h *PostVerificationHandler) VerifyExternal(c *fiber.Ctx) error {
	if h.zernioClient == nil {
		return fiber.NewError(fiber.StatusServiceUnavailable, "external verification is not available")
	}
	var body verifyExternalRequest
	if err := c.BodyParser(&body); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid request body")
	}
	if body.URL == "" && body.PostID == "" {
		return fiber.NewError(fiber.StatusBadRequest, "url or post_id is required")
	}

	post, err := load(c, h.repo.GetByID, "post not found")
	if err != nil {
		return err
	}
	profileID, err := h.resolveProfileID(c)
	if err != nil {
		return err
	}
	// Resolve the connected account whose token reads the platform.
	supported := zernio.LookupSupportedBySqid(post.PlatformID)
	if supported == nil {
		return fiber.NewError(fiber.StatusConflict, "post has no supported platform")
	}
	accountID, handled, err := h.resolveExternalAccountID(c, post, profileID, supported.ZernioID)
	if handled {
		return err
	}

	result, err := h.zernioClient.SyncExternalPost(reqCtx(c), zernio.SyncExternalRequest{
		AccountID: accountID,
		URL:       body.URL,
		PostID:    body.PostID,
	})
	if err != nil && !zernio.IsStatus(err, fiber.StatusNotFound) {
		jobs.ZernioExternalVerifyFailed.Add(1)
		return fiber.NewError(fiber.StatusBadGateway, "sync-external upstream error")
	}
	// A 404 means the platform has no such post — a normal "not found".
	if err != nil || !result.Found || result.Post == nil {
		jobs.ZernioExternalVerifyNotFound.Add(1)
		return c.JSON(fiber.Map{"found": false})
	}
	ext := result.Post

	svc := &verify.Service{Posts: h.repo, Versions: h.versionRepo, Analytics: h.analyticsRepo, Hub: h.analyticsHub}
	if err := svc.Confirm(reqCtx(c), post, ext); err != nil {
		jobs.ZernioExternalVerifyFailed.Add(1)
		return err
	}

	jobs.ZernioExternalVerifySucceeded.Add(1)
	return c.JSON(fiber.Map{
		"found": true,
		"post": fiber.Map{
			"id": post.ID,
			// The confirmed external post's own id — the one ext.Analytics
			// belong to (post.PublisherPostID is left as-is when already set).
			"publisher_post_id": ext.PlatformPostID,
			// The persisted permalink, so the FE can render "View post"
			// without a re-fetch.
			"published_url": post.PublishedURL,
			"sync_status":   "synced",
		},
		// The fetched metrics, whether or not analytics persistence is wired.
		"analytics": verify.ExternalMetrics(ext.Analytics),
	})
}

// resolveProfileID returns the tenant's Zernio profile, or a 503 when the
// integration isn't configured.
func (h *PostVerificationHandler) resolveProfileID(c *fiber.Ctx) (string, error) {
	profileID := ""
	if h.profileID != nil {
		var err error
		if profileID, err = h.profileID(reqCtx(c)); err != nil {
			return "", err
		}
	}
	if profileID == "" {
		return "", fiber.NewError(fiber.StatusServiceUnavailable, "zernio integration is not configured")
	}
	return profileID, nil
}

// resolveExternalAccountID resolves the Zernio account id to read the platform
// with, applying the CON-150 rules. It returns (accountID, handled, err): when
// handled is true the caller must return err immediately — either a response
// has already been written (in which case err is the fiber write result, which
// is nil on success) or err is a real failure. When handled is false, accountID
// is non-empty and safe to use. The explicit handled flag is required because
// c.Status(...).JSON(...) / writeAccountSelectionError return nil on a
// successful write, so a nil error alone cannot signal "already responded".
func (h *PostVerificationHandler) resolveExternalAccountID(c *fiber.Ctx, post *models.Post, profileID, zernioPlatform string) (string, bool, error) {
	if h.socialAccountRepo == nil {
		return "", true, fiber.NewError(fiber.StatusServiceUnavailable, "social accounts are not available")
	}
	res, err := accountselect.Resolve(reqCtx(c), h.socialAccountRepo, profileID, post, zernioPlatform)
	if err != nil {
		return "", true, err
	}
	switch res.Outcome {
	case accountselect.Resolved:
		return res.AccountID, false, nil
	case accountselect.Unavailable:
		return "", true, writeAccountSelectionError(c, &schedule.AccountSelectionError{Reason: "account_unavailable", Platform: zernioPlatform})
	case accountselect.PlatformMismatch:
		return "", true, writeAccountSelectionError(c, &schedule.AccountSelectionError{Reason: "account_platform_mismatch", Platform: zernioPlatform})
	case accountselect.NoAccount:
		return "", true, c.Status(fiber.StatusConflict).JSON(fiber.Map{"error": "no_account_connected", "platform": zernioPlatform})
	default: // Ambiguous
		candidates := make([]schedule.AccountCandidate, 0, len(res.Candidates))
		for _, cc := range res.Candidates {
			candidates = append(candidates, schedule.AccountCandidate{ID: cc.ID, Username: cc.Username, DisplayName: cc.DisplayName})
		}
		return "", true, writeAccountSelectionError(c, &schedule.AccountSelectionError{Reason: "account_selection_required", Platform: zernioPlatform, Candidates: candidates})
	}
}
