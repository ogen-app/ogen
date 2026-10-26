package server

import (
	"context"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	billingv1 "github.com/ogen-app/ogen/gen/billing/v1"
	"github.com/ogen-app/ogen/src/usecase/billing"
)

func (s *billingAdminService) ListProviderVariants(ctx context.Context, req *billingv1.ListProviderVariantsRequest) (*billingv1.ListProviderVariantsResponse, error) {
	variants, err := s.svc.ProviderVariants(ctx, req.GetTestMode())
	if err != nil {
		return nil, billingStatusErr(err)
	}
	out := &billingv1.ListProviderVariantsResponse{}
	for _, v := range variants {
		out.Variants = append(out.Variants, toProviderVariantProto(v))
	}
	return out, nil
}

func (s *billingAdminService) ListVariantMappings(ctx context.Context, req *billingv1.ListVariantMappingsRequest) (*billingv1.ListVariantMappingsResponse, error) {
	list, err := s.svc.VariantMappingList(ctx, req.GetTestMode())
	if err != nil {
		return nil, billingStatusErr(err)
	}
	out := &billingv1.ListVariantMappingsResponse{TestMode: list.TestMode, Checked: list.Checked}
	for _, m := range list.Mappings {
		out.Mappings = append(out.Mappings, toVariantMappingProto(m))
	}
	return out, nil
}

func (s *billingAdminService) SetVariantMapping(ctx context.Context, req *billingv1.SetVariantMappingRequest) (*billingv1.SetVariantMappingResponse, error) {
	priceID := strings.TrimSpace(req.GetTierVersionPriceId())
	if priceID == "" {
		return nil, status.Error(codes.InvalidArgument, "tier_version_price_id is required")
	}
	m, err := s.svc.SetVariantMapping(ctx, priceID, req.GetTestMode(), req.GetVariantId(), req.GetReason())
	if err != nil {
		return nil, billingStatusErr(err)
	}
	return &billingv1.SetVariantMappingResponse{Mapping: toVariantMappingProto(m)}, nil
}

func (s *billingAdminService) SuggestVariantMappings(ctx context.Context, req *billingv1.SuggestVariantMappingsRequest) (*billingv1.SuggestVariantMappingsResponse, error) {
	suggestions, err := s.svc.SuggestVariantMappings(ctx, req.GetTestMode())
	if err != nil {
		return nil, billingStatusErr(err)
	}
	out := &billingv1.SuggestVariantMappingsResponse{}
	for _, sg := range suggestions {
		out.Suggestions = append(out.Suggestions, &billingv1.VariantSuggestion{TierVersionPriceId: sg.PriceID, Variant: toProviderVariantProto(sg.Variant)})
	}
	return out, nil
}

func toProviderVariantProto(v billing.ProviderVariant) *billingv1.ProviderVariant {
	return &billingv1.ProviderVariant{
		Id: v.ID, ProductId: v.ProductID, ProductName: v.ProductName, Name: v.Name,
		PriceMinor: v.PriceMinor, Currency: v.Currency, Interval: v.Interval,
		HasFreeTrial: v.HasFreeTrial, TrialInterval: v.TrialInterval, TrialIntervalCount: int32(v.TrialIntervalCount),
		Status: v.Status, TestMode: v.TestMode, MappedPriceId: v.MappedPriceID,
	}
}

func toVariantMappingProto(m billing.VariantMapping) *billingv1.VariantMapping {
	p := &billingv1.VariantMapping{
		TierVersionPriceId: m.Price.ID, TierId: m.Price.TierID, TierName: m.Price.TierName,
		TierVersionId: m.Price.TierVersionID, TierVersion: int32(m.Price.TierVersion),
		Interval: m.Price.Interval, NetMinor: m.Price.NetMinor, Currency: m.Price.Currency, CountryCode: m.Price.CountryCode,
		FreeTrialTier: m.FreeTrialTier, VariantId: m.VariantID, Status: m.Status, Message: m.Message,
	}
	if m.Variant != nil {
		p.Variant = toProviderVariantProto(*m.Variant)
	}
	if m.Row != nil {
		p.UpdatedAt, p.UpdatedBy = timestamppb.New(m.Row.UpdatedAt), m.Row.UpdatedBy
	}
	return p
}
