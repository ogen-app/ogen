// CON-308: ModelConfigAdminService — the operator-facing (Harbor) gRPC surface
// for choosing which foundation model each genkit flow uses, keyed by
// (tier, flow, slot). Mirrors PlatformAdminService and shares the same
// bearer-token gate (see server.go). Generated stubs come from gen/modelconfig/v1
// (the buf.build/ogen-app/proto module; `make proto`).
//
// The flow/slot catalog and the model catalog + prices are code-owned (the
// modelconfig catalog + the CON-86 vendor registry) — ListFlows / ListModels
// expose them read-only; only the assignment is persisted. After every write it
// refreshes the in-process resolver so an operator edit takes effect without
// waiting for the periodic tick.
package server

import (
	"context"
	"errors"
	"expvar"
	"log/slog"
	"sort"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	modelconfigv1 "github.com/ogen-app/ogen/gen/modelconfig/v1"
	"github.com/ogen-app/ogen/src/domain/modelconfig"
	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/genkit/modelprobe"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/infra/vendors"
	"github.com/ogen-app/ogen/src/infra/vendors/llm"
	"github.com/ogen-app/ogen/src/kernel/logging"
)

// expvar counters (CON-308 §12). Distinct "ogen_model_config_admin_*" namespace.
var (
	modelConfigAdminSlotSet     = expvar.NewInt("ogen_model_config_admin_slot_set")
	modelConfigAdminSlotCleared = expvar.NewInt("ogen_model_config_admin_slot_cleared")
)

// modelProber runs the CON-308 §8a live compatibility probe for a candidate
// model. Satisfied by modelprobe.Runner; an interface so tests can inject a fake
// and the service stays nil-safe (a nil prober = static-only TestSlotModel).
type modelProber interface {
	Probe(ctx context.Context, flowKey, slotKey, modelID string) (sample string, latencyMs int64, err error)
}

type modelConfigAdminService struct {
	modelconfigv1.UnimplementedModelConfigAdminServiceServer
	repo   repository.FlowModelConfigRepository
	prober modelProber
}

func newModelConfigAdminService(repo repository.FlowModelConfigRepository, prober modelProber) *modelConfigAdminService {
	return &modelConfigAdminService{repo: repo, prober: prober}
}

// --- code-owned catalogs (read-only) ---

func (s *modelConfigAdminService) ListFlows(_ context.Context, _ *modelconfigv1.ListFlowsRequest) (*modelconfigv1.ListFlowsResponse, error) {
	flows := modelconfig.Flows()
	out := make([]*modelconfigv1.Flow, 0, len(flows))
	for _, f := range flows {
		slots := make([]*modelconfigv1.FlowSlot, 0, len(f.Slots))
		for _, sl := range f.Slots {
			slots = append(slots, &modelconfigv1.FlowSlot{
				Key:         sl.Key,
				Description: sl.Description,
				Capability:  string(sl.Capability),
				GlobalOnly:  sl.GlobalOnly,
			})
		}
		out = append(out, &modelconfigv1.Flow{Key: f.Key, Description: f.Description, Slots: slots})
	}
	return &modelconfigv1.ListFlowsResponse{Flows: out}, nil
}

func (s *modelConfigAdminService) ListModels(_ context.Context, req *modelconfigv1.ListModelsRequest) (*modelconfigv1.ListModelsResponse, error) {
	filter := strings.TrimSpace(req.GetCapability())
	var out []*modelconfigv1.Model
	for _, d := range vendors.ByFamily(vendors.FamilyModel) {
		for modelID, rates := range d.Prices.Models {
			caps, _ := vendors.CapabilitiesOf(d.Name, modelID)
			if !assignable(d.Name, caps) {
				continue // v1: a non-Anthropic chat model fills no slot — omit it
			}
			if filter != "" && string(caps.Capability) != filter {
				continue
			}
			pbRates := make([]*modelconfigv1.ModelRate, 0, len(rates))
			for kind, rate := range rates {
				pbRates = append(pbRates, &modelconfigv1.ModelRate{Kind: string(kind), MicrosPerMillion: rate})
			}
			sort.Slice(pbRates, func(i, j int) bool { return pbRates[i].Kind < pbRates[j].Kind })
			out = append(out, &modelconfigv1.Model{
				Id:           modelID,
				Vendor:       d.Name,
				Capability:   string(caps.Capability),
				PriceVersion: d.Prices.Version,
				Rates:        pbRates,
				Capabilities: &modelconfigv1.ModelCapabilities{
					Tools:            caps.Tools,
					StructuredOutput: caps.StructuredOutput,
					Streaming:        caps.Streaming,
					MaxOutputTokens:  int32(caps.MaxOutputTokens),
					ContextWindow:    int32(caps.ContextWindow),
					EmbedDims:        int32(caps.EmbedDims),
					// A model surfaced from the registry is registered + assignable;
					// per-key liveness is verified at assign time by TestSlotModel.
					Live: true,
				},
			})
		}
	}
	// Stable order (vendor, id) so the Harbor dropdown doesn't reshuffle.
	sort.Slice(out, func(i, j int) bool {
		if out[i].Vendor != out[j].Vendor {
			return out[i].Vendor < out[j].Vendor
		}
		return out[i].Id < out[j].Id
	})
	return &modelconfigv1.ListModelsResponse{Models: out}, nil
}

// --- assignments (read) ---

func (s *modelConfigAdminService) ListConfig(ctx context.Context, req *modelconfigv1.ListConfigRequest) (*modelconfigv1.ListConfigResponse, error) {
	rows, err := s.repo.List(ctx)
	if err != nil {
		return nil, s.internal(ctx, "list config", err)
	}
	filter := strings.TrimSpace(req.GetTierId())
	out := make([]*modelconfigv1.SlotAssignment, 0, len(rows))
	for i := range rows {
		if filter != "" && scopeTier(&rows[i]) != filter {
			continue
		}
		out = append(out, toAssignmentProto(&rows[i]))
	}
	return &modelconfigv1.ListConfigResponse{Assignments: out}, nil
}

func (s *modelConfigAdminService) GetEffectiveConfig(ctx context.Context, req *modelconfigv1.GetEffectiveConfigRequest) (*modelconfigv1.GetEffectiveConfigResponse, error) {
	tierID := strings.TrimSpace(req.GetTierId())
	if tierID == "" {
		return nil, status.Error(codes.InvalidArgument, "tier_id is required")
	}
	rows, err := s.repo.List(ctx)
	if err != nil {
		return nil, s.internal(ctx, "get effective config", err)
	}
	global := map[string]string{}
	tier := map[string]string{}
	for i := range rows {
		key := rows[i].FlowKey + "\x00" + rows[i].SlotKey
		switch scopeTier(&rows[i]) {
		case "":
			global[key] = rows[i].ModelID
		case tierID:
			tier[key] = rows[i].ModelID
		}
	}
	var out []*modelconfigv1.ResolvedSlot
	for _, sr := range modelconfig.AllSlots() {
		key := sr.FlowKey + "\x00" + sr.Slot.Key
		// The embed (global-only) slot never honours a tier override.
		if m, ok := tier[key]; ok && !sr.Slot.GlobalOnly {
			out = append(out, &modelconfigv1.ResolvedSlot{FlowKey: sr.FlowKey, SlotKey: sr.Slot.Key, ModelId: m, FromTierOverride: true})
			continue
		}
		if m, ok := global[key]; ok {
			out = append(out, &modelconfigv1.ResolvedSlot{FlowKey: sr.FlowKey, SlotKey: sr.Slot.Key, ModelId: m, FromTierOverride: false})
		}
	}
	return &modelconfigv1.GetEffectiveConfigResponse{Slots: out}, nil
}

// --- assignments (write) ---

func (s *modelConfigAdminService) SetSlotModel(ctx context.Context, req *modelconfigv1.SetSlotModelRequest) (*modelconfigv1.SetSlotModelResponse, error) {
	flowKey := strings.TrimSpace(req.GetFlowKey())
	slotKey := strings.TrimSpace(req.GetSlotKey())
	slot, ok := modelconfig.LookupSlot(flowKey, slotKey)
	if !ok {
		return nil, status.Errorf(codes.InvalidArgument, "unknown flow/slot %q/%q", flowKey, slotKey)
	}
	tierID := strings.TrimSpace(req.GetTierId())
	if tierID != "" && slot.GlobalOnly {
		return nil, status.Errorf(codes.FailedPrecondition, "slot %s/%s is global-only; it cannot have a per-tier override", flowKey, slotKey)
	}
	modelID := strings.TrimSpace(req.GetModelId())
	if modelID == "" {
		return nil, status.Error(codes.InvalidArgument, "model_id is required")
	}
	if verr := validateSlotModel(slot, modelID); verr != nil {
		return nil, verr
	}

	var tierPtr *string
	if tierID != "" {
		tierPtr = &tierID
	}
	cfg := &models.FlowModelConfig{TierID: tierPtr, FlowKey: flowKey, SlotKey: slotKey, ModelID: modelID, UpdatedBy: "harbor"}
	if err := s.repo.Upsert(ctx, cfg); err != nil {
		if pgCode(err) == pgFKViolation {
			return nil, status.Errorf(codes.FailedPrecondition, "no such tier %q", tierID)
		}
		return nil, s.internal(ctx, "set slot model", err)
	}
	if rerr := modelconfig.Refresh(ctx); rerr != nil {
		slog.WarnContext(ctx, "model config refresh after set failed", logging.AttrComponent, "grpcserver", logging.AttrError, rerr)
	}
	got, err := s.repo.GetByScope(ctx, tierPtr, flowKey, slotKey)
	if err != nil {
		return nil, s.internal(ctx, "set slot model", err)
	}
	modelConfigAdminSlotSet.Add(1)
	slog.InfoContext(ctx, "model slot set", logging.AttrComponent, "grpcserver",
		"scope", scopeLabel(tierID), "flow", flowKey, "slot", slotKey, "model", modelID)
	return &modelconfigv1.SetSlotModelResponse{Assignment: toAssignmentProto(got)}, nil
}

func (s *modelConfigAdminService) ClearSlotModel(ctx context.Context, req *modelconfigv1.ClearSlotModelRequest) (*modelconfigv1.ClearSlotModelResponse, error) {
	tierID := strings.TrimSpace(req.GetTierId())
	if tierID == "" {
		return nil, status.Error(codes.InvalidArgument, "tier_id is required; a global default cannot be cleared")
	}
	flowKey := strings.TrimSpace(req.GetFlowKey())
	slotKey := strings.TrimSpace(req.GetSlotKey())
	deleted, err := s.repo.Delete(ctx, &tierID, flowKey, slotKey)
	if err != nil {
		return nil, s.internal(ctx, "clear slot model", err)
	}
	if deleted {
		if rerr := modelconfig.Refresh(ctx); rerr != nil {
			slog.WarnContext(ctx, "model config refresh after clear failed", logging.AttrComponent, "grpcserver", logging.AttrError, rerr)
		}
		modelConfigAdminSlotCleared.Add(1)
		slog.InfoContext(ctx, "model slot cleared", logging.AttrComponent, "grpcserver", "tier", tierID, "flow", flowKey, "slot", slotKey)
	}
	return &modelconfigv1.ClearSlotModelResponse{}, nil
}

// --- verification (CON-308 §8a) ---

// TestSlotModel runs the static requirements match, then (if it passes) the
// live golden-probe — a real model call that catches what static checks cannot:
// a missing/rotated key, an unregistered plugin, an unknown model, region
// gating. A static failure short-circuits (no billed call).
func (s *modelConfigAdminService) TestSlotModel(ctx context.Context, req *modelconfigv1.TestSlotModelRequest) (*modelconfigv1.TestSlotModelResponse, error) {
	flowKey := strings.TrimSpace(req.GetFlowKey())
	slotKey := strings.TrimSpace(req.GetSlotKey())
	slot, ok := modelconfig.LookupSlot(flowKey, slotKey)
	if !ok {
		return nil, status.Errorf(codes.InvalidArgument, "unknown flow/slot %q/%q", flowKey, slotKey)
	}
	modelID := strings.TrimSpace(req.GetModelId())
	if modelID == "" {
		return nil, status.Error(codes.InvalidArgument, "model_id is required")
	}
	slog.InfoContext(ctx, "model test requested", logging.AttrComponent, "grpcserver",
		"flow", flowKey, "slot", slotKey, "model", modelID)
	if unmet := unmetForSlot(slot, modelID); len(unmet) > 0 {
		slog.InfoContext(ctx, "model test: static requirements not met", logging.AttrComponent, "grpcserver",
			"flow", flowKey, "slot", slotKey, "model", modelID, "unmet", unmet)
		return &modelconfigv1.TestSlotModelResponse{Passed: false, Detail: "static requirements not met", UnmetRequirements: unmet}, nil
	}
	if s.prober == nil {
		slog.InfoContext(ctx, "model test: static passed (live probe unavailable)", logging.AttrComponent, "grpcserver",
			"flow", flowKey, "slot", slotKey, "model", modelID)
		return &modelconfigv1.TestSlotModelResponse{Passed: true, Detail: "static checks passed (live probe unavailable)"}, nil
	}
	sample, latency, err := s.prober.Probe(ctx, flowKey, slotKey, modelID)
	switch {
	case errors.Is(err, modelprobe.ErrProbeUnsupported):
		slog.InfoContext(ctx, "model test: static passed (no live probe for this slot)", logging.AttrComponent, "grpcserver",
			"flow", flowKey, "slot", slotKey, "model", modelID)
		return &modelconfigv1.TestSlotModelResponse{Passed: true, Detail: "static checks passed (no live probe for this slot)"}, nil
	case err != nil:
		slog.WarnContext(ctx, "model test failed", logging.AttrComponent, "grpcserver",
			"flow", flowKey, "slot", slotKey, "model", modelID, "latency_ms", latency, logging.AttrError, err)
		return &modelconfigv1.TestSlotModelResponse{Passed: false, Detail: err.Error(), LatencyMs: latency}, nil
	default:
		slog.InfoContext(ctx, "model test passed", logging.AttrComponent, "grpcserver",
			"flow", flowKey, "slot", slotKey, "model", modelID, "latency_ms", latency)
		return &modelconfigv1.TestSlotModelResponse{Passed: true, Detail: "ok", LatencyMs: latency, Sample: sample}, nil
	}
}

// --- helpers ---

// validateSlotModel enforces the §8/§8a write rules and returns a gRPC error.
func validateSlotModel(slot modelconfig.Slot, modelID string) error {
	if unmet := unmetForSlot(slot, modelID); len(unmet) > 0 {
		return status.Errorf(codes.InvalidArgument, "model %q cannot fill slot %s: %s", modelID, slot.Key, strings.Join(unmet, ", "))
	}
	return nil
}

// unmetForSlot lists why a model can't fill a slot: unknown model, capability
// family mismatch, the v1 Anthropic-only-chat rule, and each unmet requirement.
func unmetForSlot(slot modelconfig.Slot, modelID string) []string {
	vendor, ok := vendors.VendorOf(modelID)
	if !ok {
		return []string{"unknown model (not in the vendor registry)"}
	}
	caps, _ := vendors.CapabilitiesOf(vendor, modelID)
	var unmet []string
	if caps.Capability != slot.Capability {
		unmet = append(unmet, "capability: needs "+string(slot.Capability)+", model is "+string(caps.Capability))
	}
	if slot.Capability == modelconfig.CapabilityChat && vendor != llm.VendorAnthropic {
		unmet = append(unmet, "vendor: chat slots accept only Anthropic models in v1")
	}
	unmet = append(unmet, modelconfig.Satisfies(caps, slot.Requires)...)
	return unmet
}

// assignable reports whether a model can be assigned to any slot today, so
// ListModels can omit models no slot would accept. It mirrors the v1 chat rule
// in unmetForSlot: Provider.CallConfig is Anthropic-shaped, so a non-Anthropic
// chat model fills no chat slot (and no other slot), and is dropped. Embed
// models and Anthropic chat models are assignable.
func assignable(vendor string, caps modelconfig.ModelCapabilities) bool {
	if caps.Capability == modelconfig.CapabilityChat && vendor != llm.VendorAnthropic {
		return false
	}
	return true
}

func scopeTier(c *models.FlowModelConfig) string {
	if c.TierID == nil {
		return ""
	}
	return *c.TierID
}

func scopeLabel(tierID string) string {
	if tierID == "" {
		return "global"
	}
	return "tier:" + tierID
}

func toAssignmentProto(c *models.FlowModelConfig) *modelconfigv1.SlotAssignment {
	return &modelconfigv1.SlotAssignment{
		TierId:    scopeTier(c),
		FlowKey:   c.FlowKey,
		SlotKey:   c.SlotKey,
		ModelId:   c.ModelID,
		UpdatedAt: timestamppb.New(c.UpdatedAt),
	}
}

func (s *modelConfigAdminService) internal(ctx context.Context, op string, err error) error {
	slog.ErrorContext(ctx, "model config admin "+op, logging.AttrComponent, "grpcserver", logging.AttrError, err)
	return status.Error(codes.Internal, op+" failed")
}
