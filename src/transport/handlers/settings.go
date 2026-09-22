package handlers

import (
	"github.com/gofiber/fiber/v2"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/usecase/settings"
)

type SettingsHandler struct {
	repo repository.SettingRepository
	auth fiber.Handler
}

func NewSettingsHandler(repo repository.SettingRepository, auth fiber.Handler) *SettingsHandler {
	return &SettingsHandler{repo: repo, auth: auth}
}

func (h *SettingsHandler) Register(app *fiber.App) {
	g := app.Group("/api/settings")
	g.Get("/", h.auth, h.List)
	// CON-97: GET /:key is always authenticated. The old setup_complete bootstrap
	// gate (unauthenticated reads while first-run setup was incomplete) was
	// removed once self-service signup via POST /api/tenants became the sole
	// onboarding path — the UI no longer probes settings before a session exists.
	g.Get("/:key", h.auth, h.Get)
	g.Put("/:key", h.auth, h.Upsert)
	g.Delete("/:key", h.auth, h.Delete)
}

type upsertSettingRequest struct {
	Value string `json:"value" validate:"required"`
}

// List godoc
// @Summary      List settings
// @Description  Returns all key-value settings.
// @Tags         settings
// @Produce      json
// @Security     CookieAuth
// @Success      200  {array}   models.Setting
// @Failure      401  {object}  map[string]string
// @Router       /api/settings [get]
func (h *SettingsHandler) List(c *fiber.Ctx) error {
	settings, err := h.repo.List(reqCtx(c))
	if err != nil {
		return err
	}
	return c.JSON(settings)
}

// Get godoc
// @Summary      Get setting
// @Description  Returns a single setting by key (scoped to the caller's tenant).
// @Tags         settings
// @Produce      json
// @Security     CookieAuth
// @Param        key  path      string  true  "Setting key"
// @Success      200  {object}  models.Setting
// @Failure      401  {object}  map[string]string
// @Failure      404  {object}  map[string]string
// @Router       /api/settings/{key} [get]
func (h *SettingsHandler) Get(c *fiber.Ctx) error {
	setting, err := h.repo.GetByKey(reqCtx(c), c.Params("key"))
	if err != nil {
		return notFound(err, "setting not found")
	}
	return c.JSON(setting)
}

// Upsert godoc
// @Summary      Upsert setting
// @Description  Creates or updates a setting by key.
// @Tags         settings
// @Accept       json
// @Produce      json
// @Security     CookieAuth
// @Param        key   path      string                true  "Setting key"
// @Param        body  body      upsertSettingRequest  true  "Setting value"
// @Success      200   {object}  models.Setting
// @Failure      400   {object}  map[string]string
// @Failure      401   {object}  map[string]string
// @Router       /api/settings/{key} [put]
func (h *SettingsHandler) Upsert(c *fiber.Ctx) error {
	var req upsertSettingRequest
	if err := bindAndValidate(c, &req); err != nil {
		return err
	}

	key := c.Params("key")
	// CON-78: the workspace timezone must be a valid IANA zone name so
	// scheduling can resolve relative times against it. Reject unknown
	// zones up front rather than letting a bad value silently fall back
	// to UTC at schedule time.
	if key == settings.TimezoneKey {
		if _, err := settings.ResolveTimezone(req.Value); err != nil {
			return fiber.NewError(fiber.StatusBadRequest, "invalid timezone: "+req.Value)
		}
	}

	setting := &models.Setting{
		Key:   key,
		Value: req.Value,
	}
	if err := h.repo.Upsert(reqCtx(c), setting); err != nil {
		return err
	}

	return c.JSON(setting)
}

// Delete godoc
// @Summary      Delete setting
// @Description  Deletes a setting by key.
// @Tags         settings
// @Security     CookieAuth
// @Param        key  path  string  true  "Setting key"
// @Success      204
// @Failure      401  {object}  map[string]string
// @Failure      404  {object}  map[string]string
// @Router       /api/settings/{key} [delete]
func (h *SettingsHandler) Delete(c *fiber.Ctx) error {
	deleted, err := h.repo.Delete(reqCtx(c), c.Params("key"))
	if err != nil {
		return err
	}
	if !deleted {
		return fiber.NewError(fiber.StatusNotFound, "setting not found")
	}
	return c.SendStatus(fiber.StatusNoContent)
}
