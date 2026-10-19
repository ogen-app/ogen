package handlers

import (
	"encoding/json"
	"errors"

	"github.com/gofiber/fiber/v2"

	"github.com/ogen-app/ogen/src/domain/listquery"
	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/usecase/savedviews"
)

// Machine-readable codes of the saved-views 400s.
const (
	SavedViewCodeUnknownSurface = "unknown_surface"
	SavedViewCodeInvalidQuery   = "invalid_query"
)

// SavedViewsHandler is the REST surface for saved views (smart groups) of
// any filterable list. Every member may list and create views; changing one
// is for its creator, or an owner when it is shared or its creator has left.
type SavedViewsHandler struct {
	svc   *savedviews.Service
	users repository.UserRepository
	auth  fiber.Handler
}

func NewSavedViewsHandler(svc *savedviews.Service, users repository.UserRepository, auth fiber.Handler) *SavedViewsHandler {
	return &SavedViewsHandler{svc: svc, users: users, auth: auth}
}

func (h *SavedViewsHandler) Register(app *fiber.App) {
	g := app.Group("/api/saved-views", h.auth)
	g.Get("/", h.List)
	g.Post("/", h.Create)
	g.Patch("/:id", h.Update)
	g.Delete("/:id", h.Delete)
}

// savedViewListResponse is the wrapped GET body.
type savedViewListResponse struct {
	Views []models.SavedView `json:"views"`
}

// createSavedViewRequest is the POST body.
type createSavedViewRequest struct {
	// Surface is the list: ideas or assets.
	Surface string `json:"surface"`
	Name    string `json:"name"`
	// Query is the list's query string, as the list's endpoint takes it.
	Query  string `json:"query"`
	Shared bool   `json:"shared"`
}

// updateSavedViewRequest is the presence-aware PATCH body.
type updateSavedViewRequest struct {
	Name   Optional[string] `json:"name"   swaggertype:"string"`
	Query  Optional[string] `json:"query"  swaggertype:"string"`
	Shared Optional[bool]   `json:"shared" swaggertype:"boolean"`
}

var updateSavedViewFields = []string{"name", "query", "shared"}

func (h *SavedViewsHandler) caller(c *fiber.Ctx) (savedviews.Caller, error) {
	user, err := callerUser(c, h.users)
	if err != nil {
		return savedviews.Caller{}, err
	}
	return savedviews.Caller{UserID: user.ID, Owner: user.Role == models.RoleOwner}, nil
}

func savedViewError(c *fiber.Ctx, err error) error {
	if qe, ok := errors.AsType[*savedviews.QueryError](err); ok {
		body := fiber.Map{"code": SavedViewCodeInvalidQuery, "error": qe.Msg}
		if qe.Param != "" {
			body["param"] = qe.Param
		}
		return c.Status(fiber.StatusBadRequest).JSON(body)
	}
	switch {
	case errors.Is(err, savedviews.ErrUnknownSurface):
		return rejectCode(c, fiber.StatusBadRequest, SavedViewCodeUnknownSurface, err.Error())
	case errors.Is(err, savedviews.ErrInvalidName), errors.Is(err, savedviews.ErrUnknownCount),
		errors.Is(err, savedviews.ErrOwnerlessPrivate):
		return fiber.NewError(fiber.StatusBadRequest, err.Error())
	case errors.Is(err, savedviews.ErrNotFound):
		return fiber.NewError(fiber.StatusNotFound, err.Error())
	case errors.Is(err, savedviews.ErrForbidden):
		return fiber.NewError(fiber.StatusForbidden, err.Error())
	}
	return err
}

// List godoc
// @Summary      List saved views of a list
// @Description  The caller's own views of the surface plus every shared one, newest first. `counts` adds each valid view's counts by name, all computed in one round trip: every surface has `total`; `ideas` adds `undecided` (its waiting pile), `assets` has `total` alone. A view whose stored query no longer parses reads back with `valid: false` and no counts. `can_edit` says whether the caller may change it.
// @Tags         saved-views
// @Produce      json
// @Security     CookieAuth
// @Param        surface  query  string  true   "The list: ideas | assets"
// @Param        counts   query  string  false  "Comma-separated count names"
// @Param        tz       query  string  false  "IANA time zone of the date presets and day bounds the counts read (default UTC)"
// @Success      200  {object}  savedViewListResponse
// @Failure      400  {object}  map[string]string  "code: unknown_surface | invalid_filter"
// @Router       /api/saved-views [get]
func (h *SavedViewsHandler) List(c *fiber.Ctx) error {
	caller, err := h.caller(c)
	if err != nil {
		return err
	}
	v := queryValuesOf(c)
	loc, err := listquery.Location(v)
	if err != nil {
		return listQueryFailure(c, err)
	}
	views, err := h.svc.List(reqCtx(c), caller, listquery.Single(v, "surface"), listquery.Values(v, "counts"), loc)
	if err != nil {
		return savedViewError(c, err)
	}
	return c.JSON(savedViewListResponse{Views: views})
}

// Create godoc
// @Summary      Save a view of a list
// @Description  `query` is the list's query string. Paging and display parameters (`cursor`, `limit`, `facets`, `tz`, and per list e.g. `pile` on ideas, `ids_only`, `scope_campaign` and `checksum` on assets) are dropped; the rest is validated by the list's own parser and stored canonical: keys sorted, values sorted and comma-joined.
// @Tags         saved-views
// @Accept       json
// @Produce      json
// @Security     CookieAuth
// @Param        body  body  createSavedViewRequest  true  "the view"
// @Success      201  {object}  models.SavedView
// @Failure      400  {object}  map[string]string  "code: unknown_surface | invalid_query (with param)"
// @Router       /api/saved-views [post]
func (h *SavedViewsHandler) Create(c *fiber.Ctx) error {
	var req createSavedViewRequest
	if err := json.Unmarshal(c.Body(), &req); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "request body must be a JSON object")
	}
	caller, err := h.caller(c)
	if err != nil {
		return err
	}
	view, err := h.svc.Create(reqCtx(c), caller, savedviews.CreateInput{
		Surface: req.Surface, Name: req.Name, Query: req.Query, Shared: req.Shared,
	})
	if err != nil {
		return savedViewError(c, err)
	}
	return c.Status(fiber.StatusCreated).JSON(view)
}

// Update godoc
// @Summary      Rename, re-filter or (un)share a saved view (presence-aware)
// @Tags         saved-views
// @Accept       json
// @Produce      json
// @Security     CookieAuth
// @Param        id    path  string                  true  "Saved view id"
// @Param        body  body  updateSavedViewRequest  true  "any of name, query, shared"
// @Success      200  {object}  models.SavedView
// @Failure      400  {object}  map[string]string
// @Failure      403  {object}  map[string]string
// @Failure      404  {object}  map[string]string
// @Router       /api/saved-views/{id} [patch]
func (h *SavedViewsHandler) Update(c *fiber.Ctx) error {
	raw, err := bodyKeys(c, updateSavedViewFields)
	if err != nil {
		return err
	}
	if len(raw) == 0 {
		return fiber.NewError(fiber.StatusBadRequest, "at least one of name, query or shared is required")
	}
	var req updateSavedViewRequest
	if err := json.Unmarshal(c.Body(), &req); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, err.Error())
	}
	for _, null := range []bool{
		req.Name.Present && req.Name.Value == nil,
		req.Query.Present && req.Query.Value == nil,
		req.Shared.Present && req.Shared.Value == nil,
	} {
		if null {
			return fiber.NewError(fiber.StatusBadRequest, "name, query and shared cannot be null")
		}
	}
	caller, err := h.caller(c)
	if err != nil {
		return err
	}
	view, err := h.svc.Update(reqCtx(c), caller, c.Params("id"), savedviews.UpdateInput{
		Name: req.Name.Value, Query: req.Query.Value, Shared: req.Shared.Value,
	})
	if err != nil {
		return savedViewError(c, err)
	}
	return c.JSON(view)
}

// Delete godoc
// @Summary   Delete a saved view
// @Tags      saved-views
// @Security  CookieAuth
// @Param     id  path  string  true  "Saved view id"
// @Success   204
// @Failure   403  {object}  map[string]string
// @Failure   404  {object}  map[string]string
// @Router    /api/saved-views/{id} [delete]
func (h *SavedViewsHandler) Delete(c *fiber.Ctx) error {
	caller, err := h.caller(c)
	if err != nil {
		return err
	}
	if err := h.svc.Delete(reqCtx(c), caller, c.Params("id")); err != nil {
		return savedViewError(c, err)
	}
	return c.SendStatus(fiber.StatusNoContent)
}
