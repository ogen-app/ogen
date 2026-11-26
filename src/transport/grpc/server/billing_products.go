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

func (s *billingAdminService) ListProviderProducts(ctx context.Context, req *billingv1.ListProviderProductsRequest) (*billingv1.ListProviderProductsResponse, error) {
	products, err := s.svc.ProviderProducts(ctx, req.GetTestMode())
	if err != nil {
		return nil, billingStatusErr(err)
	}
	out := &billingv1.ListProviderProductsResponse{}
	for _, p := range products {
		out.Products = append(out.Products, toProviderProductProto(p))
	}
	return out, nil
}

func (s *billingAdminService) ListProductMappings(ctx context.Context, req *billingv1.ListProductMappingsRequest) (*billingv1.ListProductMappingsResponse, error) {
	list, err := s.svc.ProductMappingList(ctx, req.GetTestMode())
	if err != nil {
		return nil, billingStatusErr(err)
	}
	out := &billingv1.ListProductMappingsResponse{TestMode: list.TestMode, Checked: list.Checked}
	for _, m := range list.Mappings {
		out.Mappings = append(out.Mappings, toProductMappingProto(m))
	}
	return out, nil
}

func (s *billingAdminService) SetProductMapping(ctx context.Context, req *billingv1.SetProductMappingRequest) (*billingv1.SetProductMappingResponse, error) {
	priceID := strings.TrimSpace(req.GetTierVersionPriceId())
	if priceID == "" {
		return nil, status.Error(codes.InvalidArgument, "tier_version_price_id is required")
	}
	m, err := s.svc.SetProductMapping(ctx, priceID, req.GetTestMode(), req.GetProductId(), req.GetReason())
	if err != nil {
		return nil, billingStatusErr(err)
	}
	return &billingv1.SetProductMappingResponse{Mapping: toProductMappingProto(m)}, nil
}

func (s *billingAdminService) SuggestProductMappings(ctx context.Context, req *billingv1.SuggestProductMappingsRequest) (*billingv1.SuggestProductMappingsResponse, error) {
	suggestions, err := s.svc.SuggestProductMappings(ctx, req.GetTestMode())
	if err != nil {
		return nil, billingStatusErr(err)
	}
	out := &billingv1.SuggestProductMappingsResponse{}
	for _, sg := range suggestions {
		out.Suggestions = append(out.Suggestions, &billingv1.ProductSuggestion{TierVersionPriceId: sg.PriceID, Product: toProviderProductProto(sg.Product)})
	}
	return out, nil
}

func toProviderProductProto(p billing.ProviderProduct) *billingv1.ProviderProduct {
	out := &billingv1.ProviderProduct{
		Id: p.ID, Name: p.Name, Interval: p.Interval, IntervalCount: int32(max(p.IntervalCount, 1)),
		HasFreeTrial: p.HasFreeTrial, TrialInterval: p.TrialInterval, TrialIntervalCount: int32(p.TrialIntervalCount),
		Archived: p.Archived, TestMode: p.TestMode, MappedPriceIds: p.MappedPriceIDs,
	}
	for _, pr := range p.Prices {
		out.Prices = append(out.Prices, &billingv1.ProductPrice{Currency: pr.Currency, AmountMinor: pr.AmountMinor})
	}
	return out
}

func toProductMappingProto(m billing.ProductMappingView) *billingv1.ProductMapping {
	p := &billingv1.ProductMapping{
		TierVersionPriceId: m.Price.ID, TierId: m.Price.TierID, TierName: m.Price.TierName,
		TierVersionId: m.Price.TierVersionID, TierVersion: int32(m.Price.TierVersion),
		Interval: m.Price.Interval, NetMinor: m.Price.NetMinor, Currency: m.Price.Currency, CountryCode: m.Price.CountryCode,
		FreeTrialTier: m.FreeTrialTier, ProductId: m.ProductID, Status: m.Status, Message: m.Message,
	}
	if m.Product != nil {
		p.Product = toProviderProductProto(*m.Product)
	}
	if m.Row != nil {
		p.UpdatedAt, p.UpdatedBy = timestamppb.New(m.Row.UpdatedAt), m.Row.UpdatedBy
	}
	return p
}
