package handlers

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/usage"
)

// UsageHandler serves the CON-86 read APIs (current consumption, effective
// limits) and the admin write API (set limits). Usage events come from the
// analytics pool (events is nil when ANALYTICS_DSN is empty); the
// tenant_usage_limits config lives in the control-plane DB, so limits
// read/write works regardless. Every query auto-scopes to the caller's tenant
// via the TenantScoped hooks, so one tenant never sees another's data.
type UsageHandler struct {
	events     repository.UsageRepository // nil when analytics disabled
	limits     repository.UsageLimitsRepository
	defaults   usage.Defaults
	auth       fiber.Handler
	adminToken string // gates the write route; empty = write disabled (fail-closed)
}

func NewUsageHandler(
	events repository.UsageRepository,
	limits repository.UsageLimitsRepository,
	defaults usage.Defaults,
	auth fiber.Handler,
	adminToken string,
) *UsageHandler {
	return &UsageHandler{events: events, limits: limits, defaults: defaults, auth: auth, adminToken: adminToken}
}

func (h *UsageHandler) Register(app *fiber.App) {
	g := app.Group("/api/usage")
	g.Get("/", h.auth, h.Summary)
	g.Get("/limits", h.auth, h.GetLimits)
	// Raising a cap or disabling enforcement is an operator action, not tenant
	// self-service — otherwise a tenant could lift its own spend limit. The
	// user model has no admin/owner role yet (CON-86 §15), so the write is
	// gated behind an operator token (X-Admin-Token == USAGE_ADMIN_TOKEN) on
	// top of auth. Empty token fails closed. Operator-only RBAC is the
	// follow-up that replaces this stopgap.
	g.Put("/limits", h.auth, h.requireOperator, h.SetLimits)
}

// requireOperator rejects the request unless USAGE_ADMIN_TOKEN is configured
// and the X-Admin-Token header matches it (constant-time). When unset it fails
// closed, so tenant users can never mutate their own limits.
func (h *UsageHandler) requireOperator(c *fiber.Ctx) error {
	if h.adminToken == "" {
		return fiber.NewError(fiber.StatusForbidden, "setting usage limits is disabled (no operator token configured)")
	}
	got := c.Get("X-Admin-Token")
	if subtle.ConstantTimeCompare([]byte(got), []byte(h.adminToken)) != 1 {
		return fiber.NewError(fiber.StatusForbidden, "operator token required")
	}
	return c.Next()
}

type usageTotals struct {
	InputTokens         int64            `json:"input_tokens"`
	OutputTokens        int64            `json:"output_tokens"`
	CacheReadTokens     int64            `json:"cache_read_tokens"`
	CacheCreationTokens int64            `json:"cache_creation_tokens"`
	ExtraUnits          map[string]int64 `json:"extra_units,omitempty"`
	CostMicros          int64            `json:"cost_micros"`
	USD                 string           `json:"usd"`
	Count               int64            `json:"count"`
}

type usageSummaryResponse struct {
	Period string    `json:"period"`
	From   time.Time `json:"from"`
	To     time.Time `json:"to"`
	// AnalyticsEnabled is false when ANALYTICS_DSN is empty: then the zeros below
	// mean "recording is off", not "no usage". Without this flag the two are
	// indistinguishable. When true, empty totals genuinely mean this tenant has
	// no usage in the window (e.g. you're logged into a different tenant).
	AnalyticsEnabled bool                           `json:"analytics_enabled"`
	Totals           usageTotals                    `json:"totals"`
	Breakdown        []repository.UsageBreakdownRow `json:"breakdown"`
}

// Summary godoc — GET /api/usage?period=day|month&from=&to=&family=
func (h *UsageHandler) Summary(c *fiber.Ctx) error {
	period := c.Query("period", "month")
	if period != "day" && period != "month" {
		return fiber.NewError(fiber.StatusBadRequest, "period must be day or month")
	}

	now := time.Now().UTC()
	dayStart, monthStart := usage.PeriodBounds(now)
	start, end := monthStart, now
	if period == "day" {
		start = dayStart
	}
	if v := c.Query("from"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return fiber.NewError(fiber.StatusBadRequest, "from must be RFC3339")
		}
		start = t.UTC()
	}
	if v := c.Query("to"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return fiber.NewError(fiber.StatusBadRequest, "to must be RFC3339")
		}
		end = t.UTC()
	}

	resp := usageSummaryResponse{
		Period:           period,
		From:             start,
		To:               end,
		AnalyticsEnabled: h.events != nil,
		Breakdown:        []repository.UsageBreakdownRow{},
	}

	// Analytics disabled → no events; return zeros (with analytics_enabled=false)
	// rather than an error so a future UI degrades gracefully.
	if h.events != nil {
		rows, err := h.events.Summary(reqCtx(c), start, end)
		if err != nil {
			return err
		}
		family := c.Query("family")
		for _, r := range rows {
			if family != "" && r.Family != family {
				continue
			}
			resp.Breakdown = append(resp.Breakdown, r)
			resp.Totals.InputTokens += r.InputTokens
			resp.Totals.OutputTokens += r.OutputTokens
			resp.Totals.CacheReadTokens += r.CacheReadTokens
			resp.Totals.CacheCreationTokens += r.CacheCreationTokens
			resp.Totals.CostMicros += r.CostMicros
			resp.Totals.Count += r.Count
			for k, v := range r.ExtraUnits {
				if resp.Totals.ExtraUnits == nil {
					resp.Totals.ExtraUnits = make(map[string]int64)
				}
				resp.Totals.ExtraUnits[k] += v
			}
		}
	}
	resp.Totals.USD = microsUSD(resp.Totals.CostMicros)

	return c.JSON(resp)
}

type usageLimitsResponse struct {
	usage.Effective
	DaySpentMicros   int64 `json:"day_spent_micros"`
	MonthSpentMicros int64 `json:"month_spent_micros"`
}

// GetLimits godoc — GET /api/usage/limits
func (h *UsageHandler) GetLimits(c *fiber.Ctx) error {
	row, err := h.limits.GetByTenant(reqCtx(c))
	if err != nil {
		return err
	}
	resp := usageLimitsResponse{Effective: usage.Resolve(row, h.defaults)}

	if h.events != nil {
		now := time.Now().UTC()
		dayStart, monthStart := usage.PeriodBounds(now)
		if resp.DaySpentMicros, err = h.events.SpendBetween(reqCtx(c), dayStart, now); err != nil {
			return err
		}
		if resp.MonthSpentMicros, err = h.events.SpendBetween(reqCtx(c), monthStart, now); err != nil {
			return err
		}
	}
	return c.JSON(resp)
}

type setLimitsRequest struct {
	DailyCapMicros   *int64  `json:"daily_cap_micros"`
	MonthlyCapMicros *int64  `json:"monthly_cap_micros"`
	Mode             *string `json:"mode"`
	Enabled          *bool   `json:"enabled"`
}

// SetLimits godoc — PUT /api/usage/limits (full replace of the tenant's row).
func (h *UsageHandler) SetLimits(c *fiber.Ctx) error {
	dec := json.NewDecoder(bytes.NewReader(c.Body()))
	dec.DisallowUnknownFields()
	var req setLimitsRequest
	if err := dec.Decode(&req); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, err.Error())
	}

	if req.DailyCapMicros != nil && *req.DailyCapMicros < 0 {
		return fiber.NewError(fiber.StatusBadRequest, "daily_cap_micros must be >= 0")
	}
	if req.MonthlyCapMicros != nil && *req.MonthlyCapMicros < 0 {
		return fiber.NewError(fiber.StatusBadRequest, "monthly_cap_micros must be >= 0")
	}
	mode := models.LimitModeEnforce
	if req.Mode != nil {
		if *req.Mode != models.LimitModeEnforce && *req.Mode != models.LimitModeWarn {
			return fiber.NewError(fiber.StatusBadRequest, "mode must be enforce or warn")
		}
		mode = *req.Mode
	}
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}

	lim := &models.TenantUsageLimit{
		DailyCapMicros:   req.DailyCapMicros,
		MonthlyCapMicros: req.MonthlyCapMicros,
		Mode:             mode,
		Enabled:          enabled,
	}
	if err := h.limits.Upsert(reqCtx(c), lim); err != nil {
		return err
	}
	return c.JSON(usage.Resolve(lim, h.defaults))
}

// microsUSD renders USD-micros as a plain decimal-dollar string, e.g. 3_600 ->
// "0.003600".
func microsUSD(micros int64) string {
	neg := ""
	if micros < 0 {
		neg, micros = "-", -micros
	}
	return fmt.Sprintf("%s%d.%06d", neg, micros/1_000_000, micros%1_000_000)
}
