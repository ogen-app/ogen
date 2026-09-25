package handlers

import (
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
)

type CampaignTypesHandler struct {
	repo repository.CampaignTypeRepository
	auth fiber.Handler
}

func NewCampaignTypesHandler(repo repository.CampaignTypeRepository, auth fiber.Handler) *CampaignTypesHandler {
	return &CampaignTypesHandler{repo: repo, auth: auth}
}

func (h *CampaignTypesHandler) Register(app *fiber.App) {
	g := app.Group("/api/campaign_types")
	g.Get("/", h.auth, h.List)
	g.Post("/", h.auth, h.Create)
	g.Get("/:id", h.auth, h.Get)
	g.Put("/:id", h.auth, h.Update)
	g.Delete("/:id", h.auth, h.Delete)
	g.Post("/:id/clone", h.auth, h.Clone)

	g.Post("/:id/phases", h.auth, h.AddPhase)
	g.Put("/:id/phases/:phase_id", h.auth, h.UpdatePhase)
	g.Delete("/:id/phases/:phase_id", h.auth, h.DeletePhase)
}

type campaignTypeRequest struct {
	Name        string `json:"name"        validate:"required"`
	Label       string `json:"label"       validate:"required"`
	Description string `json:"description"`
}

type cloneCampaignTypeRequest struct {
	Name  string `json:"name"  validate:"required"`
	Label string `json:"label" validate:"required"`
}

type campaignTypePhaseRequest struct {
	Name     string `json:"name"     validate:"required"`
	Purpose  string `json:"purpose"`
	Sequence int    `json:"sequence" validate:"required"`
}

// codeCampaignTypeNameTaken is the 409 for a custom type named like a system
// type or like another of the workspace's own types (CON-314). The UI keys a
// type's label and icon off its name, so a clash would draw two identical cards.
const codeCampaignTypeNameTaken = "campaign_type_name_taken"

// rejectNameTaken writes the name-clash 409 when name is taken, reporting
// whether it did (the response is then already written).
func (h *CampaignTypesHandler) rejectNameTaken(c *fiber.Ctx, name, excludeID string) (bool, error) {
	taken, err := h.repo.NameTaken(reqCtx(c), name, excludeID)
	if err != nil || !taken {
		return false, err
	}
	return true, rejectNameTakenResponse(c)
}

func rejectNameTakenResponse(c *fiber.Ctx) error {
	return rejectCoded(c, fiber.StatusConflict, codeCampaignTypeNameTaken,
		"a campaign type with this name already exists", nil)
}

// ownedType loads a campaign type the caller may modify: 404 when it isn't
// visible to the workspace (unknown, or another workspace's), 403 for a
// system type.
func (h *CampaignTypesHandler) ownedType(c *fiber.Ctx, id string) (*models.CampaignType, error) {
	ct, err := h.repo.GetByID(reqCtx(c), id)
	if err != nil {
		return nil, notFound(err, "campaign type not found")
	}
	if ct.IsSystem {
		return nil, fiber.NewError(fiber.StatusForbidden, "system campaign type cannot be modified")
	}
	return ct, nil
}

// List godoc
// @Summary      List campaign types
// @Description  Returns the system campaign types plus the workspace's own custom types, with their phases, ordered by name.
// @Tags         campaign_types
// @Produce      json
// @Security     CookieAuth
// @Success      200  {array}   models.CampaignType
// @Failure      401  {object}  map[string]string
// @Router       /api/campaign_types [get]
func (h *CampaignTypesHandler) List(c *fiber.Ctx) error {
	types, err := h.repo.List(reqCtx(c))
	if err != nil {
		return err
	}
	return c.JSON(types)
}

// Create godoc
// @Summary      Create campaign type
// @Description  Creates a campaign type owned by the caller's workspace, without phases. 409 campaign_type_name_taken when the name (case-insensitive) is used by a system type or another of the workspace's types.
// @Tags         campaign_types
// @Accept       json
// @Produce      json
// @Security     CookieAuth
// @Param        body  body      campaignTypeRequest  true  "CampaignType payload"
// @Success      201   {object}  models.CampaignType
// @Failure      400   {object}  map[string]string
// @Failure      401   {object}  map[string]string
// @Failure      409   {object}  map[string]string
// @Router       /api/campaign_types [post]
func (h *CampaignTypesHandler) Create(c *fiber.Ctx) error {
	var req campaignTypeRequest
	if err := bindAndValidate(c, &req); err != nil {
		return err
	}

	if rejected, err := h.rejectNameTaken(c, req.Name, ""); rejected || err != nil {
		return err
	}

	id, err := models.NewID()
	if err != nil {
		return err
	}

	ct := &models.CampaignType{
		ID:          id,
		Name:        req.Name,
		Label:       req.Label,
		Description: req.Description,
		IsSystem:    false,
		Phases:      []models.CampaignTypePhase{},
	}
	if err := h.repo.Create(reqCtx(c), ct); err != nil {
		if isUniqueViolation(err) {
			return rejectNameTakenResponse(c)
		}
		return err
	}
	return c.Status(fiber.StatusCreated).JSON(ct)
}

// Get godoc
// @Summary      Get campaign type
// @Description  Returns a single campaign type with its phases by ID. Another workspace's custom type is 404.
// @Tags         campaign_types
// @Produce      json
// @Security     CookieAuth
// @Param        id   path      string  true  "CampaignType ID"
// @Success      200  {object}  models.CampaignType
// @Failure      401  {object}  map[string]string
// @Failure      404  {object}  map[string]string
// @Router       /api/campaign_types/{id} [get]
func (h *CampaignTypesHandler) Get(c *fiber.Ctx) error {
	ct, err := h.repo.GetByID(reqCtx(c), c.Params("id"))
	if err != nil {
		return notFound(err, "campaign type not found")
	}
	return c.JSON(ct)
}

// Update godoc
// @Summary      Update campaign type
// @Description  Updates name, label, and description of one of the workspace's own campaign types. System types cannot be modified. 409 campaign_type_name_taken on a name clash.
// @Tags         campaign_types
// @Accept       json
// @Produce      json
// @Security     CookieAuth
// @Param        id    path      string               true  "CampaignType ID"
// @Param        body  body      campaignTypeRequest  true  "CampaignType payload"
// @Success      200   {object}  models.CampaignType
// @Failure      400   {object}  map[string]string
// @Failure      401   {object}  map[string]string
// @Failure      403   {object}  map[string]string
// @Failure      404   {object}  map[string]string
// @Failure      409   {object}  map[string]string
// @Router       /api/campaign_types/{id} [put]
func (h *CampaignTypesHandler) Update(c *fiber.Ctx) error {
	var req campaignTypeRequest
	if err := bindAndValidate(c, &req); err != nil {
		return err
	}

	ct, err := h.ownedType(c, c.Params("id"))
	if err != nil {
		return err
	}
	if rejected, err := h.rejectNameTaken(c, req.Name, ct.ID); rejected || err != nil {
		return err
	}

	ct.Name = req.Name
	ct.Label = req.Label
	ct.Description = req.Description
	ct.UpdatedAt = time.Now().UTC()

	if err := h.repo.Update(reqCtx(c), ct); err != nil {
		if isUniqueViolation(err) {
			return rejectNameTakenResponse(c)
		}
		return notFound(err, "campaign type not found")
	}
	return c.JSON(ct)
}

// Delete godoc
// @Summary      Delete campaign type
// @Description  Deletes one of the workspace's own campaign types and all its phases. System types cannot be deleted.
// @Tags         campaign_types
// @Security     CookieAuth
// @Param        id   path  string  true  "CampaignType ID"
// @Success      204
// @Failure      401  {object}  map[string]string
// @Failure      403  {object}  map[string]string
// @Failure      404  {object}  map[string]string
// @Router       /api/campaign_types/{id} [delete]
func (h *CampaignTypesHandler) Delete(c *fiber.Ctx) error {
	ct, err := h.repo.GetByID(reqCtx(c), c.Params("id"))
	if err != nil {
		return notFound(err, "campaign type not found")
	}
	if ct.IsSystem {
		return fiber.NewError(fiber.StatusForbidden, "system campaign type cannot be deleted")
	}

	deleted, err := h.repo.Delete(reqCtx(c), ct.ID)
	if err != nil {
		return err
	}
	if !deleted {
		return fiber.NewError(fiber.StatusNotFound, "campaign type not found")
	}
	return c.SendStatus(fiber.StatusNoContent)
}

// Clone godoc
// @Summary      Clone campaign type
// @Description  Creates a workspace-owned campaign type by deep-copying a visible type (system or the workspace's own) and all its phases. 409 campaign_type_name_taken on a name clash.
// @Tags         campaign_types
// @Accept       json
// @Produce      json
// @Security     CookieAuth
// @Param        id    path      string                    true  "Source CampaignType ID"
// @Param        body  body      cloneCampaignTypeRequest  true  "Name and label for the clone"
// @Success      201   {object}  models.CampaignType
// @Failure      400   {object}  map[string]string
// @Failure      401   {object}  map[string]string
// @Failure      404   {object}  map[string]string
// @Failure      409   {object}  map[string]string
// @Router       /api/campaign_types/{id}/clone [post]
func (h *CampaignTypesHandler) Clone(c *fiber.Ctx) error {
	var req cloneCampaignTypeRequest
	if err := bindAndValidate(c, &req); err != nil {
		return err
	}

	src, err := h.repo.GetByID(reqCtx(c), c.Params("id"))
	if err != nil {
		return notFound(err, "campaign type not found")
	}
	if rejected, err := h.rejectNameTaken(c, req.Name, ""); rejected || err != nil {
		return err
	}

	newID, err := models.NewID()
	if err != nil {
		return err
	}

	clone := &models.CampaignType{
		ID:          newID,
		Name:        req.Name,
		Label:       req.Label,
		Description: src.Description,
		IsSystem:    false,
		Phases:      []models.CampaignTypePhase{},
	}
	if err := h.repo.Create(reqCtx(c), clone); err != nil {
		if isUniqueViolation(err) {
			return rejectNameTakenResponse(c)
		}
		return err
	}

	for _, p := range src.Phases {
		phaseID, err := models.NewID()
		if err != nil {
			return err
		}
		phase := &models.CampaignTypePhase{
			ID:             phaseID,
			CampaignTypeID: clone.ID,
			Name:           p.Name,
			Purpose:        p.Purpose,
			Sequence:       p.Sequence,
		}
		if err := h.repo.AddPhase(reqCtx(c), phase); err != nil {
			return err
		}
		clone.Phases = append(clone.Phases, *phase)
	}

	return c.Status(fiber.StatusCreated).JSON(clone)
}

// AddPhase godoc
// @Summary      Add phase to campaign type
// @Description  Adds a phase to one of the workspace's own campaign types. System types are read-only (403).
// @Tags         campaign_types
// @Accept       json
// @Produce      json
// @Security     CookieAuth
// @Param        id    path      string                    true  "CampaignType ID"
// @Param        body  body      campaignTypePhaseRequest  true  "Phase payload"
// @Success      201   {object}  models.CampaignTypePhase
// @Failure      400   {object}  map[string]string
// @Failure      401   {object}  map[string]string
// @Failure      403   {object}  map[string]string
// @Failure      404   {object}  map[string]string
// @Router       /api/campaign_types/{id}/phases [post]
func (h *CampaignTypesHandler) AddPhase(c *fiber.Ctx) error {
	var req campaignTypePhaseRequest
	if err := bindAndValidate(c, &req); err != nil {
		return err
	}

	if _, err := h.ownedType(c, c.Params("id")); err != nil {
		return err
	}

	id, err := models.NewID()
	if err != nil {
		return err
	}

	phase := &models.CampaignTypePhase{
		ID:             id,
		CampaignTypeID: c.Params("id"),
		Name:           req.Name,
		Purpose:        req.Purpose,
		Sequence:       req.Sequence,
	}
	if err := h.repo.AddPhase(reqCtx(c), phase); err != nil {
		return err
	}
	return c.Status(fiber.StatusCreated).JSON(phase)
}

// ownedPhase loads the :phase_id phase of the :id type for a write: 404 unless
// the phase belongs to that type and the type is visible to the workspace, 403
// when it is a system type.
func (h *CampaignTypesHandler) ownedPhase(c *fiber.Ctx) (*models.CampaignTypePhase, error) {
	phase, err := h.repo.GetPhaseByID(reqCtx(c), c.Params("phase_id"))
	if err != nil {
		return nil, notFound(err, "phase not found")
	}
	if phase.CampaignTypeID != c.Params("id") {
		return nil, fiber.NewError(fiber.StatusNotFound, "phase not found")
	}
	if _, err := h.ownedType(c, phase.CampaignTypeID); err != nil {
		return nil, err
	}
	return phase, nil
}

// UpdatePhase godoc
// @Summary      Update phase
// @Description  Updates a phase of one of the workspace's own campaign types. System types are read-only (403).
// @Tags         campaign_types
// @Accept       json
// @Produce      json
// @Security     CookieAuth
// @Param        id        path      string                    true  "CampaignType ID"
// @Param        phase_id  path      string                    true  "Phase ID"
// @Param        body      body      campaignTypePhaseRequest  true  "Phase payload"
// @Success      200       {object}  models.CampaignTypePhase
// @Failure      400       {object}  map[string]string
// @Failure      401       {object}  map[string]string
// @Failure      403       {object}  map[string]string
// @Failure      404       {object}  map[string]string
// @Router       /api/campaign_types/{id}/phases/{phase_id} [put]
func (h *CampaignTypesHandler) UpdatePhase(c *fiber.Ctx) error {
	var req campaignTypePhaseRequest
	if err := bindAndValidate(c, &req); err != nil {
		return err
	}

	phase, err := h.ownedPhase(c)
	if err != nil {
		return err
	}

	phase.Name = req.Name
	phase.Purpose = req.Purpose
	phase.Sequence = req.Sequence
	phase.UpdatedAt = time.Now().UTC()

	if err := h.repo.UpdatePhase(reqCtx(c), phase); err != nil {
		return notFound(err, "phase not found")
	}
	return c.JSON(phase)
}

// DeletePhase godoc
// @Summary      Delete phase
// @Description  Deletes a phase from one of the workspace's own campaign types. 403 for a system type; 409 phase_in_use while any post is planned against it.
// @Tags         campaign_types
// @Security     CookieAuth
// @Param        id        path  string  true  "CampaignType ID"
// @Param        phase_id  path  string  true  "Phase ID"
// @Success      204
// @Failure      401  {object}  map[string]string
// @Failure      403  {object}  map[string]string
// @Failure      404  {object}  map[string]string
// @Failure      409  {object}  map[string]string
// @Router       /api/campaign_types/{id}/phases/{phase_id} [delete]
func (h *CampaignTypesHandler) DeletePhase(c *fiber.Ctx) error {
	phase, err := h.ownedPhase(c)
	if err != nil {
		return err
	}

	deleted, err := h.repo.DeletePhase(reqCtx(c), phase.ID)
	if err != nil {
		// CON-166: posts still reference the phase (posts.campaign_type_phase_id
		// FK) — a client error, not a 500.
		if repository.IsForeignKeyViolation(err) {
			return rejectCoded(c, fiber.StatusConflict, codePhaseInUse,
				"the phase can't be deleted while posts are planned against it", nil)
		}
		return err
	}
	if !deleted {
		return fiber.NewError(fiber.StatusNotFound, "phase not found")
	}
	return c.SendStatus(fiber.StatusNoContent)
}
