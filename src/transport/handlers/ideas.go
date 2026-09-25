package handlers

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/activity"
	"github.com/ogen-app/ogen/src/usecase/ideas"
)

// IdeasHandler is the REST surface for the workspace Ideas backlog (CON-315).
// The wire contract is the header comment in the ui repo's
// services/api/ideas.ts. Every endpoint is open to any workspace member; the
// verdict has its own endpoint so a decision can never ride along with an edit.
type IdeasHandler struct {
	svc      *ideas.Service
	auth     fiber.Handler
	activity *activity.Recorder
}

func NewIdeasHandler(svc *ideas.Service, auth fiber.Handler) *IdeasHandler {
	return &IdeasHandler{svc: svc, auth: auth}
}

// SetActivityRecorder wires the CON-125 activity recorder. nil is a no-op.
func (h *IdeasHandler) SetActivityRecorder(r *activity.Recorder) {
	h.activity = r
}

func (h *IdeasHandler) recordActivity(c *fiber.Ctx, typ, ideaID string, payload map[string]any) {
	if h.activity == nil {
		return
	}
	h.activity.Record(reqCtx(c), activity.CategoryIdea, typ,
		activity.WithSource(activity.SourceAPI),
		activity.WithEntity("idea", ideaID),
		activity.WithPayload(payload),
	)
}

func (h *IdeasHandler) Register(app *fiber.App) {
	g := app.Group("/api/ideas", h.auth)
	g.Get("/", h.List)
	g.Post("/", h.Create)
	g.Patch("/:id", h.Update)
	g.Put("/:id/verdict", h.SetVerdict)
	g.Delete("/:id", h.Delete)
}

// ideaCampaignNone is the ?campaign_id value that lists only workspace-wide ideas.
const ideaCampaignNone = "none"

// ideaListResponse is the wrapped GET body — an object, not a bare array.
type ideaListResponse struct {
	Ideas []models.Idea `json:"ideas"`
}

// createIdeaRequest is the POST body. Only title is required; any author or
// decision field in the body is ignored — the server stamps those.
type createIdeaRequest struct {
	Title      string  `json:"title"`
	Note       string  `json:"note"`
	CampaignID *string `json:"campaign_id"`
}

// updateIdeaRequest is the presence-aware PATCH body: an omitted key is left
// unchanged, a present one replaces the stored value (note "" clears the note,
// campaign_id null detaches the idea). No other key is accepted.
type updateIdeaRequest struct {
	Title      Optional[string] `json:"title"       swaggertype:"string"`
	Note       Optional[string] `json:"note"        swaggertype:"string"`
	CampaignID Optional[string] `json:"campaign_id" swaggertype:"string"`
}

// updateIdeaFields is the closed set of keys PATCH accepts.
var updateIdeaFields = []string{"title", "note", "campaign_id"}

// ideaVerdictRequest is the PUT …/verdict body. Both keys must be present;
// verdict null returns the idea to the inbox.
type ideaVerdictRequest struct {
	Verdict  *string    `json:"verdict"   enums:"yes,later,no"`
	RemindAt *time.Time `json:"remind_at"`
}

// ideaVerdictFields is the exact key set PUT …/verdict requires.
var ideaVerdictFields = []string{"verdict", "remind_at"}

// ideaError maps a service error onto the HTTP response.
func ideaError(err error) error {
	switch {
	case errors.Is(err, ideas.ErrNotFound):
		return fiber.NewError(fiber.StatusNotFound, "idea not found")
	case ideas.IsValidation(err):
		return fiber.NewError(fiber.StatusBadRequest, err.Error())
	default:
		return err
	}
}

// bodyKeys decodes a JSON object body into its raw keys, rejecting anything
// outside allowed so reserved fields (verdict, remind_at, created_by, …) are a
// 400 rather than silently ignored.
func bodyKeys(c *fiber.Ctx, allowed []string) (map[string]json.RawMessage, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(c.Body(), &raw); err != nil || raw == nil {
		return nil, fiber.NewError(fiber.StatusBadRequest, "request body must be a JSON object")
	}
	var rejected []string
	for k := range raw {
		if !slices.Contains(allowed, k) {
			rejected = append(rejected, k)
		}
	}
	if len(rejected) > 0 {
		sort.Strings(rejected)
		return nil, fiber.NewError(fiber.StatusBadRequest, fmt.Sprintf("field %q cannot be set here", rejected[0]))
	}
	return raw, nil
}

// List godoc
// @Summary  List workspace ideas
// @Tags     ideas
// @Produce  json
// @Security CookieAuth
// @Param    campaign_id query string false "only ideas attached to this campaign, or `none` for workspace-wide ideas"
// @Success  200 {object} ideaListResponse
// @Router   /api/ideas [get]
func (h *IdeasHandler) List(c *fiber.Ctx) error {
	var filter repository.IdeaListFilter
	switch campaignID := c.Query("campaign_id"); campaignID {
	case "":
	case ideaCampaignNone:
		filter.Unfiled = true
	default:
		filter.CampaignID = campaignID
	}
	list, err := h.svc.List(reqCtx(c), filter)
	if err != nil {
		return err
	}
	return c.JSON(ideaListResponse{Ideas: list})
}

// Create godoc
// @Summary  Capture an idea
// @Tags     ideas
// @Accept   json
// @Produce  json
// @Security CookieAuth
// @Param    body body createIdeaRequest true "the idea; only title is required"
// @Success  201 {object} models.Idea
// @Failure  400 {object} map[string]string
// @Router   /api/ideas [post]
func (h *IdeasHandler) Create(c *fiber.Ctx) error {
	var req createIdeaRequest
	if err := json.Unmarshal(c.Body(), &req); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, err.Error())
	}
	session := c.Locals("session").(*models.Session)
	idea, err := h.svc.Create(reqCtx(c), ideas.CreateInput{
		Title:      req.Title,
		Note:       req.Note,
		CampaignID: req.CampaignID,
		UserID:     session.UserID,
	})
	if err != nil {
		return ideaError(err)
	}
	h.recordActivity(c, "idea_captured", idea.ID, map[string]any{"campaign_id": idea.CampaignID})
	return c.Status(fiber.StatusCreated).JSON(idea)
}

// Update godoc
// @Summary  Edit an idea's title, note, or campaign (presence-aware)
// @Tags     ideas
// @Accept   json
// @Produce  json
// @Security CookieAuth
// @Param    id   path string            true "Idea id"
// @Param    body body updateIdeaRequest true "any of title, note, campaign_id (null detaches)"
// @Success  200 {object} models.Idea
// @Failure  400 {object} map[string]string
// @Failure  404 {object} map[string]string
// @Router   /api/ideas/{id} [patch]
func (h *IdeasHandler) Update(c *fiber.Ctx) error {
	raw, err := bodyKeys(c, updateIdeaFields)
	if err != nil {
		return err
	}
	if len(raw) == 0 {
		return fiber.NewError(fiber.StatusBadRequest, "at least one of title, note or campaign_id is required")
	}
	var req updateIdeaRequest
	if err := json.Unmarshal(c.Body(), &req); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, err.Error())
	}
	if (req.Title.Present && req.Title.Value == nil) || (req.Note.Present && req.Note.Value == nil) {
		return fiber.NewError(fiber.StatusBadRequest, "title and note cannot be null")
	}
	idea, fields, err := h.svc.Update(reqCtx(c), c.Params("id"), ideas.UpdateInput{
		Title:       req.Title.Value,
		Note:        req.Note.Value,
		SetCampaign: req.CampaignID.Present,
		CampaignID:  req.CampaignID.Value,
	})
	if err != nil {
		return ideaError(err)
	}
	h.recordActivity(c, "idea_updated", idea.ID, map[string]any{"fields": fields})
	return c.JSON(idea)
}

// SetVerdict godoc
// @Summary  Triage an idea: yes, later (with remind_at), no, or null (back to the inbox)
// @Tags     ideas
// @Accept   json
// @Produce  json
// @Security CookieAuth
// @Param    id   path string             true "Idea id"
// @Param    body body ideaVerdictRequest true "both keys required; remind_at only for later"
// @Success  200 {object} models.Idea
// @Failure  400 {object} map[string]string
// @Failure  404 {object} map[string]string
// @Router   /api/ideas/{id}/verdict [put]
func (h *IdeasHandler) SetVerdict(c *fiber.Ctx) error {
	raw, err := bodyKeys(c, ideaVerdictFields)
	if err != nil {
		return err
	}
	for _, k := range ideaVerdictFields {
		if _, ok := raw[k]; !ok {
			return fiber.NewError(fiber.StatusBadRequest, "verdict and remind_at are both required")
		}
	}
	var req ideaVerdictRequest
	if err := json.Unmarshal(c.Body(), &req); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, err.Error())
	}
	var verdict *models.IdeaVerdict
	if req.Verdict != nil {
		v := models.IdeaVerdict(*req.Verdict)
		verdict = &v
	}
	session := c.Locals("session").(*models.Session)
	idea, previous, err := h.svc.SetVerdict(reqCtx(c), c.Params("id"), verdict, req.RemindAt, session.UserID)
	if err != nil {
		return ideaError(err)
	}
	h.recordActivity(c, "idea_decided", idea.ID, map[string]any{
		"verdict":          idea.Verdict,
		"previous_verdict": previous,
		"remind_at":        idea.RemindAt,
	})
	return c.JSON(idea)
}

// Delete godoc
// @Summary  Delete an idea (hard delete)
// @Tags     ideas
// @Security CookieAuth
// @Param    id path string true "Idea id"
// @Success  204
// @Failure  404 {object} map[string]string
// @Router   /api/ideas/{id} [delete]
func (h *IdeasHandler) Delete(c *fiber.Ctx) error {
	idea, err := h.svc.Delete(reqCtx(c), c.Params("id"))
	if err != nil {
		return ideaError(err)
	}
	h.recordActivity(c, "idea_deleted", idea.ID, map[string]any{"verdict": idea.Verdict})
	return c.SendStatus(fiber.StatusNoContent)
}
