package handlers

import (
	"encoding/json"
	"errors"

	"github.com/gofiber/fiber/v2"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/kernel/activity"
	"github.com/ogen-app/ogen/src/usecase/series"
)

// SeriesHandler is the REST surface for series and the campaign runs of them.
// The wire contract is the ui repo's services/api/series.ts. Every endpoint is
// open to any workspace member.
type SeriesHandler struct {
	svc      *series.Service
	auth     fiber.Handler
	activity *activity.Recorder
}

// NewSeriesHandler builds the handler. A nil rec records nothing.
func NewSeriesHandler(svc *series.Service, auth fiber.Handler, rec *activity.Recorder) *SeriesHandler {
	return &SeriesHandler{svc: svc, auth: auth, activity: rec}
}

func (h *SeriesHandler) Register(app *fiber.App) {
	g := app.Group("/api/series", h.auth)
	g.Get("/", h.List)
	g.Post("/", h.Create)
	g.Put("/:id", h.Update)
	g.Delete("/:id", h.Delete)
	g.Post("/:id/promote", h.Promote)

	c := app.Group("/api/campaigns")
	c.Get("/:id/series", h.auth, h.CampaignSeries)
	c.Post("/:id/series", h.auth, h.Attach)
	c.Put("/:id/series/:seriesId", h.auth, h.SetRhythm)
	c.Delete("/:id/series/:seriesId", h.auth, h.Detach)
}

func (h *SeriesHandler) recordActivity(c *fiber.Ctx, typ, seriesID string, payload map[string]any) {
	if h.activity == nil {
		return
	}
	h.activity.Record(reqCtx(c), activity.CategorySeries, typ,
		activity.WithSource(activity.SourceAPI),
		activity.WithEntity("series", seriesID),
		activity.WithPayload(payload),
	)
}

// seriesListResponse is the wrapped GET body: every live series, both scopes.
type seriesListResponse struct {
	Series []models.Series `json:"series"`
}

// seriesRequest is the POST and PUT body. campaign_id is read on create only;
// usage, id and timestamps are server-owned and ignored.
type seriesRequest struct {
	Name          string                `json:"name"`
	Promise       string                `json:"promise"`
	Recipe        string                `json:"recipe"`
	Supply        models.SeriesSupply   `json:"supply"         enums:"self,idea"`
	ContentFormat *models.ContentFormat `json:"content_format" swaggertype:"string" extensions:"x-nullable"`
	DefaultRhythm *models.SeriesRhythm  `json:"default_rhythm" extensions:"x-nullable"`
	CampaignID    *string               `json:"campaign_id"    extensions:"x-nullable"`
}

func (r seriesRequest) input() series.Input {
	return series.Input{
		Name:          r.Name,
		Promise:       r.Promise,
		Recipe:        r.Recipe,
		Supply:        r.Supply,
		ContentFormat: r.ContentFormat,
		DefaultRhythm: r.DefaultRhythm,
		CampaignID:    r.CampaignID,
	}
}

// attachSeriesRequest is the POST /api/campaigns/:id/series body.
type attachSeriesRequest struct {
	SeriesID string `json:"series_id"`
}

// seriesRhythmRequest is the PUT /api/campaigns/:id/series/:seriesId body. The
// key is required; null is occasional.
type seriesRhythmRequest struct {
	Rhythm *models.SeriesRhythm `json:"rhythm" extensions:"x-nullable"`
}

// seriesError maps a service error onto the HTTP response.
func seriesError(err error) error {
	switch {
	case series.IsNotFound(err):
		return fiber.NewError(fiber.StatusNotFound, err.Error())
	case series.IsValidation(err):
		return fiber.NewError(fiber.StatusBadRequest, err.Error())
	default:
		return err
	}
}

func decodeSeriesRequest(c *fiber.Ctx) (seriesRequest, error) {
	var req seriesRequest
	if err := json.Unmarshal(c.Body(), &req); err != nil {
		return req, fiber.NewError(fiber.StatusBadRequest, err.Error())
	}
	return req, nil
}

// List godoc
// @Summary  List series
// @Description Every live series in the workspace: the library (campaign_id null) and the series defined inside campaigns.
// @Tags     series
// @Produce  json
// @Security CookieAuth
// @Success  200 {object} seriesListResponse
// @Router   /api/series [get]
func (h *SeriesHandler) List(c *fiber.Ctx) error {
	list, err := h.svc.List(reqCtx(c))
	if err != nil {
		return err
	}
	return c.JSON(seriesListResponse{Series: list})
}

// Create godoc
// @Summary  Create a series
// @Description With campaign_id set the series is defined inside that campaign and the campaign runs it at its default_rhythm.
// @Tags     series
// @Accept   json
// @Produce  json
// @Security CookieAuth
// @Param    body body seriesRequest true "the series; name and supply are required"
// @Success  201 {object} models.Series
// @Failure  400 {object} map[string]string
// @Failure  404 {object} map[string]string "campaign not found"
// @Router   /api/series [post]
func (h *SeriesHandler) Create(c *fiber.Ctx) error {
	req, err := decodeSeriesRequest(c)
	if err != nil {
		return err
	}
	created, err := h.svc.Create(reqCtx(c), req.input())
	if err != nil {
		return seriesError(err)
	}
	h.recordActivity(c, "series_created", created.ID, map[string]any{"campaign_id": created.CampaignID})
	return c.Status(fiber.StatusCreated).JSON(created)
}

// Update godoc
// @Summary  Replace a series
// @Description Replaces every editable field. campaign_id and usage are ignored: only promote changes the scope.
// @Tags     series
// @Accept   json
// @Produce  json
// @Security CookieAuth
// @Param    id   path string        true "Series id"
// @Param    body body seriesRequest true "the whole series"
// @Success  200 {object} models.Series
// @Failure  400 {object} map[string]string
// @Failure  404 {object} map[string]string
// @Router   /api/series/{id} [put]
func (h *SeriesHandler) Update(c *fiber.Ctx) error {
	req, err := decodeSeriesRequest(c)
	if err != nil {
		return err
	}
	updated, err := h.svc.Update(reqCtx(c), c.Params("id"), req.input())
	if err != nil {
		return seriesError(err)
	}
	h.recordActivity(c, "series_updated", updated.ID, nil)
	return c.JSON(updated)
}

// Delete godoc
// @Summary  Delete a series
// @Description Removes the series and every campaign's run of it. Posts it produced keep their text and their series_id.
// @Tags     series
// @Security CookieAuth
// @Param    id path string true "Series id"
// @Success  204
// @Failure  404 {object} map[string]string
// @Router   /api/series/{id} [delete]
func (h *SeriesHandler) Delete(c *fiber.Ctx) error {
	deleted, err := h.svc.Delete(reqCtx(c), c.Params("id"))
	if err != nil {
		return seriesError(err)
	}
	h.recordActivity(c, "series_deleted", deleted.ID, map[string]any{"campaign_id": deleted.CampaignID})
	return c.SendStatus(fiber.StatusNoContent)
}

// Promote godoc
// @Summary  Promote a campaign's series into the workspace library
// @Description One-way: there is no demote. Promoting a library series is a no-op.
// @Tags     series
// @Produce  json
// @Security CookieAuth
// @Param    id path string true "Series id"
// @Success  200 {object} models.Series
// @Failure  404 {object} map[string]string
// @Router   /api/series/{id}/promote [post]
func (h *SeriesHandler) Promote(c *fiber.Ctx) error {
	promoted, changed, err := h.svc.Promote(reqCtx(c), c.Params("id"))
	if err != nil {
		return seriesError(err)
	}
	if changed {
		h.recordActivity(c, "series_promoted", promoted.ID, nil)
	}
	return c.JSON(promoted)
}

// CampaignSeries godoc
// @Summary  List the series a campaign runs
// @Tags     series
// @Produce  json
// @Security CookieAuth
// @Param    id path string true "Campaign id"
// @Success  200 {object} models.CampaignSeries
// @Failure  404 {object} map[string]string
// @Router   /api/campaigns/{id}/series [get]
func (h *SeriesHandler) CampaignSeries(c *fiber.Ctx) error {
	runs, err := h.svc.CampaignSeries(reqCtx(c), c.Params("id"))
	if err != nil {
		return seriesError(err)
	}
	return c.JSON(runs)
}

// Attach godoc
// @Summary  Attach a series to a campaign
// @Description Idempotent: a series the campaign already runs keeps its rhythm. A new run starts at the series' default_rhythm.
// @Tags     series
// @Accept   json
// @Produce  json
// @Security CookieAuth
// @Param    id   path string              true "Campaign id"
// @Param    body body attachSeriesRequest true "the series to attach"
// @Success  200 {object} models.CampaignSeries
// @Failure  400 {object} map[string]string "series belongs to another campaign"
// @Failure  404 {object} map[string]string
// @Router   /api/campaigns/{id}/series [post]
func (h *SeriesHandler) Attach(c *fiber.Ctx) error {
	var req attachSeriesRequest
	if err := json.Unmarshal(c.Body(), &req); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, err.Error())
	}
	if req.SeriesID == "" {
		return fiber.NewError(fiber.StatusBadRequest, "series_id is required")
	}
	campaignID := c.Params("id")
	runs, err := h.svc.Attach(reqCtx(c), campaignID, req.SeriesID)
	if err != nil {
		return seriesError(err)
	}
	h.recordActivity(c, "campaign_series_attached", req.SeriesID, map[string]any{"campaign_id": campaignID})
	return c.JSON(runs)
}

// Detach godoc
// @Summary  Detach a series from a campaign
// @Description Detaching a series the campaign does not run returns the campaign unchanged.
// @Tags     series
// @Produce  json
// @Security CookieAuth
// @Param    id       path string true "Campaign id"
// @Param    seriesId path string true "Series id"
// @Success  200 {object} models.CampaignSeries
// @Failure  404 {object} map[string]string
// @Router   /api/campaigns/{id}/series/{seriesId} [delete]
func (h *SeriesHandler) Detach(c *fiber.Ctx) error {
	campaignID, seriesID := c.Params("id"), c.Params("seriesId")
	runs, err := h.svc.Detach(reqCtx(c), campaignID, seriesID)
	if err != nil {
		return seriesError(err)
	}
	h.recordActivity(c, "campaign_series_detached", seriesID, map[string]any{"campaign_id": campaignID})
	return c.JSON(runs)
}

// SetRhythm godoc
// @Summary  Set how often a campaign runs one of its series
// @Description Touches one run. rhythm null is occasional: the series claims no plan slots.
// @Tags     series
// @Accept   json
// @Produce  json
// @Security CookieAuth
// @Param    id       path string              true "Campaign id"
// @Param    seriesId path string              true "Series id"
// @Param    body     body seriesRhythmRequest true "rhythm is required; null is occasional"
// @Success  200 {object} models.CampaignSeries
// @Failure  400 {object} map[string]string
// @Failure  404 {object} map[string]string
// @Router   /api/campaigns/{id}/series/{seriesId} [put]
func (h *SeriesHandler) SetRhythm(c *fiber.Ctx) error {
	raw, err := bodyKeys(c, []string{"rhythm"})
	if err != nil {
		return err
	}
	if _, ok := raw["rhythm"]; !ok {
		return fiber.NewError(fiber.StatusBadRequest, "rhythm is required")
	}
	var req seriesRhythmRequest
	if err := json.Unmarshal(c.Body(), &req); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, err.Error())
	}
	campaignID, seriesID := c.Params("id"), c.Params("seriesId")
	runs, err := h.svc.SetRhythm(reqCtx(c), campaignID, seriesID, req.Rhythm)
	if err != nil {
		return seriesError(err)
	}
	h.recordActivity(c, "campaign_series_rhythm_set", seriesID, map[string]any{
		"campaign_id": campaignID,
		"rhythm":      req.Rhythm,
	})
	return c.JSON(runs)
}

// checkPostSeries validates a post's series_id against the post's campaign.
// nil svc skips validation.
func checkPostSeries(c *fiber.Ctx, svc *series.Service, seriesID *string, campaignID string) error {
	if svc == nil || seriesID == nil {
		return nil
	}
	err := svc.CheckPostSeries(reqCtx(c), *seriesID, campaignID)
	if errors.Is(err, series.ErrPostSeriesNotFound) || errors.Is(err, series.ErrSeriesOutOfScope) {
		return fiber.NewError(fiber.StatusBadRequest, err.Error())
	}
	return err
}
