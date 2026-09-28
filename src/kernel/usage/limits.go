package usage

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
)

// LimitsReader resolves a tenant's configured limit row;
// repository.UsageLimitsRepository satisfies it.
type LimitsReader interface {
	GetByTenant(ctx context.Context) (*models.TenantUsageLimit, error)
}

// SpendReader returns period-to-date spend; repository.UsageRepository
// satisfies it.
type SpendReader interface {
	SpendBetween(ctx context.Context, start, end time.Time) (int64, error)
}

// Defaults are the operator's global fallback caps, applied to
// any tenant without an override row. A zero cap means "no default cap".
type Defaults struct {
	DailyCapMicros   int64
	MonthlyCapMicros int64
	Mode             string // enforce | warn; empty defaults to enforce
}

// Effective is a tenant's resolved limits after applying the precedence
// tenant-row → config-default → unlimited.
type Effective struct {
	DailyCapMicros   *int64 `json:"daily_cap_micros"`
	MonthlyCapMicros *int64 `json:"monthly_cap_micros"`
	Mode             string `json:"mode"`
	Enabled          bool   `json:"enabled"`
	Source           string `json:"source"` // tenant | default | unlimited
}

// Decision is the outcome of an enforcement Check.
type Decision struct {
	Blocked     bool   // true only for mode=enforce already over a cap
	Period      string // "day" | "month" — the cap that triggered
	CapMicros   int64
	SpentMicros int64
	Mode        string
}

// LimitExceededError is returned by Enforce when a synchronous flow is blocked.
// The HTTP layer maps it to 402 with the embedded fields.
type LimitExceededError struct {
	Decision
}

func (e *LimitExceededError) Error() string {
	return fmt.Sprintf("usage_limit_exceeded: %s cap %d micros, spent %d micros (mode %s)",
		e.Period, e.CapMicros, e.SpentMicros, e.Mode)
}

// Checker decides whether a synchronous flow may call a provider, from the
// tenant's effective caps and period-to-date spend. A nil
// *Checker always allows (analytics disabled). Reads are cached per tenant
// for a short TTL to keep the hot path cheap; staleness is bounded and
// tolerated ("block once already over").
type Checker struct {
	limits   LimitsReader
	spend    SpendReader
	defaults Defaults
	metrics  *Metrics

	ttl time.Duration
	now func() time.Time

	mu    sync.Mutex
	cache map[string]cacheEntry
}

type cacheEntry struct {
	eff        Effective
	daySpent   int64
	monthSpent int64
	expiry     time.Time
}

// NewChecker builds a Checker. ttl<=0 defaults to 45s.
func NewChecker(limits LimitsReader, spend SpendReader, defaults Defaults, m *Metrics, ttl time.Duration) *Checker {
	if ttl <= 0 {
		ttl = 45 * time.Second
	}
	if defaults.Mode == "" {
		defaults.Mode = models.LimitModeEnforce
	}
	return &Checker{
		limits:   limits,
		spend:    spend,
		defaults: defaults,
		metrics:  m,
		ttl:      ttl,
		now:      func() time.Time { return time.Now().UTC() },
		cache:    make(map[string]cacheEntry),
	}
}

// Enforce is the flow-facing guard: it returns a *LimitExceededError when the
// tenant is blocked, else nil. Nil-safe (analytics disabled => always nil).
func (c *Checker) Enforce(ctx context.Context) error {
	d := c.Check(ctx)
	if d.Blocked {
		return &LimitExceededError{Decision: d}
	}
	return nil
}

// Check resolves the decision without raising an error. On any analytics read
// failure it fails open (allows) and counts a degraded-mode tick.
func (c *Checker) Check(ctx context.Context) Decision {
	if c == nil {
		return Decision{}
	}
	tenantID, ok := tenantctx.From(ctx)
	if !ok {
		return Decision{} // untenanted work is not gated
	}

	eff, daySpent, monthSpent, err := c.load(ctx, tenantID)
	if err != nil {
		c.metrics.EnforcementDegraded.Add(1)
		return Decision{}
	}
	if !eff.Enabled {
		return Decision{Mode: eff.Mode}
	}

	periods := []struct {
		name  string
		cap   *int64
		spent int64
	}{
		{"day", eff.DailyCapMicros, daySpent},
		{"month", eff.MonthlyCapMicros, monthSpent},
	}

	warned := false
	for _, p := range periods {
		if p.cap == nil || *p.cap <= 0 || p.spent < *p.cap {
			continue
		}
		// Over this cap.
		if eff.Mode == models.LimitModeWarn {
			if !warned {
				c.metrics.LimitWarnings.Add(1)
				warned = true
			}
			continue // warn proceeds
		}
		c.metrics.LimitBlocks.Add(1)
		return Decision{Blocked: true, Period: p.name, CapMicros: *p.cap, SpentMicros: p.spent, Mode: eff.Mode}
	}
	return Decision{Mode: eff.Mode}
}

// EffectiveLimits returns the tenant's resolved limits plus period-to-date
// spend, for GET /api/usage/limits. Uses the same cached read as
// Check.
func (c *Checker) EffectiveLimits(ctx context.Context) (eff Effective, daySpent, monthSpent int64, err error) {
	if c == nil {
		return Effective{Source: "unlimited", Enabled: false, Mode: models.LimitModeEnforce}, 0, 0, nil
	}
	if _, ok := tenantctx.From(ctx); !ok {
		return Effective{}, 0, 0, tenantctx.ErrNoTenant
	}
	return c.load(ctx, mustTenant(ctx))
}

func mustTenant(ctx context.Context) string {
	id, _ := tenantctx.From(ctx)
	return id
}

// load returns the effective limits + day/month spend for tenantID, using a
// per-tenant TTL cache to keep the hot path off the databases.
func (c *Checker) load(ctx context.Context, tenantID string) (Effective, int64, int64, error) {
	now := c.now()

	c.mu.Lock()
	ent, ok := c.cache[tenantID]
	c.mu.Unlock()
	if ok && now.Before(ent.expiry) {
		return ent.eff, ent.daySpent, ent.monthSpent, nil
	}

	eff, err := c.effective(ctx)
	if err != nil {
		return Effective{}, 0, 0, err
	}

	// Reuse PeriodBounds so enforcement and the HTTP summary never drift apart.
	dayStart, monthStart := PeriodBounds(now)

	daySpent, err := c.spend.SpendBetween(ctx, dayStart, now)
	if err != nil {
		return Effective{}, 0, 0, err
	}
	monthSpent, err := c.spend.SpendBetween(ctx, monthStart, now)
	if err != nil {
		return Effective{}, 0, 0, err
	}

	c.mu.Lock()
	c.cache[tenantID] = cacheEntry{eff: eff, daySpent: daySpent, monthSpent: monthSpent, expiry: now.Add(c.ttl)}
	c.mu.Unlock()

	return eff, daySpent, monthSpent, nil
}

// effective applies the precedence tenant-row → config-default → unlimited.
func (c *Checker) effective(ctx context.Context) (Effective, error) {
	row, err := c.limits.GetByTenant(ctx)
	if err != nil {
		return Effective{}, err
	}
	return Resolve(row, c.defaults), nil
}

// Resolve applies the effective-limit precedence tenant-row → config-default →
// unlimited. Pure (no I/O): the HTTP layer reuses it for
// GET /api/usage/limits, where the spend numbers come separately and the
// analytics-backed Checker may be absent.
func Resolve(row *models.TenantUsageLimit, def Defaults) Effective {
	if row != nil {
		return Effective{
			DailyCapMicros:   row.DailyCapMicros,
			MonthlyCapMicros: row.MonthlyCapMicros,
			Mode:             row.Mode,
			Enabled:          row.Enabled,
			Source:           "tenant",
		}
	}

	eff := Effective{Mode: def.Mode, Enabled: true, Source: "default"}
	if eff.Mode == "" {
		eff.Mode = models.LimitModeEnforce
	}
	if def.DailyCapMicros > 0 {
		v := def.DailyCapMicros
		eff.DailyCapMicros = &v
	}
	if def.MonthlyCapMicros > 0 {
		v := def.MonthlyCapMicros
		eff.MonthlyCapMicros = &v
	}
	if eff.DailyCapMicros == nil && eff.MonthlyCapMicros == nil {
		eff.Source = "unlimited"
	}
	return eff
}

// PeriodBounds returns the UTC day-start and month-start for now — the windows
// used for period-to-date spend. Exposed so the HTTP summary uses
// the same boundaries as enforcement.
func PeriodBounds(now time.Time) (dayStart, monthStart time.Time) {
	now = now.UTC()
	dayStart = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	monthStart = time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	return dayStart, monthStart
}
