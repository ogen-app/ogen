package server

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	vendorcostv1 "github.com/ogen-app/ogen/gen/vendorcost/v1"
	"github.com/ogen-app/ogen/src/infra/anthropicadmin"
)

type stubAnthropicCosts struct {
	costs    []anthropicadmin.DailyCost
	err      error
	from, to time.Time
}

func (s *stubAnthropicCosts) DailyCosts(_ context.Context, from, to time.Time) ([]anthropicadmin.DailyCost, error) {
	s.from, s.to = from, to
	return s.costs, s.err
}

func dialVendorCost(t *testing.T, a AnthropicCosts) vendorcostv1.VendorCostServiceClient {
	t.Helper()
	const token = "vendorcost-token"
	srv, err := New(Deps{Token: token, Secrets: newFakeStore(), AnthropicCosts: a})
	if err != nil {
		t.Fatal(err)
	}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithUnaryInterceptor(func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
			return invoker(metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token), method, req, reply, cc, opts...)
		}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return vendorcostv1.NewVendorCostServiceClient(conn)
}

func TestGetAnthropicCost(t *testing.T) {
	stub := &stubAnthropicCosts{costs: []anthropicadmin.DailyCost{
		{Date: "2026-10-01", Model: "", CostType: "web_search", USDMicros: 1_000_000},
		{Date: "2026-10-01", Model: "claude-sonnet-5", CostType: "tokens", USDMicros: 14_345_679},
	}}
	c := dialVendorCost(t, stub)
	resp, err := c.GetAnthropicCost(t.Context(), &vendorcostv1.GetAnthropicCostRequest{FromDate: "2026-09-01", ToDate: "2026-10-31"})
	if err != nil {
		t.Fatal(err)
	}
	if !resp.GetConfigured() || resp.GetFetchedAt() == nil || len(resp.GetCosts()) != 2 {
		t.Fatalf("resp = %v", resp)
	}
	if got := resp.GetCosts()[1]; got.GetDate() != "2026-10-01" || got.GetModel() != "claude-sonnet-5" || got.GetCostType() != "tokens" || got.GetCostUsdMicros() != 14_345_679 {
		t.Fatalf("cost = %v", got)
	}
	if stub.from.Format(time.DateOnly) != "2026-09-01" || stub.to.Format(time.DateOnly) != "2026-10-31" {
		t.Fatalf("range passed = %s .. %s", stub.from, stub.to)
	}
}

func TestGetAnthropicCostNotConfigured(t *testing.T) {
	c := dialVendorCost(t, &stubAnthropicCosts{err: anthropicadmin.ErrNotConfigured})
	resp, err := c.GetAnthropicCost(t.Context(), &vendorcostv1.GetAnthropicCostRequest{FromDate: "2026-10-01", ToDate: "2026-10-01"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetConfigured() || len(resp.GetCosts()) != 0 {
		t.Fatalf("resp = %v, want configured=false and no costs", resp)
	}
}

func TestGetAnthropicCostErrors(t *testing.T) {
	for _, tc := range []struct {
		name     string
		from, to string
		err      error
		want     codes.Code
	}{
		{"bad from", "2026/10/01", "2026-10-01", nil, codes.InvalidArgument},
		{"bad to", "2026-10-01", "", nil, codes.InvalidArgument},
		{"reversed", "2026-10-02", "2026-10-01", nil, codes.InvalidArgument},
		{"93 days", "2026-08-01", "2026-11-01", nil, codes.InvalidArgument},
		{"rejected key", "2026-10-01", "2026-10-31", anthropicadmin.ErrUnauthorized, codes.FailedPrecondition},
		{"forbidden key", "2026-10-01", "2026-10-31", anthropicadmin.ErrForbidden, codes.FailedPrecondition},
		{"canceled", "2026-10-01", "2026-10-31", fmt.Errorf("cost report: %w", context.Canceled), codes.Canceled},
		{"anthropic down", "2026-10-01", "2026-10-31", fmt.Errorf("%w: status 529", anthropicadmin.ErrUnavailable), codes.Unavailable},
		{"other", "2026-10-01", "2026-10-31", fmt.Errorf("decode cost report: boom"), codes.Internal},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := dialVendorCost(t, &stubAnthropicCosts{err: tc.err})
			_, err := c.GetAnthropicCost(t.Context(), &vendorcostv1.GetAnthropicCostRequest{FromDate: tc.from, ToDate: tc.to})
			if status.Code(err) != tc.want {
				t.Fatalf("code = %v (%v), want %v", status.Code(err), err, tc.want)
			}
		})
	}
}

func TestGetAnthropicCostAllows92Days(t *testing.T) {
	c := dialVendorCost(t, &stubAnthropicCosts{})
	if _, err := c.GetAnthropicCost(t.Context(), &vendorcostv1.GetAnthropicCostRequest{FromDate: "2026-08-01", ToDate: "2026-10-31"}); err != nil {
		t.Fatalf("Aug 1 – Oct 31 (92 days): %v", err)
	}
}

func TestVendorCostOffWithoutReader(t *testing.T) {
	c := dialVendorCost(t, nil)
	_, err := c.GetAnthropicCost(t.Context(), &vendorcostv1.GetAnthropicCostRequest{FromDate: "2026-10-01", ToDate: "2026-10-01"})
	if status.Code(err) != codes.Unimplemented {
		t.Fatalf("code = %v, want Unimplemented", status.Code(err))
	}
}
