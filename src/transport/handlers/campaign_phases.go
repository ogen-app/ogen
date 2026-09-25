package handlers

import (
	"errors"
	"log/slog"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/ogen-app/ogen/src/domain/campaignphase"
	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/activity"
	"github.com/ogen-app/ogen/src/kernel/logging"
)

// Stable machine-readable codes for the CON-166 phase-plan / type-lock /
// phase-integrity rejections, returned beside the human "error" message.
const (
	codeCampaignTypeLocked         = "campaign_type_locked"
	codeCampaignUnscheduled        = "campaign_unscheduled"
	codeInvalidPhasePlan           = "invalid_phase_plan"
	codeInvalidCampaignTypePhaseID = "invalid_campaign_type_phase_id"
	codePhaseInUse                 = "phase_in_use"
)

// rejectCoded writes a 4xx rejection carrying a stable code beside the human
// message (the CON-281 {code,error} shape), plus any extra fields. It returns
// nil because the response is already written — the central error handler has
// no slot for a code.
func rejectCoded(c *fiber.Ctx, status int, code, msg string, extra fiber.Map) error {
	body := fiber.Map{"code": code, "error": msg}
	for k, v := range extra {
		body[k] = v
	}
	return c.Status(status).JSON(body)
}

// rejectTypeLocked is the 409 for a campaign-type change on a campaign whose
// posts are planned against its type's phases (CON-166).
func rejectTypeLocked(c *fiber.Ctx, phasedPostCount int) error {
	return rejectCoded(c, fiber.StatusConflict, codeCampaignTypeLocked,
		"the campaign type can't be changed: posts are planned against its phases",
		fiber.Map{"phased_post_count": phasedPostCount})
}

// rejectInvalidPhase is the 400 for a post whose campaign_type_phase_id isn't a
// phase of its campaign's type (CON-166).
func rejectInvalidPhase(c *fiber.Ctx) error {
	return rejectCoded(c, fiber.StatusBadRequest, codeInvalidCampaignTypePhaseID,
		"campaign_type_phase_id is not a phase of the campaign's type", nil)
}

// CampaignPhasesHandler serves a campaign's phase date plan (CON-166): the
// per-phase windows, derived from the campaign dates by default and
// overridable by the user.
type CampaignPhasesHandler struct {
	repo     repository.CampaignRepository
	auth     fiber.Handler
	activity *activity.Recorder
}

func NewCampaignPhasesHandler(repo repository.CampaignRepository, recorder *activity.Recorder, auth fiber.Handler) *CampaignPhasesHandler {
	return &CampaignPhasesHandler{repo: repo, auth: auth, activity: recorder}
}

func (h *CampaignPhasesHandler) Register(app *fiber.App) {
	g := app.Group("/api/campaigns")
	g.Get("/:id/phases", h.auth, h.Get)
	g.Put("/:id/phases", h.auth, h.Put)
	g.Delete("/:id/phases", h.auth, h.Reset)
}

// campaignPhasePlan is a campaign's phase date plan.
type campaignPhasePlan struct {
	CampaignID     string `json:"campaign_id"`
	CampaignTypeID string `json:"campaign_type_id"`
	// Source is derived (even split of the campaign dates), manual (user-edited)
	// or unscheduled (the campaign has no dates, so the phases have none).
	Source     campaignphase.Source `json:"source" enums:"derived,manual,unscheduled"`
	TypeLocked bool                 `json:"type_locked"`
	Phases     []campaignPhaseEntry `json:"phases"`
}

// campaignPhaseEntry is one phase with its inclusive window (YYYY-MM-DD) and
// how many of the campaign's posts are planned against it.
type campaignPhaseEntry struct {
	PhaseID   string  `json:"phase_id"`
	Sequence  int     `json:"sequence"`
	Name      string  `json:"name"`
	Purpose   string  `json:"purpose"`
	StartDate *string `json:"start_date"`
	EndDate   *string `json:"end_date"`
	PostCount int     `json:"post_count"`
}

// campaignPhasePlanRequest replaces the whole manual plan: one window per phase
// of the campaign's type.
type campaignPhasePlanRequest struct {
	Phases []campaignPhaseWindowInput `json:"phases"`
}

type campaignPhaseWindowInput struct {
	PhaseID   string `json:"phase_id"   example:"98"`
	StartDate string `json:"start_date" example:"2026-10-01"`
	EndDate   string `json:"end_date"   example:"2026-10-15"`
}

// Get godoc
// @Summary      Get campaign phase plan
// @Description  Returns each phase of the campaign's type with its inclusive date window and post count (CON-166).
// @Description  With no stored plan the windows are derived: the campaign's start/end dates split evenly across
// @Description  the phases (remainder days to the earliest) — the same windows content generation uses.
// @Description  source is "unscheduled" (and the dates null) while the campaign has no dates.
// @Tags         campaigns
// @Produce      json
// @Security     CookieAuth
// @Param        id   path      string  true  "Campaign Sqid"
// @Success      200  {object}  campaignPhasePlan
// @Failure      401  {object}  map[string]string
// @Failure      404  {object}  map[string]string
// @Router       /api/campaigns/{id}/phases [get]
func (h *CampaignPhasesHandler) Get(c *fiber.Ctx) error {
	campaign, err := h.repo.GetByID(reqCtx(c), c.Params("id"))
	if err != nil {
		return notFound(err, "campaign not found")
	}
	return h.respond(c, campaign)
}

// Put godoc
// @Summary      Set campaign phase plan
// @Description  Replaces the campaign's phase windows with a manual plan (CON-166). The plan must hold exactly one
// @Description  window per phase of the campaign's type, contiguous in phase order (each starts the day after the
// @Description  previous ends — no gaps or overlaps), the first starting on start_date and the last ending on
// @Description  end_date. Content generation then plans posts into these windows. 400 invalid_phase_plan names
// @Description  the first offending phase; 409 campaign_unscheduled when the campaign has no dates.
// @Tags         campaigns
// @Accept       json
// @Produce      json
// @Security     CookieAuth
// @Param        id    path      string                    true  "Campaign Sqid"
// @Param        body  body      campaignPhasePlanRequest  true  "Whole phase plan"
// @Success      200   {object}  campaignPhasePlan
// @Failure      400   {object}  map[string]string
// @Failure      401   {object}  map[string]string
// @Failure      404   {object}  map[string]string
// @Failure      409   {object}  map[string]string
// @Router       /api/campaigns/{id}/phases [put]
func (h *CampaignPhasesHandler) Put(c *fiber.Ctx) error {
	var req campaignPhasePlanRequest
	if err := c.BodyParser(&req); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, err.Error())
	}
	campaign, err := h.repo.GetByID(reqCtx(c), c.Params("id"))
	if err != nil {
		return notFound(err, "campaign not found")
	}
	if campaign.StartDate == nil || campaign.EndDate == nil {
		return rejectCoded(c, fiber.StatusConflict, codeCampaignUnscheduled, campaignphase.ErrUnscheduled.Error(), nil)
	}

	entries := make([]campaignphase.Entry, 0, len(req.Phases))
	for _, in := range req.Phases {
		start, serr := time.Parse(time.DateOnly, in.StartDate)
		end, eerr := time.Parse(time.DateOnly, in.EndDate)
		if serr != nil || eerr != nil {
			return rejectCoded(c, fiber.StatusBadRequest, codeInvalidPhasePlan,
				"phase "+in.PhaseID+": start_date and end_date must be YYYY-MM-DD", nil)
		}
		entries = append(entries, campaignphase.Entry{PhaseID: in.PhaseID, Start: start, End: end})
	}
	windows, err := campaignphase.Validate(campaign, entries)
	if err != nil {
		if pe, ok := errors.AsType[*campaignphase.PlanError](err); ok {
			return rejectCoded(c, fiber.StatusBadRequest, codeInvalidPhasePlan, pe.Msg, nil)
		}
		return err
	}

	rows := make([]models.CampaignPhaseWindow, len(windows))
	for i, w := range windows {
		rows[i] = models.CampaignPhaseWindow{PhaseID: w.Phase.ID, StartDate: w.Start, EndDate: w.End}
	}
	if err := h.repo.ReplacePhaseWindows(reqCtx(c), campaign.ID, rows); err != nil {
		return err
	}
	h.activity.Record(reqCtx(c), activity.CategoryCampaign, "campaign_phase_plan_updated",
		activity.WithSource(activity.SourceAPI), activity.WithEntity("campaign", campaign.ID))
	return h.reload(c, campaign.ID)
}

// Reset godoc
// @Summary      Reset campaign phase plan
// @Description  Drops the campaign's manual phase plan so the windows are derived from its dates again (CON-166).
// @Description  Idempotent. Returns the resulting (derived) plan.
// @Tags         campaigns
// @Produce      json
// @Security     CookieAuth
// @Param        id   path      string  true  "Campaign Sqid"
// @Success      200  {object}  campaignPhasePlan
// @Failure      401  {object}  map[string]string
// @Failure      404  {object}  map[string]string
// @Router       /api/campaigns/{id}/phases [delete]
func (h *CampaignPhasesHandler) Reset(c *fiber.Ctx) error {
	campaign, err := h.repo.GetByID(reqCtx(c), c.Params("id"))
	if err != nil {
		return notFound(err, "campaign not found")
	}
	if len(campaign.PhaseWindows) == 0 {
		return h.respond(c, campaign)
	}
	if err := h.repo.DeletePhaseWindows(reqCtx(c), campaign.ID); err != nil {
		return err
	}
	h.activity.Record(reqCtx(c), activity.CategoryCampaign, "campaign_phase_plan_reset",
		activity.WithSource(activity.SourceAPI), activity.WithEntity("campaign", campaign.ID))
	return h.reload(c, campaign.ID)
}

func (h *CampaignPhasesHandler) reload(c *fiber.Ctx, id string) error {
	campaign, err := h.repo.GetByID(reqCtx(c), id)
	if err != nil {
		return notFound(err, "campaign not found")
	}
	return h.respond(c, campaign)
}

func (h *CampaignPhasesHandler) respond(c *fiber.Ctx, campaign *models.Campaign) error {
	counts, err := h.repo.PhasePostCounts(reqCtx(c), campaign.ID)
	if err != nil {
		return err
	}
	return c.JSON(buildPhasePlan(campaign, counts))
}

// buildPhasePlan renders a hydrated campaign's effective phase plan.
func buildPhasePlan(campaign *models.Campaign, counts map[string]int) campaignPhasePlan {
	windows, source := campaignphase.Resolve(campaign)
	byPhase := make(map[string]campaignphase.Window, len(windows))
	for _, w := range windows {
		byPhase[w.Phase.ID] = w
	}
	phases := campaignphase.SortedPhases(campaign)
	out := campaignPhasePlan{
		CampaignID:     campaign.ID,
		CampaignTypeID: campaign.CampaignTypeID,
		Source:         source,
		TypeLocked:     campaign.TypeLocked,
		Phases:         make([]campaignPhaseEntry, 0, len(phases)),
	}
	for _, ph := range phases {
		e := campaignPhaseEntry{
			PhaseID:   ph.ID,
			Sequence:  ph.Sequence,
			Name:      ph.Name,
			Purpose:   ph.Purpose,
			PostCount: counts[ph.ID],
		}
		if w, ok := byPhase[ph.ID]; ok {
			s, en := w.Start.Format(time.DateOnly), w.End.Format(time.DateOnly)
			e.StartDate, e.EndDate = &s, &en
		}
		out.Phases = append(out.Phases, e)
	}
	return out
}

// maintainPhasePlan keeps a campaign's stored manual phase plan consistent
// after a whole-record update (CON-166): a type change drops it (the phases are
// different), a date change re-anchors its outer bounds or — when a window
// would empty out — drops it back to derived. Reports whether the plan was
// dropped. Best-effort: a stale plan is ignored at read time anyway
// (campaignphase.Resolve falls back to derived), so a failure here is logged,
// not surfaced.
func maintainPhasePlan(c *fiber.Ctx, repo repository.CampaignRepository, campaign *models.Campaign, typeChanged, datesChanged bool) bool {
	stored := campaign.PhaseWindows
	if len(stored) == 0 || (!typeChanged && !datesChanged) {
		return false
	}
	ctx := reqCtx(c)
	if !typeChanged {
		if entries, ok := campaignphase.Rebound(campaign, stored); ok {
			rows := make([]models.CampaignPhaseWindow, len(entries))
			for i, e := range entries {
				rows[i] = models.CampaignPhaseWindow{PhaseID: e.PhaseID, StartDate: e.Start, EndDate: e.End}
			}
			if err := repo.ReplacePhaseWindows(ctx, campaign.ID, rows); err != nil {
				slog.WarnContext(ctx, "re-anchoring campaign phase plan failed",
					logging.AttrComponent, "campaigns", "campaign_id", campaign.ID, logging.AttrError, err)
			}
			return false
		}
	}
	if err := repo.DeletePhaseWindows(ctx, campaign.ID); err != nil {
		slog.WarnContext(ctx, "dropping campaign phase plan failed",
			logging.AttrComponent, "campaigns", "campaign_id", campaign.ID, logging.AttrError, err)
	}
	slog.InfoContext(ctx, "campaign phase plan reset to derived",
		logging.AttrComponent, "campaigns", "campaign_id", campaign.ID,
		"type_changed", typeChanged, "dates_changed", datesChanged)
	return true
}
