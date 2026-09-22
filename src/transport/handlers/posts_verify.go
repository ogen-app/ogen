package handlers

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/eventhub"
	"github.com/ogen-app/ogen/src/infra/publishers/zernio"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/jobs"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
	"github.com/ogen-app/ogen/src/usecase/accountselect"
	"github.com/ogen-app/ogen/src/usecase/post_actions/schedule"
)

// PostVerificationHandler owns POST /api/posts/:id/verify-external (CON-153):
// confirm a manually-published post via Zernio's sync-external, then back-fill
// its publisher linkage + a first analytics snapshot. It was split out of the
// PostsHandler god-object (CON-291) — a focused handler with only the deps this
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

	post, err := h.repo.GetByID(reqCtx(c), c.Params("id"))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fiber.NewError(fiber.StatusNotFound, "post not found")
		}
		return err
	}

	profileID := ""
	if h.profileID != nil {
		if profileID, err = h.profileID(reqCtx(c)); err != nil {
			return err
		}
	}
	if profileID == "" {
		return fiber.NewError(fiber.StatusServiceUnavailable, "zernio integration is not configured")
	}

	// Resolve the connected account whose token reads the platform (CON-150).
	supported := zernio.LookupSupportedBySqid(post.PlatformID)
	if supported == nil {
		return fiber.NewError(fiber.StatusConflict, "post has no supported platform")
	}
	zernioPlatform := supported.ZernioID

	accountID, handled, err := h.resolveExternalAccountID(c, post, profileID, zernioPlatform)
	if handled {
		return err
	}

	result, err := h.zernioClient.SyncExternalPost(reqCtx(c), zernio.SyncExternalRequest{
		AccountID: accountID,
		URL:       body.URL,
		PostID:    body.PostID,
	})
	if err != nil {
		// A 404 means the platform has no such post — a normal "not found",
		// not a server error.
		if zernio.IsStatus(err, fiber.StatusNotFound) {
			jobs.ZernioExternalVerifyNotFound.Add(1)
			return c.JSON(fiber.Map{"found": false})
		}
		jobs.ZernioExternalVerifyFailed.Add(1)
		return fiber.NewError(fiber.StatusBadGateway, "sync-external upstream error")
	}
	if !result.Found || result.Post == nil {
		jobs.ZernioExternalVerifyNotFound.Add(1)
		return c.JSON(fiber.Map{"found": false})
	}
	ext := result.Post

	// Back-fill the publisher linkage so per-post analytics resolve, and mark
	// the post published-external.
	if post.PublisherPostID == "" {
		post.PublisherPostID = ext.PlatformPostID
	}
	if post.Publisher == "" {
		post.Publisher = models.PublisherZernio
	}
	if post.PublishedAt == nil {
		if pa := ext.PublishedAtTime(); pa != nil {
			post.PublishedAt = pa
		}
	}
	// CON-165: persist the canonical, platform-normalised permalink. Verification
	// is authoritative, so a confirmed URL overwrites any user-pasted one.
	if ext.PlatformPostURL != "" {
		post.PublishedURL = ext.PlatformPostURL
	}
	post.Status = models.PostStatusPublished
	post.UpdatedAt = time.Now().UTC()
	if err := h.repo.Update(reqCtx(c), post); err != nil {
		jobs.ZernioExternalVerifyFailed.Add(1)
		return err
	}
	// CON-251: the post is now confirmed published — snapshot the content as a
	// durable record of what went out (best-effort, deduped against the head).
	h.snapshotPublished(reqCtx(c), post)

	// Refresh the current-state analytics row (best-effort) and emit the update
	// event so open analytics streams refresh. The response carries the fetched
	// metrics regardless of whether persistence is available. Mirrors the refresh
	// job's dedup discipline (CON-236): preserve first_seen_at, always bump
	// last_checked_at, and append a trend point / publish only on a real change —
	// so re-verifying an already-tracked post doesn't clobber its history.
	metrics := externalMetrics(ext.Analytics)
	if h.analyticsRepo != nil {
		now := time.Now().UTC()
		built := h.buildExternalCurrent(post, ext)
		prev, _ := h.analyticsRepo.GetByPostID(reqCtx(c), post.ID)
		changed := prev == nil || prev.MetricsKey() != built.MetricsKey()
		built.LastCheckedAt = now
		if prev == nil {
			built.FirstSeenAt = now
			built.LastChangedAt = now
		} else {
			built.FirstSeenAt = prev.FirstSeenAt
			if changed {
				built.LastChangedAt = now
			} else {
				built.LastChangedAt = prev.LastChangedAt
			}
		}
		if changed {
			if id, ierr := models.NewID(); ierr == nil {
				if werr := h.analyticsRepo.UpsertWithSnapshot(reqCtx(c), built, built.NewSnapshot(id, now)); werr == nil {
					h.publishAnalyticsUpdated(reqCtx(c), built)
				}
			}
		} else {
			_ = h.analyticsRepo.Upsert(reqCtx(c), built)
		}
	}

	jobs.ZernioExternalVerifySucceeded.Add(1)
	return c.JSON(fiber.Map{
		"found": true,
		"post": fiber.Map{
			"id": post.ID,
			// The confirmed external post's own id — the one ext.Analytics
			// belong to (post.PublisherPostID is left as-is when already set).
			"publisher_post_id": ext.PlatformPostID,
			// CON-165: echo the persisted permalink so the FE can render
			// "View post" straight after verify, without a re-fetch.
			"published_url": post.PublishedURL,
			"sync_status":   "synced",
		},
		"analytics": metrics,
	})
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

// externalMetrics maps a synced external post's analytics onto the shared
// metrics block.
func externalMetrics(a zernio.ExternalPostAnalytics) models.PostAnalyticsMetrics {
	return models.PostAnalyticsMetrics{
		Impressions:    a.Impressions,
		Reach:          a.Reach,
		Likes:          a.Likes,
		Comments:       a.Comments,
		Shares:         a.Shares,
		Saves:          a.Saves,
		Clicks:         a.Clicks,
		Views:          a.Views,
		EngagementRate: a.EngagementRate,
	}
}

// buildExternalCurrent builds the current-state analytics row from a synced
// external post, denormalising the post's display fields exactly like the
// refresh job (CON-236). The first_seen/last_changed/last_checked timestamps are
// stamped by the caller (they depend on the dedup comparison against any
// existing current row).
func (h *PostVerificationHandler) buildExternalCurrent(post *models.Post, ext *zernio.ExternalPost) *models.PostAnalytics {
	m := externalMetrics(ext.Analytics)
	platformName := ext.Platform
	if post.Platform != nil && post.Platform.Name != "" {
		platformName = post.Platform.Name
	}
	return &models.PostAnalytics{
		PostID: post.ID,
		// The synced external post's id — kept consistent with the metrics
		// below (ext.Analytics), not the post's possibly-preexisting linkage.
		PublisherPostID: ext.PlatformPostID,
		Publisher:       models.PublisherZernio,
		Platform:        platformName,
		Title:           post.Title,
		PublishedAt:     post.PublishedAt,
		Impressions:     m.Impressions,
		Reach:           m.Reach,
		Likes:           m.Likes,
		Comments:        m.Comments,
		Shares:          m.Shares,
		Saves:           m.Saves,
		Clicks:          m.Clicks,
		Views:           m.Views,
		EngagementRate:  m.EngagementRate,
		PlatformAnalytics: models.PlatformAnalyticsList{{
			Platform:        ext.Platform,
			PlatformPostID:  ext.PlatformPostID,
			PlatformPostURL: ext.PlatformPostURL,
			SyncStatus:      "synced",
			Analytics:       m,
		}},
		SyncStatus:         "synced",
		MetricsLastUpdated: ext.Analytics.LastUpdatedTime(),
	}
}

// publishAnalyticsUpdated emits the post.analytics.updated event (CON-93 §8)
// after a verify-external snapshot. No-op when no Hub is wired.
func (h *PostVerificationHandler) publishAnalyticsUpdated(ctx context.Context, a *models.PostAnalytics) {
	if h.analyticsHub == nil {
		return
	}
	tid, _ := tenantctx.From(ctx)
	_ = h.analyticsHub.Publish(ctx, eventhub.Event{
		Topic:    fmt.Sprintf("entity:post:%s", a.PostID),
		TenantID: tid,
		Type:     "post.analytics.updated",
		Payload: map[string]any{
			"post_id":     a.PostID,
			"sync_status": a.SyncStatus,
			"analytics":   a.Metrics(),
		},
	})
}

// snapshotPublished records a system-authored version of a post's content at
// the moment it is confirmed published (CON-251), so "what actually went out"
// becomes a durable record rather than an assumption. Deduped only against a
// prior "Published" system snapshot of identical content, so re-verifying an
// already-published post adds nothing — but a matching-content user/assistant
// edit (or the schedule-time "Submitted" snapshot) at the head does NOT
// suppress it, so the publish is always recorded once. Best-effort: a nil repo
// or any error is swallowed — the publish has already committed, mirroring the
// surrounding analytics writes.
func (h *PostVerificationHandler) snapshotPublished(ctx context.Context, post *models.Post) {
	if h.versionRepo == nil {
		return
	}
	latest, err := h.versionRepo.GetLatestByPostID(ctx, post.ID)
	if err != nil {
		return
	}
	// CON-284 R2: content is the canonical full thread body, so SnapshotContent is
	// simply post.Content for every post type.
	content := post.SnapshotContent()
	if latest != nil && latest.IsSystemSnapshot() &&
		latest.Note == models.PostVersionNotePublished && latest.Content == content {
		return
	}
	nextNum := 1
	if latest != nil {
		nextNum = latest.VersionNumber + 1
	}
	id, err := models.NewID()
	if err != nil {
		return
	}
	_ = h.versionRepo.Create(ctx, &models.PostVersion{
		ID:            id,
		PostID:        post.ID,
		VersionNumber: nextNum,
		Content:       content,
		Note:          models.PostVersionNotePublished,
		Creator:       models.PostVersionCreatorSystem,
	})
}
