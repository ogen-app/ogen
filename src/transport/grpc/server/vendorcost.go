package server

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	vendorcostv1 "github.com/ogen-app/ogen/gen/vendorcost/v1"
	"github.com/ogen-app/ogen/src/infra/anthropicadmin"
	"github.com/ogen-app/ogen/src/kernel/logging"
)

// maxVendorCostDays is the longest range one GetAnthropicCost call covers,
// days inclusive: enough for a previous and a current month.
const maxVendorCostDays = 92

// AnthropicCosts reads what Anthropic charged per day.
// *anthropicadmin.Client satisfies it.
type AnthropicCosts interface {
	DailyCosts(ctx context.Context, from, to time.Time) ([]anthropicadmin.DailyCost, error)
}

// registerVendorCost wires vendorcost.v1 VendorCostService. With no reader
// (nil) the service answers Unimplemented.
func registerVendorCost(srv *grpc.Server, anthropic AnthropicCosts) {
	if anthropic == nil {
		return
	}
	vendorcostv1.RegisterVendorCostServiceServer(srv, &vendorCostService{anthropic: anthropic, now: time.Now})
}

type vendorCostService struct {
	vendorcostv1.UnimplementedVendorCostServiceServer
	anthropic AnthropicCosts
	now       func() time.Time
}

func (s *vendorCostService) GetAnthropicCost(ctx context.Context, req *vendorcostv1.GetAnthropicCostRequest) (*vendorcostv1.GetAnthropicCostResponse, error) {
	from, to, err := parseCostRange(req.GetFromDate(), req.GetToDate())
	if err != nil {
		return nil, err
	}
	costs, err := s.anthropic.DailyCosts(ctx, from, to)
	fetchedAt := timestamppb.New(s.now())
	switch {
	case errors.Is(err, anthropicadmin.ErrNotConfigured):
		return &vendorcostv1.GetAnthropicCostResponse{Configured: false, FetchedAt: fetchedAt}, nil
	case errors.Is(err, anthropicadmin.ErrUnauthorized):
		return nil, status.Error(codes.FailedPrecondition, "Anthropic rejected anthropic_admin_api_key; it must be an Admin key (sk-ant-admin…), not an API key")
	case errors.Is(err, anthropicadmin.ErrForbidden):
		return nil, status.Error(codes.FailedPrecondition, "anthropic_admin_api_key may not read the cost report; check the key's organization role and that the organization has the Admin API")
	case errors.Is(err, anthropicadmin.ErrUnavailable), errors.Is(err, context.DeadlineExceeded):
		slog.WarnContext(ctx, "anthropic cost report unavailable", logging.AttrComponent, "grpcserver", logging.AttrError, err)
		return nil, status.Error(codes.Unavailable, "Anthropic cost report is unavailable; retry later")
	case errors.Is(err, context.Canceled):
		return nil, status.Error(codes.Canceled, "request canceled")
	case err != nil:
		slog.ErrorContext(ctx, "anthropic cost report failed", logging.AttrComponent, "grpcserver", logging.AttrError, err)
		return nil, status.Error(codes.Internal, "read Anthropic cost report")
	}
	out := make([]*vendorcostv1.DailyCost, 0, len(costs))
	for _, c := range costs {
		out = append(out, &vendorcostv1.DailyCost{Date: c.Date, Model: c.Model, CostType: c.CostType, CostUsdMicros: c.USDMicros})
	}
	return &vendorcostv1.GetAnthropicCostResponse{Configured: true, Costs: out, FetchedAt: fetchedAt}, nil
}

func parseCostRange(fromStr, toStr string) (time.Time, time.Time, error) {
	from, err := time.Parse(time.DateOnly, fromStr)
	if err != nil {
		return time.Time{}, time.Time{}, status.Errorf(codes.InvalidArgument, "from_date %q is not YYYY-MM-DD", fromStr)
	}
	to, err := time.Parse(time.DateOnly, toStr)
	if err != nil {
		return time.Time{}, time.Time{}, status.Errorf(codes.InvalidArgument, "to_date %q is not YYYY-MM-DD", toStr)
	}
	if to.Before(from) {
		return time.Time{}, time.Time{}, status.Error(codes.InvalidArgument, "from_date is after to_date")
	}
	if to.Sub(from) >= maxVendorCostDays*24*time.Hour {
		return time.Time{}, time.Time{}, status.Errorf(codes.InvalidArgument, "the range covers more than %d days", maxVendorCostDays)
	}
	return from, to, nil
}
