package modelconfig

import (
	"context"
	"testing"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
)

// fakeSource is an in-memory Source with scope-aware upsert semantics matching
// the DB's COALESCE(tier_id,”) unique index.
type fakeSource struct{ rows []models.FlowModelConfig }

func (f *fakeSource) List(context.Context) ([]models.FlowModelConfig, error) {
	out := make([]models.FlowModelConfig, len(f.rows))
	copy(out, f.rows)
	return out, nil
}

func (f *fakeSource) Upsert(_ context.Context, c *models.FlowModelConfig) error {
	sameTier := func(a, b *string) bool {
		return (a == nil && b == nil) || (a != nil && b != nil && *a == *b)
	}
	for i := range f.rows {
		if sameTier(f.rows[i].TierID, c.TierID) && f.rows[i].FlowKey == c.FlowKey && f.rows[i].SlotKey == c.SlotKey {
			f.rows[i].ModelID = c.ModelID
			return nil
		}
	}
	f.rows = append(f.rows, *c)
	return nil
}

func testVendorOf(model string) (string, bool) {
	switch model {
	case "sonnet", "haiku", "qual", "opus":
		return "anthropic", true
	case "emb":
		return "gemini", true
	default:
		return "", false
	}
}

func testDefaults() Defaults {
	return Defaults{Generation: "sonnet", Quality: "qual", Planning: "haiku", Embed: "emb"}
}

// TestResolverReconcileAndPrecedence covers boot reconcile (every catalog slot
// seeded from the legacy defaults), the "vendor/model" ref, tier-override
// precedence with global fallback, and the embed slot ignoring the tier.
func TestResolverReconcileAndPrecedence(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	src := &fakeSource{}
	Init(ctx, src, testDefaults(), testVendorOf, tenantctx.TierFrom)

	if got := len(src.rows); got != len(AllSlots()) {
		t.Fatalf("reconcile seeded %d rows, want %d", got, len(AllSlots()))
	}

	base := context.Background()
	cases := map[[2]string]string{
		{FlowContentPlan, SlotMain}:               "anthropic/sonnet",
		{FlowPostQuality, SlotMain}:               "anthropic/qual",
		{FlowPostAssistant, SlotPlanner}:          "anthropic/haiku",
		{FlowPostAssistant, SlotWriter}:           "anthropic/sonnet",
		{FlowCampaignAssistant, SlotOrchestrator}: "anthropic/haiku",
		{FlowEmbed, SlotMain}:                     "gemini/emb",
	}
	for k, want := range cases {
		if got := Ref(base, k[0], k[1]); got != want {
			t.Errorf("Ref(%s/%s) = %q, want %q", k[0], k[1], got, want)
		}
	}
	if m := Model(base, FlowContentPlan, SlotMain); m != "sonnet" {
		t.Errorf("Model = %q, want sonnet", m)
	}

	// Tier override wins for its tier; other tiers fall back to the global.
	pro := "pro"
	_ = src.Upsert(ctx, &models.FlowModelConfig{TierID: &pro, FlowKey: FlowContentPlan, SlotKey: SlotMain, ModelID: "opus"})
	if err := Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if got := Ref(tenantctx.WithTier(base, "pro"), FlowContentPlan, SlotMain); got != "anthropic/opus" {
		t.Errorf("pro override Ref = %q, want anthropic/opus", got)
	}
	if got := Ref(tenantctx.WithTier(base, "free"), FlowContentPlan, SlotMain); got != "anthropic/sonnet" {
		t.Errorf("non-overridden tier Ref = %q, want global anthropic/sonnet", got)
	}

	// Embed is global-only: a stray tier row must be ignored.
	_ = src.Upsert(ctx, &models.FlowModelConfig{TierID: &pro, FlowKey: FlowEmbed, SlotKey: SlotMain, ModelID: "opus"})
	if err := Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if got := Ref(tenantctx.WithTier(base, "pro"), FlowEmbed, SlotMain); got != "gemini/emb" {
		t.Errorf("embed global-only violated: Ref = %q, want gemini/emb", got)
	}
}

// TestReconcileDoesNotClobber proves an existing global row (an operator edit or
// a prior env-derived seed) survives reconcile on the next boot.
func TestReconcileDoesNotClobber(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	src := &fakeSource{rows: []models.FlowModelConfig{
		{ID: "x", FlowKey: FlowContentPlan, SlotKey: SlotMain, ModelID: "opus"},
	}}
	Init(ctx, src, testDefaults(), testVendorOf, tenantctx.TierFrom)

	if got := Ref(context.Background(), FlowContentPlan, SlotMain); got != "anthropic/opus" {
		t.Fatalf("reconcile clobbered the existing row: Ref = %q, want anthropic/opus", got)
	}
}
