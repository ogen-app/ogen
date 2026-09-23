package modelconfig

import (
	"context"
	"expvar"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/kernel/logging"
)

// Source is the persistence the resolver needs: read every assignment and seed
// missing global-default rows. Satisfied by repository.FlowModelConfigRepository.
type Source interface {
	List(ctx context.Context) ([]models.FlowModelConfig, error)
	Upsert(ctx context.Context, cfg *models.FlowModelConfig) error
}

// VendorFunc maps a model id to its vendor slug (satisfied by vendors.VendorOf).
// The domain resolver takes it as a dependency so it needn't import infra.
type VendorFunc func(model string) (vendor string, ok bool)

// TierFunc returns the caller's tier id from context (empty/false = no tier, so
// resolution uses the global default). Satisfied by tenantctx.TierFrom, or a
// closure that also looks the tier up from the tenant.
type TierFunc func(ctx context.Context) (tierID string, ok bool)

// Defaults are the boot-reconcile seed models for the global-default rows,
// sourced from the legacy config fields so day-one behaviour is unchanged
// (CON-308 §10.2).
type Defaults struct {
	Generation string // chat generation flows (incl. post_assistant writer)
	Quality    string // post_quality
	Planning   string // post_assistant planner + campaign_assistant orchestrator
	Embed      string // embed
}

// For maps a (flow, slot) to its legacy default model id.
func (d Defaults) For(flowKey, slotKey string) string {
	switch {
	case flowKey == FlowEmbed:
		return d.Embed
	case flowKey == FlowPostQuality:
		return d.Quality
	case flowKey == FlowPostAssistant && slotKey == SlotPlanner:
		return d.Planning
	case flowKey == FlowCampaignAssistant && slotKey == SlotOrchestrator:
		return d.Planning
	default:
		return d.Generation
	}
}

const refreshInterval = 60 * time.Second

type entry struct {
	vendor string
	model  string
}

// snapshot is an immutable, atomically-swapped view of the assignment table.
type snapshot struct {
	scoped map[string]entry // key: tier \x00 flow \x00 slot ; tier "" == global
}

var (
	active atomic.Pointer[snapshot]

	// Set once by Init before the refresh goroutine and any serving, so plain
	// access is race-free (mirrors platforms.InitGlobalLimits).
	cfgSource   Source
	cfgVendor   VendorFunc
	cfgTier     TierFunc
	cfgFallback map[Capability]entry // capability -> default entry, the last-resort value

	refreshOK    = expvar.NewInt("ogen_model_config_refresh_ok")
	refreshFail  = expvar.NewInt("ogen_model_config_refresh_fail")
	unconfigured = expvar.NewInt("ogen_model_config_unconfigured_slot")
)

func scopeKey(tier, flow, slot string) string {
	return tier + "\x00" + flow + "\x00" + slot
}

// Init reconciles missing global defaults from def, loads the assignment
// snapshot from src, and starts a periodic refresh bound to ctx. Non-fatal: a
// failed load logs and the resolver serves capability defaults so the app still
// starts (CON-308 §10.4).
func Init(ctx context.Context, src Source, def Defaults, vendorOf VendorFunc, tierOf TierFunc) {
	cfgSource = src
	cfgVendor = vendorOf
	cfgTier = tierOf
	cfgFallback = buildFallback(def, vendorOf)

	if err := reconcile(ctx, src, def); err != nil {
		slog.ErrorContext(ctx, "model config reconcile failed; global defaults may be incomplete",
			logging.AttrComponent, "modelconfig", logging.AttrError, err)
	}
	if err := Refresh(ctx); err != nil {
		slog.ErrorContext(ctx, "initial model config load failed; serving capability defaults",
			logging.AttrComponent, "modelconfig", logging.AttrError, err)
	}
	go func() {
		t := time.NewTicker(refreshInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := Refresh(ctx); err != nil {
					slog.WarnContext(ctx, "model config refresh failed; keeping previous snapshot",
						logging.AttrComponent, "modelconfig", logging.AttrError, err)
				}
			}
		}
	}()
}

func buildFallback(def Defaults, vendorOf VendorFunc) map[Capability]entry {
	mk := func(model string) entry {
		v, _ := vendorOf(model)
		return entry{vendor: v, model: model}
	}
	return map[Capability]entry{
		CapabilityChat:  mk(def.Generation),
		CapabilityEmbed: mk(def.Embed),
	}
}

// reconcile inserts a global-default row for any catalog slot that lacks one,
// seeded from def. It NEVER overwrites an existing row, so an operator's Harbor
// edit (or a prior env-derived seed) survives every redeploy (CON-308 §14).
func reconcile(ctx context.Context, src Source, def Defaults) error {
	rows, err := src.List(ctx)
	if err != nil {
		return err
	}
	existing := make(map[string]bool, len(rows))
	for _, r := range rows {
		if r.TierID == nil {
			existing[r.FlowKey+"\x00"+r.SlotKey] = true
		}
	}
	for _, sr := range AllSlots() {
		if existing[sr.FlowKey+"\x00"+sr.Slot.Key] {
			continue
		}
		model := def.For(sr.FlowKey, sr.Slot.Key)
		if model == "" {
			continue
		}
		if err := src.Upsert(ctx, &models.FlowModelConfig{
			FlowKey: sr.FlowKey, SlotKey: sr.Slot.Key, ModelID: model, UpdatedBy: "boot-reconcile",
		}); err != nil {
			return err
		}
	}
	return nil
}

// Refresh reloads the assignment snapshot now. The gRPC admin writes call it so
// an operator edit takes effect without waiting for the periodic tick. A failed
// reload keeps the previous snapshot.
func Refresh(ctx context.Context) error {
	src := cfgSource
	if src == nil {
		return nil
	}
	rows, err := src.List(ctx)
	if err != nil {
		refreshFail.Add(1)
		return err
	}
	snap := &snapshot{scoped: make(map[string]entry, len(rows))}
	for _, r := range rows {
		tier := ""
		if r.TierID != nil {
			tier = *r.TierID
		}
		v, _ := cfgVendor(r.ModelID)
		snap.scoped[scopeKey(tier, r.FlowKey, r.SlotKey)] = entry{vendor: v, model: r.ModelID}
	}
	active.Store(snap)
	refreshOK.Add(1)
	return nil
}

// resolve applies `tier-override ?? global-default ?? capability-default`. The
// embed slot (and any global-only slot) ignores the caller's tier.
func resolve(ctx context.Context, flowKey, slotKey string) entry {
	slot, haveSlot := LookupSlot(flowKey, slotKey)
	if snap := active.Load(); snap != nil {
		tier := ""
		if cfgTier != nil && !(haveSlot && slot.GlobalOnly) {
			if t, ok := cfgTier(ctx); ok {
				tier = t
			}
		}
		if tier != "" {
			if e, ok := snap.scoped[scopeKey(tier, flowKey, slotKey)]; ok {
				return e
			}
		}
		if e, ok := snap.scoped[scopeKey("", flowKey, slotKey)]; ok {
			return e
		}
	}
	// No configured row (fresh/broken DB, or a slot added in code before the next
	// reconcile). Fall back to the capability default so a flow never gets an
	// empty model.
	unconfigured.Add(1)
	capab := CapabilityChat
	if haveSlot {
		capab = slot.Capability
	}
	return cfgFallback[capab]
}

// Model returns the bare model id for a flow slot (the `model` usage dimension).
func Model(ctx context.Context, flowKey, slotKey string) string {
	return resolve(ctx, flowKey, slotKey).model
}

// Vendor returns the vendor slug of the resolved model for a flow slot — the
// `vendor` usage dimension recorded alongside Model.
func Vendor(ctx context.Context, flowKey, slotKey string) string {
	return resolve(ctx, flowKey, slotKey).vendor
}

// Ref returns the genkit "vendor/model" reference for ai.WithModelName, e.g.
// "anthropic/claude-sonnet-4-5-20250929". The vendor is derived from the model's
// owning descriptor, replacing the fixed "anthropic/" prefix.
func Ref(ctx context.Context, flowKey, slotKey string) string {
	e := resolve(ctx, flowKey, slotKey)
	if e.vendor == "" {
		return e.model
	}
	return e.vendor + "/" + e.model
}
