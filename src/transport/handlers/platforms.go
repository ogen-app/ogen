package handlers

import (
	"context"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/domain/platforms"
	"github.com/ogen-app/ogen/src/infra/publishers"
	"github.com/ogen-app/ogen/src/infra/repository"
)

type PlatformsHandler struct {
	repo       repository.PlatformRepository
	publishers []publishers.Publisher
	allowlist  repository.AutoPublishAllowlistRepository
	auth       fiber.Handler
}

// NewPlatformsHandler wires the handler. The publishers slice may be
// empty when no integration is configured — in that case List/Get
// emit `"publishers": []` per platform so clients see a stable shape.
//
// allowlist is the auto-publish allowlist (CON-65). It may be nil in
// callers that don't care to surface auto_publish_allowed (legacy
// tests); when nil, the field falls back to false on every view.
func NewPlatformsHandler(
	repo repository.PlatformRepository,
	pubs []publishers.Publisher,
	allowlist repository.AutoPublishAllowlistRepository,
	auth fiber.Handler,
) *PlatformsHandler {
	return &PlatformsHandler{repo: repo, publishers: pubs, allowlist: allowlist, auth: auth}
}

func (h *PlatformsHandler) Register(app *fiber.App) {
	g := app.Group("/api/platforms")
	g.Get("/", h.auth, h.List)
	g.Post("/", h.auth, h.Create)
	g.Get("/:id", h.auth, h.Get)
	g.Get("/:id/post-type-rules", h.auth, h.PostTypeRules)
	g.Put("/:id", h.auth, h.Update)
	g.Delete("/:id", h.auth, h.Delete)
}

type platformRequest struct {
	Name        string             `json:"name"        validate:"required"`
	PostTypes   models.PostTypeMap `json:"post_types"`
	Cadence     string             `json:"cadence"`
	Constraints string             `json:"constraints"`
}

// publisherView is the per-publisher entry attached to each platform
// in List / Get responses. snake_case to match the surrounding shape.
type publisherView struct {
	ID                 string        `json:"id"`
	Name               string        `json:"name"`
	State              string        `json:"state"`
	Connected          bool          `json:"connected"`
	AutoPublishAllowed bool          `json:"auto_publish_allowed"`
	SupportedPostTypes []string      `json:"supported_post_types"`
	Accounts           []accountView `json:"accounts"`
}

// accountView is the minimal account projection — full detail lives
// at the publisher's own /accounts endpoint.
type accountView struct {
	ID          string    `json:"id"`
	Username    string    `json:"username"`
	DisplayName string    `json:"display_name"`
	AvatarURL   string    `json:"avatar_url"`
	IsActive    bool      `json:"is_active"`
	ConnectedAt time.Time `json:"connected_at"`
}

// platformResponse is the wire shape returned by List / Get. Embedding
// *models.Platform keeps every existing field at the top level so the
// response is fully backward-compatible — `publishers` is purely
// additive.
type platformResponse struct {
	*models.Platform
	Publishers []publisherView `json:"publishers"`
}

// List godoc
// @Summary      List platforms (with publishers)
// @Description  Returns all platforms ordered by creation date. Each
// @Description  platform carries a `publishers` array describing
// @Description  whether it's connected via any of the configured
// @Description  publisher backends (e.g. Zernio) and which post
// @Description  types each publisher can post in.
// @Tags         platforms
// @Produce      json
// @Security     CookieAuth
// @Success      200  {array}   platformResponse
// @Failure      401  {object}  map[string]string
// @Router       /api/platforms [get]
func (h *PlatformsHandler) List(c *fiber.Ctx) error {
	// CON-292: the composer-facing list shows only enabled platforms, ordered by
	// sort_order. Disabled platforms drop out of the catalog (soft-disable) while
	// their already-scheduled posts still publish via the resolver.
	platforms, err := h.repo.ListEnabled(c.Context())
	if err != nil {
		return err
	}
	views, err := h.collectPublisherViews(c.Context(), platforms)
	if err != nil {
		return err
	}
	out := make([]platformResponse, 0, len(platforms))
	for i := range platforms {
		out = append(out, platformResponse{
			Platform:   &platforms[i],
			Publishers: ensurePublishersSlice(views[platforms[i].ID]),
		})
	}
	return c.JSON(out)
}

// Create godoc
// @Summary      Create platform
// @Description  Creates a new platform.
// @Tags         platforms
// @Accept       json
// @Produce      json
// @Security     CookieAuth
// @Param        body  body      platformRequest  true  "Platform payload"
// @Success      201   {object}  models.Platform
// @Failure      400   {object}  map[string]string
// @Failure      401   {object}  map[string]string
// @Router       /api/platforms [post]
func (h *PlatformsHandler) Create(c *fiber.Ctx) error {
	var req platformRequest
	if err := bindAndValidate(c, &req); err != nil {
		return err
	}

	id, err := models.NewID()
	if err != nil {
		return err
	}

	platform := &models.Platform{
		ID:          id,
		Name:        req.Name,
		PostTypes:   nullMap(req.PostTypes),
		Cadence:     req.Cadence,
		Constraints: req.Constraints,
	}
	if err := h.repo.Create(c.Context(), platform); err != nil {
		return err
	}
	return c.Status(fiber.StatusCreated).JSON(platform)
}

// Get godoc
// @Summary      Get platform (with publishers)
// @Description  Returns a single platform with the same `publishers`
// @Description  enrichment as the list endpoint.
// @Tags         platforms
// @Produce      json
// @Security     CookieAuth
// @Param        id   path      string  true  "Platform Sqid"
// @Success      200  {object}  platformResponse
// @Failure      401  {object}  map[string]string
// @Failure      404  {object}  map[string]string
// @Router       /api/platforms/{id} [get]
func (h *PlatformsHandler) Get(c *fiber.Ctx) error {
	platform, err := h.repo.GetByID(c.Context(), c.Params("id"))
	if err != nil {
		return notFound(err, "platform not found")
	}
	views, err := h.collectPublisherViews(c.Context(), []models.Platform{*platform})
	if err != nil {
		return err
	}
	return c.JSON(platformResponse{
		Platform:   platform,
		Publishers: ensurePublishersSlice(views[platform.ID]),
	})
}

// ensurePublishersSlice replaces a nil slice with [] so JSON
// marshalling emits an empty array rather than `null`. Clients can
// then assume `publishers` is always iterable.
func ensurePublishersSlice(s []publisherView) []publisherView {
	if s == nil {
		return []publisherView{}
	}
	return s
}

// Update godoc
// @Summary      Update platform
// @Description  Replaces all mutable fields of an existing platform.
// @Tags         platforms
// @Accept       json
// @Produce      json
// @Security     CookieAuth
// @Param        id    path      string          true  "Platform Sqid"
// @Param        body  body      platformRequest true  "Platform payload"
// @Success      200   {object}  models.Platform
// @Failure      400   {object}  map[string]string
// @Failure      401   {object}  map[string]string
// @Failure      404   {object}  map[string]string
// @Router       /api/platforms/{id} [put]
func (h *PlatformsHandler) Update(c *fiber.Ctx) error {
	var req platformRequest
	if err := bindAndValidate(c, &req); err != nil {
		return err
	}

	platform, err := h.repo.GetByID(c.Context(), c.Params("id"))
	if err != nil {
		return notFound(err, "platform not found")
	}

	platform.Name = req.Name
	platform.PostTypes = nullMap(req.PostTypes)
	platform.Cadence = req.Cadence
	platform.Constraints = req.Constraints
	platform.UpdatedAt = time.Now().UTC()

	if err := h.repo.Update(c.Context(), platform); err != nil {
		return err
	}
	return c.JSON(platform)
}

// PostTypeRules godoc
// @Summary      List per-post-type rules for a platform
// @Description  Returns each post-type slug supported by the platform
// @Description  with the structural rules enforced by the publish gate
// @Description  (CON-74) — required content, allowed attachment kinds,
// @Description  and min/max attachment counts. `max_attachments` is
// @Description  resolved against the platform's per-kind cap; `null`
// @Description  means unbounded. Slugs that are whitelist-only carry
// @Description  `rule: null` and `whitelist_only: true`.
// @Tags         platforms
// @Produce      json
// @Security     CookieAuth
// @Param        id   path      string  true  "Platform Sqid"
// @Success      200  {array}   platforms.PostTypeRuleView
// @Failure      401  {object}  map[string]string
// @Failure      404  {object}  map[string]string
// @Router       /api/platforms/{id}/post-type-rules [get]
func (h *PlatformsHandler) PostTypeRules(c *fiber.Ctx) error {
	platform, err := h.repo.GetByID(c.Context(), c.Params("id"))
	if err != nil {
		return notFound(err, "platform not found")
	}
	return c.JSON(platforms.ResolvePostTypeRules(platform))
}

// Delete godoc
// @Summary      Delete platform
// @Description  Deletes a platform by Sqid.
// @Tags         platforms
// @Security     CookieAuth
// @Param        id   path  string  true  "Platform Sqid"
// @Success      204
// @Failure      401  {object}  map[string]string
// @Failure      404  {object}  map[string]string
// @Router       /api/platforms/{id} [delete]
func (h *PlatformsHandler) Delete(c *fiber.Ctx) error {
	deleted, err := h.repo.Delete(c.Context(), c.Params("id"))
	if err != nil {
		return err
	}
	if !deleted {
		return fiber.NewError(fiber.StatusNotFound, "platform not found")
	}
	return c.SendStatus(fiber.StatusNoContent)
}

// collectPublisherViews queries each registered publisher exactly once
// and groups the result by local platform.ID. The returned map's
// values are never nil — callers can drop them straight into the
// response without nil-checks (Go's encoding/json marshals a nil
// slice as `null`, which we want to avoid).
//
// Match strategy: each PlatformView is joined to a local platform
// row by OgenPlatformID first; if that doesn't match any row's ID,
// fall back to a case-insensitive match against the platform's
// Name. The fallback is what makes the enrichment work in
// deployments where the platforms table holds Sqid IDs (the
// well-known seeded "linkedin"/"instagram"/... IDs only survive on
// untouched DBs). Views that don't match any row are dropped.
func (h *PlatformsHandler) collectPublisherViews(ctx context.Context, platforms []models.Platform) (map[string][]publisherView, error) {
	out := map[string][]publisherView{}
	if len(h.publishers) == 0 || len(platforms) == 0 {
		return out, nil
	}

	idIndex := make(map[string]string, len(platforms))   // platform.ID → platform.ID (identity, for fast contains)
	nameIndex := make(map[string]string, len(platforms)) // lower(platform.Name) → platform.ID
	for _, p := range platforms {
		idIndex[p.ID] = p.ID
		nameIndex[strings.ToLower(p.Name)] = p.ID
	}

	// Pull the auto-publish allowlist once per request and turn it
	// into a set keyed by the publisher's wire ID. nil repo (legacy
	// callers) and a transient query error both degrade to "nothing
	// allowlisted" rather than failing the request — auto_publish is
	// a forward-looking permission, not a precondition for reading
	// the platform list.
	allowSet := map[string]struct{}{}
	if h.allowlist != nil {
		ids, err := h.allowlist.List(ctx)
		if err == nil {
			for _, id := range ids {
				allowSet[id] = struct{}{}
			}
		}
	}

	for _, pub := range h.publishers {
		views, err := pub.PlatformViews(ctx)
		if err != nil {
			return nil, err
		}
		for _, v := range views {
			matchID := ""
			if v.OgenPlatformID != "" {
				if id, ok := idIndex[v.OgenPlatformID]; ok {
					matchID = id
				}
			}
			if matchID == "" && v.PlatformName != "" {
				if id, ok := nameIndex[strings.ToLower(v.PlatformName)]; ok {
					matchID = id
				}
			}
			if matchID == "" {
				continue
			}

			accounts := make([]accountView, 0, len(v.Accounts))
			for _, a := range v.Accounts {
				accounts = append(accounts, accountView{
					ID:          a.ID,
					Username:    a.Username,
					DisplayName: a.DisplayName,
					AvatarURL:   a.AvatarURL,
					IsActive:    a.IsActive,
					ConnectedAt: a.ConnectedAt,
				})
			}
			autoPublish := false
			if v.PublisherPlatformID != "" {
				_, autoPublish = allowSet[v.PublisherPlatformID]
			}
			out[matchID] = append(out[matchID], publisherView{
				ID:                 pub.ID(),
				Name:               pub.Name(),
				State:              pub.State(),
				Connected:          len(accounts) > 0,
				AutoPublishAllowed: autoPublish,
				SupportedPostTypes: append([]string(nil), v.SupportedPostTypes...),
				Accounts:           accounts,
			})
		}
	}
	return out, nil
}
