package handlers

import (
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
)

// AnnouncementsHandler serves the tenant-facing announcements surface (CON-230):
// the read-only banner feed plus per-user click / dismiss tracking. Operators
// author announcements over the internal gRPC surface (Harbor); this handler
// only delivers the currently-active ones and records engagement.
//
// Announcements are a GLOBAL table (not tenant-scoped), so delivery resolves the
// caller's active workspace to its tier + groups (via the tenant classification
// read) and filters targeting explicitly — the app-level tenant hook does not
// apply.
type AnnouncementsHandler struct {
	repo    repository.AnnouncementRepository
	tenants repository.TenantRepository
	auth    fiber.Handler
}

// NewAnnouncementsHandler wires the tenant announcements endpoints.
func NewAnnouncementsHandler(repo repository.AnnouncementRepository, tenants repository.TenantRepository, auth fiber.Handler) *AnnouncementsHandler {
	return &AnnouncementsHandler{repo: repo, tenants: tenants, auth: auth}
}

func (h *AnnouncementsHandler) Register(app *fiber.App) {
	g := app.Group("/api/announcements", h.auth)
	g.Get("/", h.List)
	g.Post("/:id/click", h.Click)
	g.Post("/:id/dismiss", h.Dismiss)
}

func (h *AnnouncementsHandler) session(c *fiber.Ctx) (*models.Session, error) {
	s, ok := c.Locals("session").(*models.Session)
	if !ok || s == nil {
		return nil, fiber.NewError(fiber.StatusUnauthorized, "authentication required")
	}
	return s, nil
}

// audience resolves the caller's active workspace to the targeting inputs
// (tier + group ids) the repository uses to match — and authorize —
// announcements. Shared by delivery and the click/dismiss gate so a user can
// only interact with an announcement actually targeted at their workspace.
func (h *AnnouncementsHandler) audience(c *fiber.Ctx, s *models.Session) (repository.AnnouncementAudience, error) {
	tenant, err := h.tenants.GetByIDWithClassification(reqCtx(c), s.TenantID)
	if err != nil {
		return repository.AnnouncementAudience{}, err
	}
	groupIDs := make([]string, len(tenant.Groups))
	for i := range tenant.Groups {
		groupIDs[i] = tenant.Groups[i].ID
	}
	return repository.AnnouncementAudience{
		TenantID: s.TenantID,
		TierID:   tenant.TierID,
		GroupIDs: groupIDs,
		UserID:   s.UserID,
	}, nil
}

// announcementDTO is the tenant-facing projection: only what the banner renders,
// never the operator fields (status, targeting, draft window start, timestamps).
type announcementDTO struct {
	ID          string     `json:"id"`
	Title       string     `json:"title"`
	Body        string     `json:"body"`
	ImageURL    string     `json:"image_url,omitempty"`
	ImageAlt    string     `json:"image_alt,omitempty"`
	CTALabel    string     `json:"cta_label,omitempty"`
	CTAURL      string     `json:"cta_url,omitempty"`
	PublishedAt *time.Time `json:"published_at,omitempty"`
	EndsAt      *time.Time `json:"ends_at,omitempty"`
	Clicked     bool       `json:"clicked"`
}

func toAnnouncementDTO(a repository.AnnouncementForUser) announcementDTO {
	return announcementDTO{
		ID:          a.ID,
		Title:       a.Title,
		Body:        a.Body,
		ImageURL:    a.ImageURL,
		ImageAlt:    a.ImageAlt,
		CTALabel:    a.CTALabel,
		CTAURL:      a.CTAURL,
		PublishedAt: a.PublishedAt,
		EndsAt:      a.EndsAt,
		Clicked:     a.Clicked,
	}
}

// List godoc
// @Summary  List active announcements for the caller's workspace
// @Tags     announcements
// @Produce  json
// @Security CookieAuth
// @Success  200 {array} handlers.announcementDTO
// @Router   /api/announcements [get]
func (h *AnnouncementsHandler) List(c *fiber.Ctx) error {
	s, err := h.session(c)
	if err != nil {
		return err
	}
	aud, err := h.audience(c, s)
	if err != nil {
		return err
	}
	rows, err := h.repo.ActiveForTenant(reqCtx(c), aud)
	if err != nil {
		return err
	}
	out := make([]announcementDTO, 0, len(rows))
	for i := range rows {
		out = append(out, toAnnouncementDTO(rows[i]))
	}
	return c.JSON(out)
}

// Click godoc
// @Summary  Record a CTA click on an announcement
// @Tags     announcements
// @Security CookieAuth
// @Param    id path string true "Announcement id"
// @Success  204
// @Failure  404 {object} map[string]string
// @Router   /api/announcements/{id}/click [post]
func (h *AnnouncementsHandler) Click(c *fiber.Ctx) error {
	s, err := h.session(c)
	if err != nil {
		return err
	}
	aud, err := h.audience(c, s)
	if err != nil {
		return err
	}
	ok, err := h.repo.RecordClick(reqCtx(c), c.Params("id"), aud)
	if err != nil {
		return err
	}
	if !ok {
		return fiber.NewError(fiber.StatusNotFound, "announcement not found")
	}
	return c.SendStatus(fiber.StatusNoContent)
}

// Dismiss godoc
// @Summary  Dismiss (hide) an announcement for the caller
// @Tags     announcements
// @Security CookieAuth
// @Param    id path string true "Announcement id"
// @Success  204
// @Failure  404 {object} map[string]string
// @Router   /api/announcements/{id}/dismiss [post]
func (h *AnnouncementsHandler) Dismiss(c *fiber.Ctx) error {
	s, err := h.session(c)
	if err != nil {
		return err
	}
	aud, err := h.audience(c, s)
	if err != nil {
		return err
	}
	ok, err := h.repo.RecordDismiss(reqCtx(c), c.Params("id"), aud)
	if err != nil {
		return err
	}
	if !ok {
		return fiber.NewError(fiber.StatusNotFound, "announcement not found")
	}
	return c.SendStatus(fiber.StatusNoContent)
}
