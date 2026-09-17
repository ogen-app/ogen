package handlers

import (
	"errors"

	"github.com/gofiber/fiber/v2"

	"github.com/ogen-app/ogen/src/usecase/activity/report"
)

// ActivityHandler owns the Activity daily-report read endpoints (CON-285): a
// full single-day report and a keyset list of non-empty days, both computed
// server-side from live post/campaign data for the caller's local day. It is the
// backend the Activity UI (CON-225) renders instead of the browser-side
// computation CON-225 §5 first specified — hence the required tz. A nil service
// leaves both endpoints at 503.
type ActivityHandler struct {
	reports *report.Service
	auth    fiber.Handler
}

// NewActivityHandler wires the Activity read endpoints.
func NewActivityHandler(reports *report.Service, auth fiber.Handler) *ActivityHandler {
	return &ActivityHandler{reports: reports, auth: auth}
}

// Register mounts the routes. /report/:date and /reports are distinct segments,
// so neither shadows the other.
func (h *ActivityHandler) Register(app *fiber.App) {
	app.Get("/api/activity/report/:date", h.auth, h.Report)
	app.Get("/api/activity/reports", h.auth, h.Reports)
}

// Report godoc
// @Summary      Activity daily report
// @Description  Deterministic counts of what happened on a local calendar day in
// @Description  the active workspace (CON-285): posts published (by channel),
// @Description  failed/not-published (by channel + per-post detail), created (by
// @Description  author) and campaigns created. Computed server-side, so tz is
// @Description  required. A future or empty day returns a zeroed 200.
// @Tags         activity
// @Produce      json
// @Security     CookieAuth
// @Param        date         path   string  true   "Local calendar day (YYYY-MM-DD)"
// @Param        tz           query  string  true   "IANA time zone, e.g. America/New_York"
// @Param        campaign_id  query  string  false  "Narrow to one campaign"
// @Success      200  {object}  report.Report
// @Failure      400  {object}  map[string]string
// @Failure      401  {object}  map[string]string
// @Failure      404  {object}  map[string]string
// @Failure      503  {object}  map[string]string
// @Router       /api/activity/report/{date} [get]
func (h *ActivityHandler) Report(c *fiber.Ctx) error {
	if h.reports == nil {
		return fiber.NewError(fiber.StatusServiceUnavailable, "activity reports are not available")
	}
	out, err := h.reports.Report(c.Context(), c.Params("date"), c.Query("tz"), c.Query("campaign_id"))
	if err != nil {
		return mapReportErr(err)
	}
	return c.JSON(out)
}

// Reports godoc
// @Summary      Activity report list
// @Description  Non-empty days newest-first (CON-285), keyset-paginated by date
// @Description  via `before`. Each item carries the four headline totals — enough
// @Description  for a feed row without fetching each day's detail.
// @Tags         activity
// @Produce      json
// @Security     CookieAuth
// @Param        tz           query  string  true   "IANA time zone"
// @Param        campaign_id  query  string  false  "Narrow to one campaign"
// @Param        before       query  string  false  "Keyset cursor: only days before this YYYY-MM-DD"
// @Param        limit        query  int     false  "Page size, default 30, max 100"
// @Success      200  {object}  report.ReportList
// @Failure      400  {object}  map[string]string
// @Failure      401  {object}  map[string]string
// @Failure      404  {object}  map[string]string
// @Failure      503  {object}  map[string]string
// @Router       /api/activity/reports [get]
func (h *ActivityHandler) Reports(c *fiber.Ctx) error {
	if h.reports == nil {
		return fiber.NewError(fiber.StatusServiceUnavailable, "activity reports are not available")
	}
	limit, err := limitParam(c)
	if err != nil {
		return err
	}
	out, err := h.reports.Reports(c.Context(), c.Query("tz"), c.Query("campaign_id"), c.Query("before"), limit)
	if err != nil {
		return mapReportErr(err)
	}
	return c.JSON(out)
}

// mapReportErr turns the report package's sentinel errors into HTTP statuses.
func mapReportErr(err error) error {
	switch {
	case errors.Is(err, report.ErrInvalidTZ):
		return fiber.NewError(fiber.StatusBadRequest, "invalid tz")
	case errors.Is(err, report.ErrInvalidDate):
		return fiber.NewError(fiber.StatusBadRequest, "invalid date")
	case errors.Is(err, report.ErrCampaignNotFound):
		return fiber.NewError(fiber.StatusNotFound, "campaign not found")
	default:
		return err
	}
}
