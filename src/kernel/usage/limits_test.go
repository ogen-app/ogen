package usage_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/kernel/usage"
)

func ptr(v int64) *int64 { return &v }

type fakeLimits struct {
	row *models.TenantUsageLimit
	err error
}

func (f fakeLimits) GetByTenant(context.Context) (*models.TenantUsageLimit, error) {
	return f.row, f.err
}

type fakeSpend struct {
	mu    sync.Mutex
	vals  []int64 // returned in call order: [daySpent, monthSpent]
	calls int
	err   error
}

func (s *fakeSpend) SpendBetween(context.Context, time.Time, time.Time) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return 0, s.err
	}
	v := int64(0)
	if s.calls < len(s.vals) {
		v = s.vals[s.calls]
	}
	s.calls++
	return v, nil
}

func (s *fakeSpend) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func newChecker(t *testing.T, lim fakeLimits, sp *fakeSpend, def usage.Defaults) (*usage.Checker, *usage.Metrics) {
	t.Helper()
	m := testMetrics()
	return usage.NewChecker(lim, sp, def, m, 0), m
}

func TestChecker_EnforceOverDayCapBlocks(t *testing.T) {
	row := &models.TenantUsageLimit{DailyCapMicros: ptr(1000), Mode: models.LimitModeEnforce, Enabled: true}
	c, m := newChecker(t, fakeLimits{row: row}, &fakeSpend{vals: []int64{1000, 0}}, usage.Defaults{})

	d := c.Check(tenantCtx("tA"))
	if !d.Blocked || d.Period != "day" || d.CapMicros != 1000 || d.SpentMicros != 1000 {
		t.Fatalf("decision = %+v, want blocked day cap 1000 spent 1000", d)
	}
	if m.LimitBlocks.Value() != 1 {
		t.Errorf("LimitBlocks = %d, want 1", m.LimitBlocks.Value())
	}
}

func TestChecker_WarnOverProceeds(t *testing.T) {
	row := &models.TenantUsageLimit{DailyCapMicros: ptr(1000), Mode: models.LimitModeWarn, Enabled: true}
	c, m := newChecker(t, fakeLimits{row: row}, &fakeSpend{vals: []int64{2000, 0}}, usage.Defaults{})

	d := c.Check(tenantCtx("tA"))
	if d.Blocked {
		t.Fatalf("warn mode should not block, got %+v", d)
	}
	if m.LimitWarnings.Value() != 1 {
		t.Errorf("LimitWarnings = %d, want 1", m.LimitWarnings.Value())
	}
}

func TestChecker_UnderCapAllows(t *testing.T) {
	row := &models.TenantUsageLimit{DailyCapMicros: ptr(1000), Mode: models.LimitModeEnforce, Enabled: true}
	c, _ := newChecker(t, fakeLimits{row: row}, &fakeSpend{vals: []int64{500, 0}}, usage.Defaults{})

	if d := c.Check(tenantCtx("tA")); d.Blocked {
		t.Fatalf("under cap should allow, got %+v", d)
	}
}

func TestChecker_MonthCapBlocksWhenDayUnderOrUnset(t *testing.T) {
	row := &models.TenantUsageLimit{MonthlyCapMicros: ptr(1000), Mode: models.LimitModeEnforce, Enabled: true}
	c, _ := newChecker(t, fakeLimits{row: row}, &fakeSpend{vals: []int64{500, 1000}}, usage.Defaults{})

	d := c.Check(tenantCtx("tA"))
	if !d.Blocked || d.Period != "month" {
		t.Fatalf("decision = %+v, want blocked month", d)
	}
}

func TestChecker_DisabledRowAllows(t *testing.T) {
	row := &models.TenantUsageLimit{DailyCapMicros: ptr(1), Mode: models.LimitModeEnforce, Enabled: false}
	c, _ := newChecker(t, fakeLimits{row: row}, &fakeSpend{vals: []int64{100, 100}}, usage.Defaults{})

	if d := c.Check(tenantCtx("tA")); d.Blocked {
		t.Fatalf("disabled limits should allow, got %+v", d)
	}
}

func TestChecker_DefaultsWhenNoRow(t *testing.T) {
	c, _ := newChecker(t, fakeLimits{row: nil},
		&fakeSpend{vals: []int64{1000, 0}},
		usage.Defaults{DailyCapMicros: 1000, Mode: models.LimitModeEnforce})

	d := c.Check(tenantCtx("tA"))
	if !d.Blocked || d.Period != "day" {
		t.Fatalf("decision = %+v, want blocked via default day cap", d)
	}

	eff, _, _, err := c.EffectiveLimits(tenantCtx("tA"))
	if err != nil {
		t.Fatalf("EffectiveLimits err: %v", err)
	}
	if eff.Source != "default" {
		t.Errorf("source = %q, want default", eff.Source)
	}
}

func TestChecker_UnlimitedWhenNoRowNoDefaults(t *testing.T) {
	c, _ := newChecker(t, fakeLimits{row: nil}, &fakeSpend{vals: []int64{1 << 40, 1 << 40}}, usage.Defaults{})

	if d := c.Check(tenantCtx("tA")); d.Blocked {
		t.Fatalf("no row + no defaults should be unlimited, got %+v", d)
	}
	eff, _, _, err := c.EffectiveLimits(tenantCtx("tA"))
	if err != nil {
		t.Fatalf("EffectiveLimits err: %v", err)
	}
	if eff.Source != "unlimited" {
		t.Errorf("source = %q, want unlimited", eff.Source)
	}
}

func TestChecker_FailOpenOnSpendError(t *testing.T) {
	row := &models.TenantUsageLimit{DailyCapMicros: ptr(1), Mode: models.LimitModeEnforce, Enabled: true}
	c, m := newChecker(t, fakeLimits{row: row}, &fakeSpend{err: errors.New("analytics down")}, usage.Defaults{})

	if d := c.Check(tenantCtx("tA")); d.Blocked {
		t.Fatalf("should fail open on spend error, got %+v", d)
	}
	if m.EnforcementDegraded.Value() != 1 {
		t.Errorf("EnforcementDegraded = %d, want 1", m.EnforcementDegraded.Value())
	}
}

func TestChecker_FailOpenOnLimitsError(t *testing.T) {
	c, m := newChecker(t, fakeLimits{err: errors.New("control plane down")}, &fakeSpend{vals: []int64{0, 0}}, usage.Defaults{})

	if d := c.Check(tenantCtx("tA")); d.Blocked {
		t.Fatalf("should fail open on limits error, got %+v", d)
	}
	if m.EnforcementDegraded.Value() != 1 {
		t.Errorf("EnforcementDegraded = %d, want 1", m.EnforcementDegraded.Value())
	}
}

func TestChecker_CachesWithinTTL(t *testing.T) {
	row := &models.TenantUsageLimit{DailyCapMicros: ptr(10_000), Mode: models.LimitModeEnforce, Enabled: true}
	sp := &fakeSpend{vals: []int64{500, 0}}
	c, _ := newChecker(t, fakeLimits{row: row}, sp, usage.Defaults{})

	c.Check(tenantCtx("tA"))
	c.Check(tenantCtx("tA"))

	// Two SpendBetween calls on the first Check (day + month); the second
	// Check is served from the TTL cache.
	if got := sp.callCount(); got != 2 {
		t.Errorf("SpendBetween calls = %d, want 2 (second Check cached)", got)
	}
}

func TestChecker_NilAllows(t *testing.T) {
	var c *usage.Checker
	if d := c.Check(tenantCtx("tA")); d.Blocked {
		t.Fatalf("nil checker should allow, got %+v", d)
	}
	if err := c.Enforce(tenantCtx("tA")); err != nil {
		t.Errorf("nil Enforce = %v, want nil", err)
	}
}

func TestChecker_EnforceReturnsLimitError(t *testing.T) {
	row := &models.TenantUsageLimit{DailyCapMicros: ptr(1000), Mode: models.LimitModeEnforce, Enabled: true}
	c, _ := newChecker(t, fakeLimits{row: row}, &fakeSpend{vals: []int64{1000, 0}}, usage.Defaults{})

	err := c.Enforce(tenantCtx("tA"))
	lim, ok := errors.AsType[*usage.LimitExceededError](err)
	if !ok {
		t.Fatalf("Enforce err = %v, want *LimitExceededError", err)
	}
	if lim.Period != "day" || lim.CapMicros != 1000 {
		t.Errorf("LimitExceededError = %+v, want day cap 1000", lim.Decision)
	}
}

func TestResolve(t *testing.T) {
	t.Run("tenant row wins", func(t *testing.T) {
		row := &models.TenantUsageLimit{DailyCapMicros: ptr(500), Mode: models.LimitModeWarn, Enabled: true}
		eff := usage.Resolve(row, usage.Defaults{DailyCapMicros: 999})
		if eff.Source != "tenant" || eff.Mode != models.LimitModeWarn || eff.DailyCapMicros == nil || *eff.DailyCapMicros != 500 {
			t.Fatalf("resolve(tenant) = %+v", eff)
		}
	})
	t.Run("config default when no row", func(t *testing.T) {
		eff := usage.Resolve(nil, usage.Defaults{DailyCapMicros: 100, Mode: models.LimitModeEnforce})
		if eff.Source != "default" || eff.DailyCapMicros == nil || *eff.DailyCapMicros != 100 {
			t.Fatalf("resolve(default) = %+v", eff)
		}
	})
	t.Run("unlimited when no row and no caps", func(t *testing.T) {
		eff := usage.Resolve(nil, usage.Defaults{})
		if eff.Source != "unlimited" || eff.DailyCapMicros != nil || eff.MonthlyCapMicros != nil {
			t.Fatalf("resolve(unlimited) = %+v", eff)
		}
		if eff.Mode != models.LimitModeEnforce {
			t.Errorf("empty default mode should fall back to enforce, got %q", eff.Mode)
		}
	})
}

func TestChecker_UntenantedNotGated(t *testing.T) {
	row := &models.TenantUsageLimit{DailyCapMicros: ptr(1), Mode: models.LimitModeEnforce, Enabled: true}
	c, _ := newChecker(t, fakeLimits{row: row}, &fakeSpend{vals: []int64{100, 100}}, usage.Defaults{})

	if d := c.Check(t.Context()); d.Blocked {
		t.Fatalf("untenanted call must not be gated, got %+v", d)
	}
}
