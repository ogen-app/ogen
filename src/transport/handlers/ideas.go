package handlers

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/kernel/activity"
	"github.com/ogen-app/ogen/src/usecase/ideas"
)

// IdeasHandler is the REST surface for the workspace Ideas backlog.
// The wire contract is the header comment in the ui repo's
// services/api/ideas.ts. Every endpoint is open to any workspace member; the
// verdict has its own endpoint so a decision can never ride along with an edit.
type IdeasHandler struct {
	svc      *ideas.Service
	auth     fiber.Handler
	activity *activity.Recorder
}

// NewIdeasHandler builds the handler. A nil rec records nothing.
func NewIdeasHandler(svc *ideas.Service, auth fiber.Handler, rec *activity.Recorder) *IdeasHandler {
	return &IdeasHandler{svc: svc, auth: auth, activity: rec}
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

// IdeaCodeReadOnly is the code of the 409 refusing an edit of an analysed
// idea's text.
const IdeaCodeReadOnly = "idea_read_only"

// ideaListResponse is one page of the ideas list.
type ideaListResponse struct {
	Ideas []models.Idea `json:"ideas"`
	// Total counts the ideas matching every filter, TotalUnfiltered every
	// idea in scope ("9 of 47").
	Total           int `json:"total"`
	TotalUnfiltered int `json:"total_unfiltered"`
	// Piles counts the match per pile, ignoring the pile filter, so the four
	// add up to the match of every other filter.
	Piles ideaPiles `json:"piles"`
	// Facets holds, per requested field, its values with the number of ideas
	// that have each.
	Facets      map[string][]models.AssetFacetCount `json:"facets"`
	FacetLabels ideaFacetLabels                     `json:"facet_labels"`
	// Groups lists, with group, every non-empty group of the whole match with
	// its row count, in display order; empty without group.
	Groups []models.AssetGroupCount `json:"groups"`
	// NextCursor continues the list in the same sort; null on the last page.
	NextCursor *string `json:"next_cursor" extensions:"x-nullable"`
}

// ideaPiles is the size of each pile tab.
type ideaPiles struct {
	Waiting int `json:"waiting"`
	Yes     int `json:"yes"`
	Later   int `json:"later"`
	No      int `json:"no"`
}

// ideaFacetLabels labels facet values that are ids.
type ideaFacetLabels struct {
	// SourceAsset labels every asset of the source_asset facet, zero counts
	// included; present only when that facet is requested.
	SourceAsset []models.IdeaFacetAsset `json:"source_asset,omitempty"`
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
	Verdict  *string    `json:"verdict"   enums:"yes,later,no" extensions:"x-nullable"`
	RemindAt *time.Time `json:"remind_at" extensions:"x-nullable"`
}

// ideaVerdictFields is the exact key set PUT …/verdict requires.
var ideaVerdictFields = []string{"verdict", "remind_at"}

// ideaError maps a service error onto the HTTP response.
func ideaError(c *fiber.Ctx, err error) error {
	switch {
	case errors.Is(err, ideas.ErrNotFound):
		return fiber.NewError(fiber.StatusNotFound, "idea not found")
	case errors.Is(err, ideas.ErrReadOnly):
		return rejectCode(c, fiber.StatusConflict, IdeaCodeReadOnly, err.Error())
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
		slices.Sort(rejected)
		return nil, fiber.NewError(fiber.StatusBadRequest, fmt.Sprintf("field %q cannot be set here", rejected[0]))
	}
	return raw, nil
}

// List godoc
// @Summary      List workspace ideas
// @Description  Filters, counts, groups and pages the workspace's ideas.
// @Description
// @Description  Every filter takes a comma-separated list meaning "any of these"; its `_not` twin excludes values. Different fields combine with AND. `pile` splits the ideas into `waiting` (undecided, or `later` with its reminder passed), `yes`, `later` (still snoozed) and `no`. `source_asset` and `campaign_id` take ids plus `none` (written ideas or a deleted source; workspace-wide). `kind` is the source asset's kind; `author` takes user ids and matches manual ideas only. `fit` bands are `high` (70–100), `medium` (40–69), `low` (10–39) and `unscored`. `text` matches title or note as a case-insensitive substring. `created` takes the presets `1d`, `7d`, `30d` and `month` in `tz`; `created_after` and `created_before` take RFC 3339 or YYYY-MM-DD.
// @Description
// @Description  `scope_campaign` confines the whole read, totals and counts included, to one campaign's ideas; the campaign_id facet is not returned there. `piles` count the match of every filter but `pile`. `facets` names the fields to count: each field's counts apply every other filter but not its own, and list every value an idea in scope has, with 0 for those nothing matches. `facet_labels.source_asset` labels every asset of the source_asset facet.
// @Description
// @Description  `group` orders the rows by group first, then by `sort` within each group, and adds `group_key` to every row and `groups` (each non-empty group's key and row count over the whole match, in display order). `source`, `campaign` and `author` groups go by name with `none` last; `fit` and `origin` keep their value order; `created` groups are `1d`, `7d`, `30d` and `earlier`.
// @Description
// @Description  Without `limit` the list is unpaged, up to 1,000 ideas.
// @Tags         ideas
// @Produce      json
// @Security     CookieAuth
// @Param        pile            query  string  false  "waiting, yes, later, no"
// @Param        pile_not        query  string  false  "Piles to exclude"
// @Param        text            query  string  false  "Title or note substring (at most 200 characters)"
// @Param        origin          query  string  false  "asset_analysis, manual"
// @Param        origin_not      query  string  false  "Origins to exclude"
// @Param        source_asset    query  string  false  "Asset ids, or none"
// @Param        source_asset_not query string  false  "Asset ids (or none) to exclude"
// @Param        asset_id        query  string  false  "One source asset id (same as source_asset)"
// @Param        kind            query  string  false  "Source asset kinds: pdf, web, audio, note, image, document, spreadsheet, csv, presentation"
// @Param        kind_not        query  string  false  "Kinds to exclude"
// @Param        campaign_id     query  string  false  "Campaign ids, or none"
// @Param        campaign_id_not query  string  false  "Campaign ids (or none) to exclude"
// @Param        fit             query  string  false  "high, medium, low, unscored"
// @Param        fit_not         query  string  false  "Fit bands to exclude"
// @Param        phase           query  string  false  "Phase names the fit rating placed the idea in"
// @Param        phase_not       query  string  false  "Phases to exclude"
// @Param        author          query  string  false  "User ids of manual ideas' authors"
// @Param        author_not      query  string  false  "Authors to exclude"
// @Param        created         query  string  false  "Preset start of the created window: 1d, 7d, 30d, month (not with created_after)"
// @Param        created_after   query  string  false  "RFC 3339 or YYYY-MM-DD, inclusive"
// @Param        created_before  query  string  false  "RFC 3339 or YYYY-MM-DD, exclusive"
// @Param        scope_campaign  query  string  false  "Campaign id to confine the read to"
// @Param        sort            query  string  false  "created (default, oldest first), created_desc, fit (best first, unscored last), waiting (longest waiting first)"
// @Param        group           query  string  false  "source, campaign, fit, origin, author, created"
// @Param        facets          query  string  false  "origin, source_asset, kind, campaign_id, fit, phase, author, created"
// @Param        limit           query  int     false  "Page size, at most 200; unpaged up to 1,000 when absent"
// @Param        cursor          query  string  false  "next_cursor of the previous page"
// @Param        tz              query  string  false  "IANA time zone of the date presets and created groups (default UTC)"
// @Success      200  {object}  ideaListResponse
// @Failure      400  {object}  map[string]string  "code: invalid_filter | invalid_cursor"
// @Failure      404  {object}  map[string]string  "scope_campaign names no live campaign"
// @Router       /api/ideas [get]
func (h *IdeasHandler) List(c *fiber.Ctx) error {
	q, err := ideas.ParseQuery(queryValuesOf(c), time.Now().UTC())
	if err != nil {
		return listQueryFailure(c, err)
	}
	res, err := h.svc.Query(reqCtx(c), q)
	switch {
	case errors.Is(err, ideas.ErrScopeNotFound):
		return fiber.NewError(fiber.StatusNotFound, "campaign not found")
	case err != nil:
		return listQueryFailure(c, err)
	}
	resp := ideaListResponse{
		Ideas: res.Ideas, Total: res.Total, TotalUnfiltered: res.TotalUnfiltered,
		Piles: ideaPiles{
			Waiting: res.Piles[models.IdeaPileWaiting], Yes: res.Piles[models.IdeaPileYes],
			Later: res.Piles[models.IdeaPileLater], No: res.Piles[models.IdeaPileNo],
		},
		Facets:      res.Facets,
		FacetLabels: ideaFacetLabels{SourceAsset: res.SourceAssetLabels},
		Groups:      res.Groups,
	}
	if resp.Facets == nil {
		resp.Facets = map[string][]models.AssetFacetCount{}
	}
	if res.NextCursor != "" {
		resp.NextCursor = &res.NextCursor
	}
	return c.JSON(resp)
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
	session, err := sessionFrom(c)
	if err != nil {
		return err
	}
	idea, err := h.svc.Create(reqCtx(c), ideas.CreateInput{
		Title:      req.Title,
		Note:       req.Note,
		CampaignID: req.CampaignID,
		UserID:     session.UserID,
	})
	if err != nil {
		return ideaError(c, err)
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
		return ideaError(c, err)
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
	session, err := sessionFrom(c)
	if err != nil {
		return err
	}
	idea, previous, err := h.svc.SetVerdict(reqCtx(c), c.Params("id"), verdict, req.RemindAt, session.UserID)
	if err != nil {
		return ideaError(c, err)
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
		return ideaError(c, err)
	}
	h.recordActivity(c, "idea_deleted", idea.ID, map[string]any{"verdict": idea.Verdict})
	return c.SendStatus(fiber.StatusNoContent)
}
