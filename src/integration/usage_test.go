//go:build integration

package integration_test

import (
	"context"
	"expvar"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/database"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/infra/vendors"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
	"github.com/ogen-app/ogen/src/kernel/usage"
)

// itestVendor is a dedicated priced model vendor for these specs, registered
// once into the process-global registry (unique name, no collision with the
// anthropic/gemini/zernio vendors the binary registers transitively).
func init() {
	vendors.Register(vendors.Descriptor{
		Name:   "itest-vendor",
		Family: vendors.FamilyModel,
		Prices: vendors.PriceTable{Version: "itest", Models: map[string]vendors.Rates{
			"m": {vendors.KindInput: 3_000_000}, // $3 / 1M input
		}},
	})
}

// freshMetrics returns counters that are NOT registered on the global expvar
// map, so each spec can assert counts without expvar.NewInt's duplicate-name
// panic across specs.
func freshMetrics() *usage.Metrics {
	return &usage.Metrics{
		EventsRecorded:      &expvar.Int{},
		EventsDropped:       &expvar.Int{},
		WriteErrors:         &expvar.Int{},
		UnknownModel:        &expvar.Int{},
		UnknownVendor:       &expvar.Int{},
		LimitBlocks:         &expvar.Int{},
		LimitWarnings:       &expvar.Int{},
		EnforcementDegraded: &expvar.Int{},
	}
}

func seedTenant(db *bun.DB, id, name, slug string) {
	GinkgoHelper()
	_, err := db.NewInsert().Model(&models.Tenant{ID: id, Name: name, Slug: slug, TierID: models.DefaultTierID}).Exec(context.Background())
	Expect(err).NotTo(HaveOccurred())
}

// mkEvent builds a vendor_usage_events row with an explicit tenant. The recorder writes
// these in a system context where BeforeAppendModel preserves the set TenantID
// (models/tenant_scoped.go), which the test mirrors via insertEvents.
func mkEvent(tenantID, vendor, family, model, op, feature string, inputTokens, cost int64, at time.Time) *models.UsageEvent {
	GinkgoHelper()
	id, err := models.NewID()
	Expect(err).NotTo(HaveOccurred())
	e := &models.UsageEvent{
		ID:          id,
		Vendor:      vendor,
		Family:      family,
		Model:       model,
		Operation:   op,
		Feature:     feature,
		InputTokens: inputTokens,
		CostMicros:  cost,
		OccurredAt:  at,
	}
	e.TenantID = tenantID
	return e
}

var _ = Describe("Usage metering (CON-86)", Ordered, func() {
	var (
		db     *bun.DB
		events repository.UsageRepository
		limits repository.UsageLimitsRepository
	)

	insertEvents := func(evs ...*models.UsageEvent) {
		GinkgoHelper()
		// System context: each row already carries its tenant_id.
		Expect(events.Insert(tenantctx.WithSystem(context.Background()), evs)).To(Succeed())
	}
	asTenant := func(id string) context.Context { return tenantctx.With(context.Background(), id) }

	BeforeAll(func() {
		db = mustOpenIntegrationDB()
		// vendor_usage_events lives in the analytics migration set (the main migrations
		// pgtest already ran created tenant_usage_limits). On vanilla Postgres the
		// guarded TimescaleDB DDL is skipped and vendor_usage_events is a plain table.
		Expect(database.MigrateAnalytics(context.Background(), db)).To(Succeed())
		events = repository.NewUsageRepository(db)
		limits = repository.NewUsageLimitsRepository(db)
		// tenant_usage_limits has an FK to tenants; seed the ones the limit specs use.
		seedTenant(db, "tn-a", "Tenant A", "tenant-a")
		seedTenant(db, "tn-b", "Tenant B", "tenant-b")
		seedTenant(db, "tn-c", "Tenant C", "tenant-c")
		seedTenant(db, "tn-d", "Tenant D", "tenant-d")
	})

	Describe("vendor_usage_events repository", func() {
		It("records and reads period-to-date spend + a grouped breakdown", func() {
			now := time.Now().UTC()
			at := now.Add(-time.Minute)
			insertEvents(
				mkEvent("t-rw", "anthropic", "model", "m1", "generate", "content_plan", 1000, 3600, at),
				mkEvent("t-rw", "anthropic", "model", "m1", "generate", "content_plan", 500, 1800, at),
			)

			ctx := asTenant("t-rw")
			spend, err := events.SpendBetween(ctx, now.Add(-time.Hour), now.Add(time.Hour))
			Expect(err).NotTo(HaveOccurred())
			Expect(spend).To(Equal(int64(5400)))

			rows, err := events.Summary(ctx, now.Add(-time.Hour), now.Add(time.Hour))
			Expect(err).NotTo(HaveOccurred())
			Expect(rows).To(HaveLen(1)) // same vendor/model/op/feature group
			Expect(rows[0].CostMicros).To(Equal(int64(5400)))
			Expect(rows[0].InputTokens).To(Equal(int64(1500)))
			Expect(rows[0].Count).To(Equal(int64(2)))
		})

		It("isolates spend across tenants (AC7)", func() {
			now := time.Now().UTC()
			at := now.Add(-time.Minute)
			insertEvents(
				mkEvent("t-iso-a", "anthropic", "model", "m1", "generate", "content_plan", 0, 1000, at),
				mkEvent("t-iso-b", "anthropic", "model", "m1", "generate", "content_plan", 0, 2000, at),
			)

			s, w := now.Add(-time.Hour), now.Add(time.Hour)
			aSpend, err := events.SpendBetween(asTenant("t-iso-a"), s, w)
			Expect(err).NotTo(HaveOccurred())
			bSpend, err := events.SpendBetween(asTenant("t-iso-b"), s, w)
			Expect(err).NotTo(HaveOccurred())

			// Each tenant sees only its own events — never the other's.
			Expect(aSpend).To(Equal(int64(1000)))
			Expect(bSpend).To(Equal(int64(2000)))
		})

		It("surfaces long-tail extra_units per group (embed_input etc.)", func() {
			now := time.Now().UTC()
			at := now.Add(-time.Minute)
			// Two embedding events in the same group: the embed tokens live in
			// extra_units, not input_tokens — the summary must still expose them.
			e1 := mkEvent("t-extra", "gemini", "model", "gemini-embedding-2", "embed", "pdf_extract", 0, 2514, at)
			e1.ExtraUnits = map[string]int64{"embed_input": 16764}
			e2 := mkEvent("t-extra", "gemini", "model", "gemini-embedding-2", "embed", "pdf_extract", 0, 1000, at)
			e2.ExtraUnits = map[string]int64{"embed_input": 5000}
			insertEvents(e1, e2)

			rows, err := events.Summary(asTenant("t-extra"), now.Add(-time.Hour), now.Add(time.Hour))
			Expect(err).NotTo(HaveOccurred())
			Expect(rows).To(HaveLen(1))
			Expect(rows[0].InputTokens).To(Equal(int64(0)))                              // embeds aren't "input"
			Expect(rows[0].CostMicros).To(Equal(int64(3514)))                            // 2514 + 1000
			Expect(rows[0].ExtraUnits).To(HaveKeyWithValue("embed_input", int64(21764))) // 16764 + 5000
		})
	})

	Describe("async Recorder end-to-end", func() {
		It("snapshots cost and persists one row, attributed to the tenant", func() {
			m := freshMetrics()
			rec := usage.NewRecorder(events, m, usage.Config{})
			rec.Record(asTenant("t-rec"), "itest-vendor", "content_plan", vendors.MeterEvent{
				Model:     "m",
				Operation: "generate",
				Usage:     vendors.Usage{vendors.KindInput: 1_000_000},
			})
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			Expect(rec.Close(ctx)).To(Succeed())
			Expect(m.EventsRecorded.Value()).To(Equal(int64(1)))

			now := time.Now().UTC()
			spend, err := events.SpendBetween(asTenant("t-rec"), now.Add(-time.Hour), now.Add(time.Hour))
			Expect(err).NotTo(HaveOccurred())
			Expect(spend).To(Equal(int64(3_000_000))) // 1M input @ $3/1M
		})
	})

	Describe("tenant_usage_limits + enforcement", func() {
		It("upserts a single row per tenant and reads it back", func() {
			ctx := asTenant("tn-c")
			Expect(limits.Upsert(ctx, &models.TenantUsageLimit{
				DailyCapMicros: new(int64(500)), Mode: models.LimitModeWarn, Enabled: true,
			})).To(Succeed())
			// Update (same tenant) — still one row, new values.
			Expect(limits.Upsert(ctx, &models.TenantUsageLimit{
				DailyCapMicros: new(int64(900)), Mode: models.LimitModeEnforce, Enabled: true,
			})).To(Succeed())

			row, err := limits.GetByTenant(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(row).NotTo(BeNil())
			Expect(*row.DailyCapMicros).To(Equal(int64(900)))
			Expect(row.Mode).To(Equal(models.LimitModeEnforce))

			// Another tenant with no row → nil (isolation).
			other, err := limits.GetByTenant(asTenant("tn-d"))
			Expect(err).NotTo(HaveOccurred())
			Expect(other).To(BeNil())
		})

		It("blocks in enforce mode once over a cap, allows under it (AC5)", func() {
			now := time.Now().UTC()
			at := now.Add(-time.Minute)

			// tn-a: cap 1000, spend 1500 → blocked.
			Expect(limits.Upsert(asTenant("tn-a"), &models.TenantUsageLimit{
				DailyCapMicros: new(int64(1000)), Mode: models.LimitModeEnforce, Enabled: true,
			})).To(Succeed())
			insertEvents(mkEvent("tn-a", "itest-vendor", "model", "m", "generate", "content_plan", 0, 1500, at))

			chk := usage.NewChecker(limits, events, usage.Defaults{}, freshMetrics(), 0)
			d := chk.Check(asTenant("tn-a"))
			Expect(d.Blocked).To(BeTrue())
			Expect(d.Period).To(Equal("day"))
			Expect(d.SpentMicros).To(Equal(int64(1500)))

			// tn-b: cap 1000, spend 500 → allowed.
			Expect(limits.Upsert(asTenant("tn-b"), &models.TenantUsageLimit{
				DailyCapMicros: new(int64(1000)), Mode: models.LimitModeEnforce, Enabled: true,
			})).To(Succeed())
			insertEvents(mkEvent("tn-b", "itest-vendor", "model", "m", "generate", "content_plan", 0, 500, at))

			d = usage.NewChecker(limits, events, usage.Defaults{}, freshMetrics(), 0).Check(asTenant("tn-b"))
			Expect(d.Blocked).To(BeFalse())
		})

		It("resolves the effective-limit source tenant → default → unlimited (AC6)", func() {
			// tn-c has a row (from the upsert spec) → source tenant.
			eff, _, _, err := usage.NewChecker(limits, events, usage.Defaults{}, freshMetrics(), 0).
				EffectiveLimits(asTenant("tn-c"))
			Expect(err).NotTo(HaveOccurred())
			Expect(eff.Source).To(Equal("tenant"))

			// tn-d has no row; with config defaults set → source default.
			eff, _, _, err = usage.NewChecker(limits, events,
				usage.Defaults{DailyCapMicros: 5000, Mode: models.LimitModeEnforce}, freshMetrics(), 0).
				EffectiveLimits(asTenant("tn-d"))
			Expect(err).NotTo(HaveOccurred())
			Expect(eff.Source).To(Equal("default"))

			// tn-d, no row + no defaults → source unlimited.
			eff, _, _, err = usage.NewChecker(limits, events, usage.Defaults{}, freshMetrics(), 0).
				EffectiveLimits(asTenant("tn-d"))
			Expect(err).NotTo(HaveOccurred())
			Expect(eff.Source).To(Equal("unlimited"))
		})

		It("fails open when the analytics DB is unreachable (AC8)", func() {
			// A closed pool as the spend reader makes SpendBetween error.
			badDB := mustOpenIntegrationDB()
			Expect(badDB.Close()).To(Succeed())
			badSpend := repository.NewUsageRepository(badDB)

			// tn-a has an enforce cap (from the enforce spec); the limits read
			// succeeds but the spend read fails, so the gate must allow.
			m := freshMetrics()
			chk := usage.NewChecker(limits, badSpend, usage.Defaults{}, m, 0)
			d := chk.Check(asTenant("tn-a"))
			Expect(d.Blocked).To(BeFalse())
			Expect(m.EnforcementDegraded.Value()).To(Equal(int64(1)))
		})
	})
})
