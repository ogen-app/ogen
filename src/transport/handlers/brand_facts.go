package handlers

import (
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/gofiber/fiber/v2"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/activity"
)

// Facts ledger (CON-316): per-row CRUD under /api/brand/facts, and the
// guardrails stance under /api/brand/guardrails/stance. Facts also reach the
// aggregate GET /api/brand as `facts`, and guardrails.facts is their projection.

const maxFactSourceChars = 500

// brandFactRequest is the POST/PUT body. Dates are "YYYY-MM-DD", null or "".
// POST defaults an omitted subject to us, kind to documented and addedAt to
// today (UTC). PUT is a full replace of these fields: an omitted date clears it.
type brandFactRequest struct {
	Statement string  `json:"statement"`
	Subject   string  `json:"subject"   enums:"us,problem,opportunity"`
	Kind      string  `json:"kind"      enums:"measured,documented,commitment,judgement"`
	Source    string  `json:"source"`
	AddedAt   *string `json:"addedAt"   example:"2026-09-01"`
	CheckedAt *string `json:"checkedAt" example:"2026-09-20"`
	ExpiresAt *string `json:"expiresAt" example:"2027-01-31"`
}

// guardrailsStanceRequest is the PUT …/guardrails/stance body.
type guardrailsStanceRequest struct {
	None *bool `json:"none"`
}

func sessionUserID(c *fiber.Ctx) *string {
	s, ok := c.Locals("session").(*models.Session)
	if !ok || s == nil || s.UserID == "" {
		return nil
	}
	id := s.UserID
	return &id
}

func factError(err error) error {
	switch {
	case errors.Is(err, repository.ErrFactDuplicate):
		return fiber.NewError(fiber.StatusConflict, repository.ErrFactDuplicate.Error())
	case errors.Is(err, repository.ErrFactLimit):
		return unprocessable("at most %d facts per workspace", repository.MaxBrandFacts)
	case errors.Is(err, sql.ErrNoRows):
		return fiber.NewError(fiber.StatusNotFound, "fact not found")
	default:
		return err
	}
}

// parseFactDate reads an optional date field: nil and "" mean no date.
func parseFactDate(field string, raw *string) (*models.CalendarDate, error) {
	if raw == nil || strings.TrimSpace(*raw) == "" {
		return nil, nil
	}
	d, err := models.ParseCalendarDate(strings.TrimSpace(*raw))
	if err != nil {
		return nil, unprocessable("invalid %s: %q (want YYYY-MM-DD)", field, *raw)
	}
	return &d, nil
}

// factFromRequest validates the body into the editable fields of a fact.
func factFromRequest(req *brandFactRequest) (*models.BrandFact, error) {
	f := &models.BrandFact{
		Statement: strings.TrimSpace(req.Statement),
		Subject:   models.FactSubject(req.Subject),
		Kind:      models.FactKind(req.Kind),
		Source:    strings.TrimSpace(req.Source),
	}
	if f.Statement == "" {
		return nil, unprocessable("statement is required")
	}
	if len(f.Statement) > maxGuardrailStmtBytes {
		return nil, unprocessable("statement exceeds %d KB", maxGuardrailStmtBytes>>10)
	}
	if !f.Subject.Valid() {
		return nil, unprocessable("invalid subject: %q", req.Subject)
	}
	if !f.Kind.Valid() {
		return nil, unprocessable("invalid kind: %q", req.Kind)
	}
	if utf8.RuneCountInString(f.Source) > maxFactSourceChars {
		return nil, unprocessable("source exceeds %d characters", maxFactSourceChars)
	}
	var err error
	if f.AddedAt, err = parseFactDate("addedAt", req.AddedAt); err != nil {
		return nil, err
	}
	if f.CheckedAt, err = parseFactDate("checkedAt", req.CheckedAt); err != nil {
		return nil, err
	}
	if f.ExpiresAt, err = parseFactDate("expiresAt", req.ExpiresAt); err != nil {
		return nil, err
	}
	return f, nil
}

func decodeFactRequest(c *fiber.Ctx) (*brandFactRequest, error) {
	var req brandFactRequest
	if err := json.Unmarshal(c.Body(), &req); err != nil {
		return nil, fiber.NewError(fiber.StatusBadRequest, err.Error())
	}
	return &req, nil
}

// CreateFact godoc
// @Summary  Add a fact to the ledger
// @Tags     brand
// @Accept   json
// @Produce  json
// @Security CookieAuth
// @Param    body body brandFactRequest true "Fact"
// @Success  201 {object} models.BrandFact
// @Failure  400 {object} map[string]string
// @Failure  409 {object} map[string]string "a fact with this statement already exists"
// @Failure  422 {object} map[string]string
// @Router   /api/brand/facts [post]
func (h *BrandHandler) CreateFact(c *fiber.Ctx) error {
	req, err := decodeFactRequest(c)
	if err != nil {
		return err
	}
	now := brandNow()
	if req.Subject == "" {
		req.Subject = string(models.FactSubjectUs)
	}
	if req.Kind == "" {
		req.Kind = string(models.FactKindDocumented)
	}
	if req.AddedAt == nil {
		today := models.CalendarDateOf(now).String()
		req.AddedAt = &today
	}
	f, err := factFromRequest(req)
	if err != nil {
		return err
	}
	if f.ID, err = models.NewID(); err != nil {
		return err
	}
	f.CreatedBy = sessionUserID(c)
	f.CreatedAt, f.UpdatedAt = now, now
	if err := h.repo.CreateFact(reqCtx(c), f); err != nil {
		return factError(err)
	}
	h.recordActivity(c, "brand_fact_created",
		activity.WithEntity("brand_fact", f.ID),
		activity.WithPayload(map[string]any{
			"subject":    f.Subject,
			"kind":       f.Kind,
			"has_expiry": f.ExpiresAt != nil,
		}))
	return c.Status(fiber.StatusCreated).JSON(f)
}

// UpdateFact godoc
// @Summary  Replace a fact's editable fields
// @Description Full replace of statement, subject, kind, source and the three dates. The author never changes; an unchanged save keeps updatedAt.
// @Tags     brand
// @Accept   json
// @Produce  json
// @Security CookieAuth
// @Param    id   path string           true "Fact id"
// @Param    body body brandFactRequest true "Fact"
// @Success  200 {object} models.BrandFact
// @Failure  400 {object} map[string]string
// @Failure  404 {object} map[string]string
// @Failure  409 {object} map[string]string "a fact with this statement already exists"
// @Failure  422 {object} map[string]string
// @Router   /api/brand/facts/{id} [put]
func (h *BrandHandler) UpdateFact(c *fiber.Ctx) error {
	req, err := decodeFactRequest(c)
	if err != nil {
		return err
	}
	f, err := factFromRequest(req)
	if err != nil {
		return err
	}
	f.ID = c.Params("id")
	f.UpdatedAt = brandNow()
	before, err := h.repo.UpdateFact(reqCtx(c), f)
	if err != nil {
		return factError(err)
	}
	if changed := changedFactFields(before, f); len(changed) > 0 {
		h.recordActivity(c, "brand_fact_updated",
			activity.WithEntity("brand_fact", f.ID),
			activity.WithPayload(map[string]any{
				"fields":    changed,
				"rechecked": rechecked(before.CheckedAt, f.CheckedAt),
			}))
	}
	return c.JSON(f)
}

// DeleteFact godoc
// @Summary  Remove a fact from the ledger
// @Tags     brand
// @Security CookieAuth
// @Param    id path string true "Fact id"
// @Success  204
// @Failure  404 {object} map[string]string
// @Router   /api/brand/facts/{id} [delete]
func (h *BrandHandler) DeleteFact(c *fiber.Ctx) error {
	f, err := h.repo.DeleteFact(reqCtx(c), c.Params("id"))
	if err != nil {
		return err
	}
	if f == nil {
		return fiber.NewError(fiber.StatusNotFound, "fact not found")
	}
	h.recordActivity(c, "brand_fact_deleted",
		activity.WithEntity("brand_fact", f.ID),
		activity.WithPayload(map[string]any{"subject": f.Subject}))
	return c.SendStatus(fiber.StatusNoContent)
}

// PutGuardrailsStance godoc
// @Summary  Record whether the workspace has decided it needs no guardrails
// @Description none=true is only accepted while there are no guardrails (409 otherwise). none=false returns the workspace to undecided. Saving guardrails clears the stance.
// @Tags     brand
// @Accept   json
// @Produce  json
// @Security CookieAuth
// @Param    body body guardrailsStanceRequest true "Stance"
// @Success  200 {object} models.GuardrailsStance
// @Failure  400 {object} map[string]string
// @Failure  409 {object} map[string]string
// @Failure  422 {object} map[string]string
// @Router   /api/brand/guardrails/stance [put]
func (h *BrandHandler) PutGuardrailsStance(c *fiber.Ctx) error {
	var req guardrailsStanceRequest
	if err := json.Unmarshal(c.Body(), &req); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, err.Error())
	}
	if req.None == nil {
		return unprocessable("none is required")
	}
	if !*req.None {
		if err := h.repo.DeleteGuardrailsStance(reqCtx(c)); err != nil {
			return err
		}
		h.recordActivity(c, "brand_guardrails_stance_set", activity.WithPayload(map[string]any{"none": false}))
		return c.JSON(models.StanceOf(nil))
	}
	rec, err := h.repo.SetGuardrailsStance(reqCtx(c), &models.BrandGuardrailsStanceRecord{
		DecidedAt: brandNow(),
		DecidedBy: sessionUserID(c),
	})
	if errors.Is(err, repository.ErrGuardrailsExist) {
		return fiber.NewError(fiber.StatusConflict, "guardrails exist — the stance is only for an empty section")
	}
	if err != nil {
		return err
	}
	h.recordActivity(c, "brand_guardrails_stance_set", activity.WithPayload(map[string]any{"none": true}))
	return c.JSON(models.StanceOf(rec))
}

// changedFactFields lists the editable fields that differ, by wire name.
func changedFactFields(a, b *models.BrandFact) []string {
	var out []string
	add := func(name string, differ bool) {
		if differ {
			out = append(out, name)
		}
	}
	add("statement", a.Statement != b.Statement)
	add("subject", a.Subject != b.Subject)
	add("kind", a.Kind != b.Kind)
	add("source", a.Source != b.Source)
	add("addedAt", !sameDate(a.AddedAt, b.AddedAt))
	add("checkedAt", !sameDate(a.CheckedAt, b.CheckedAt))
	add("expiresAt", !sameDate(a.ExpiresAt, b.ExpiresAt))
	return out
}

func sameDate(a, b *models.CalendarDate) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(b.Time)
}

// rechecked reports whether checkedAt moved forward.
func rechecked(before, after *models.CalendarDate) bool {
	if after == nil {
		return false
	}
	return before == nil || after.After(before.Time)
}
