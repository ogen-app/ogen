package handlers

import (
	"fmt"

	"github.com/gofiber/fiber/v2"

	"github.com/ogen-app/ogen/src/infra/publishers/zernio"
	"github.com/ogen-app/ogen/src/infra/repository"
)

// AutoPublishAllowlistHandler exposes the workspace-scoped auto-publish
// allowlist (CON-65). Any authenticated user can read; any
// authenticated user can replace it. Role-based access (Admin/Owner
// only for writes) is intentionally out of scope for the MVP — when a
// roles model lands, gate PUT behind it.
type AutoPublishAllowlistHandler struct {
	repo repository.AutoPublishAllowlistRepository
	auth fiber.Handler
}

func NewAutoPublishAllowlistHandler(
	repo repository.AutoPublishAllowlistRepository,
	auth fiber.Handler,
) *AutoPublishAllowlistHandler {
	return &AutoPublishAllowlistHandler{repo: repo, auth: auth}
}

func (h *AutoPublishAllowlistHandler) Register(app *fiber.App) {
	g := app.Group("/api/auto-publish-allowlist", h.auth)
	g.Get("/", h.Get)
	g.Put("/", h.Put)
}

type autoPublishAllowlistResponse struct {
	Platforms []string `json:"platforms"`
}

type autoPublishAllowlistRequest struct {
	Platforms []string `json:"platforms"`
}

// Get godoc
// @Summary      Get auto-publish allowlist
// @Description  Returns the set of Zernio platform identifiers Ogen is
// @Description  permitted to auto-publish to. Empty by default.
// @Tags         auto-publish-allowlist
// @Produce      json
// @Security     CookieAuth
// @Success      200  {object}  autoPublishAllowlistResponse
// @Failure      401  {object}  map[string]string
// @Router       /api/auto-publish-allowlist [get]
func (h *AutoPublishAllowlistHandler) Get(c *fiber.Ctx) error {
	platforms, err := h.repo.List(reqCtx(c))
	if err != nil {
		return err
	}
	if platforms == nil {
		platforms = []string{}
	}
	return c.JSON(autoPublishAllowlistResponse{Platforms: platforms})
}

// Put godoc
// @Summary      Replace auto-publish allowlist
// @Description  Replaces the auto-publish allowlist with the supplied
// @Description  set of Zernio platform identifiers. Replace-not-append:
// @Description  any platform absent from the body is removed. An empty
// @Description  array clears the allowlist.
// @Tags         auto-publish-allowlist
// @Accept       json
// @Produce      json
// @Security     CookieAuth
// @Param        body  body      autoPublishAllowlistRequest  true  "Desired allowlist"
// @Success      200   {object}  autoPublishAllowlistResponse
// @Failure      400   {object}  map[string]string  "platform_not_supported"
// @Failure      401   {object}  map[string]string
// @Router       /api/auto-publish-allowlist [put]
func (h *AutoPublishAllowlistHandler) Put(c *fiber.Ctx) error {
	var req autoPublishAllowlistRequest
	if err := c.BodyParser(&req); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, err.Error())
	}
	if req.Platforms == nil {
		req.Platforms = []string{}
	}

	// Validate Z-membership at the handler layer. The repository stays
	// independent of publishers/zernio (which depends on repository),
	// so the import cycle stays broken.
	for _, p := range req.Platforms {
		if zernio.LookupSupportedPlatform(p) == nil {
			return fiber.NewError(
				fiber.StatusBadRequest,
				fmt.Sprintf("platform %q is not in the Ogen-supported set", p),
			)
		}
	}

	if err := h.repo.Set(reqCtx(c), req.Platforms); err != nil {
		return err
	}

	platforms, err := h.repo.List(reqCtx(c))
	if err != nil {
		return err
	}
	if platforms == nil {
		platforms = []string{}
	}
	return c.JSON(autoPublishAllowlistResponse{Platforms: platforms})
}
