package handlers

import (
	"errors"

	"github.com/gofiber/fiber/v2"
	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/infra/secrets"
)

type HealthHandler struct {
	db    *bun.DB
	store secrets.Store
}

func NewHealthHandler(db *bun.DB, store secrets.Store) *HealthHandler {
	return &HealthHandler{db: db, store: store}
}

func (h *HealthHandler) Register(app *fiber.App) {
	app.Get("/api/health", h.Health)
}

// secretStatus is the per-secret entry in the health response. Holds
// presence + decryptability only — never any value derived from the
// key material.
type secretStatus struct {
	Present     bool `json:"present"`
	Decryptable bool `json:"decryptable"`
}

// Health godoc
// @Summary      Health check
// @Description  Returns the health status of the service, database
// @Description  connectivity, and per-secret resolvability. A secret
// @Description  that is present but undecryptable downgrades the
// @Description  overall status to "degraded" without failing the
// @Description  endpoint — boot stayed up under the same rule.
// @Tags         system
// @Produce      json
// @Success      200  {object}  map[string]any
// @Failure      503  {object}  map[string]string
// @Router       /api/health [get]
func (h *HealthHandler) Health(c *fiber.Ctx) error {
	if err := h.db.PingContext(reqCtx(c)); err != nil {
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{
			"status": "unhealthy",
			"error":  err.Error(),
		})
	}

	status := "ok"
	out := fiber.Map{}
	if h.store != nil {
		secretsOut := map[string]secretStatus{}
		for _, name := range secrets.AllowedNames {
			meta, err := h.store.GetMetadata(reqCtx(c), name)
			switch {
			case errors.Is(err, secrets.ErrNotFound):
				secretsOut[name] = secretStatus{Present: false, Decryptable: false}
			case err != nil:
				secretsOut[name] = secretStatus{Present: true, Decryptable: false}
				status = "degraded"
			default:
				secretsOut[name] = secretStatus{Present: true, Decryptable: meta.Decryptable}
				if !meta.Decryptable {
					status = "degraded"
				}
			}
		}
		out["secrets"] = secretsOut
	}
	out["status"] = status
	return c.JSON(out)
}
