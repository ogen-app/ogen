package handlers

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/domain/platforms"
	"github.com/ogen-app/ogen/src/infra/publishers"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/logging"
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
// allowlist is the auto-publish allowlist. It may be nil in
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
	g.Get("/:id", h.auth, h.Get)
	g.Get("/:id/post-type-rules", h.auth, h.PostTypeRules)
	// Platform lifecycle (create/update/delete + limits) is operator-only
	// via Harbor → PlatformAdminService gRPC. The tenant surface is read-only;
	// the former POST/PUT/DELETE handlers were retired (nothing in the ui repo
	// called them).
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
	// The composer-facing list shows only enabled platforms, ordered by
	// sort_order. Disabled platforms drop out of the catalog (soft-disable) while
	// their already-scheduled posts still publish via the resolver.
	platforms, err := h.repo.ListEnabled(reqCtx(c))
	if err != nil {
		return err
	}
	views, err := h.collectPublisherViews(reqCtx(c), platforms)
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
	platform, err := h.repo.GetByID(reqCtx(c), c.Params("id"))
	if err != nil {
		return notFound(err, "platform not found")
	}
	// Soft-disable: a disabled platform is invisible to tenants, the
	// same as a missing one — the composer lists enabled platforms only, and the
	// detail routes match so a disabled id can't be probed.
	if !platform.Enabled {
		return fiber.NewError(fiber.StatusNotFound, "platform not found")
	}
	views, err := h.collectPublisherViews(reqCtx(c), []models.Platform{*platform})
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
	platform, err := h.repo.GetByID(reqCtx(c), c.Params("id"))
	if err != nil {
		return notFound(err, "platform not found")
	}
	// Soft-disable: hide disabled platforms from tenants, as Get does.
	if !platform.Enabled {
		return fiber.NewError(fiber.StatusNotFound, "platform not found")
	}
	return c.JSON(platforms.ResolvePostTypeRules(platform))
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

	idx := newPlatformIndex(platforms)
	allowSet := h.autoPublishAllowSet(ctx)
	for _, pub := range h.publishers {
		views, err := pub.PlatformViews(ctx)
		if err != nil {
			return nil, err
		}
		for _, v := range views {
			matchID, ok := idx.match(v)
			if !ok {
				continue
			}
			_, autoPublish := allowSet[v.PublisherPlatformID]
			accounts := toAccountViews(v.Accounts)
			out[matchID] = append(out[matchID], publisherView{
				ID:                 pub.ID(),
				Name:               pub.Name(),
				State:              pub.State(),
				Connected:          len(accounts) > 0,
				AutoPublishAllowed: v.PublisherPlatformID != "" && autoPublish,
				SupportedPostTypes: append([]string(nil), v.SupportedPostTypes...),
				Accounts:           accounts,
			})
		}
	}
	return out, nil
}

// platformIndex resolves a publisher view to a local platform row.
type platformIndex struct {
	ids    map[string]struct{} // platform.ID
	byName map[string]string   // lower(platform.Name) → platform.ID
}

func newPlatformIndex(platforms []models.Platform) platformIndex {
	idx := platformIndex{
		ids:    make(map[string]struct{}, len(platforms)),
		byName: make(map[string]string, len(platforms)),
	}
	for _, p := range platforms {
		idx.ids[p.ID] = struct{}{}
		idx.byName[strings.ToLower(p.Name)] = p.ID
	}
	return idx
}

// match joins v by OgenPlatformID first, then case-insensitively by name.
func (idx platformIndex) match(v publishers.PlatformView) (string, bool) {
	if v.OgenPlatformID != "" {
		if _, ok := idx.ids[v.OgenPlatformID]; ok {
			return v.OgenPlatformID, true
		}
	}
	if v.PlatformName != "" {
		if id, ok := idx.byName[strings.ToLower(v.PlatformName)]; ok {
			return id, true
		}
	}
	return "", false
}

// autoPublishAllowSet loads the auto-publish allowlist keyed by the
// publisher's wire ID. A nil repo or a query error degrades to "nothing
// allowlisted" rather than failing the read — auto_publish is a
// forward-looking permission, not a precondition for listing platforms — but
// the error is logged so an outage is visible.
func (h *PlatformsHandler) autoPublishAllowSet(ctx context.Context) map[string]struct{} {
	set := map[string]struct{}{}
	if h.allowlist == nil {
		return set
	}
	ids, err := h.allowlist.List(ctx)
	if err != nil {
		slog.WarnContext(ctx, "auto-publish allowlist unavailable; treating as empty",
			logging.AttrComponent, "platforms", logging.AttrError, err)
		return set
	}
	for _, id := range ids {
		set[id] = struct{}{}
	}
	return set
}

// toAccountViews projects publisher accounts onto the wire shape; the result
// is never nil.
func toAccountViews(in []publishers.Account) []accountView {
	out := make([]accountView, 0, len(in))
	for _, a := range in {
		out = append(out, accountView{
			ID:          a.ID,
			Username:    a.Username,
			DisplayName: a.DisplayName,
			AvatarURL:   a.AvatarURL,
			IsActive:    a.IsActive,
			ConnectedAt: a.ConnectedAt,
		})
	}
	return out
}
