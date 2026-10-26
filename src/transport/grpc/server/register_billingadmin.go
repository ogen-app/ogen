package server

import (
	"context"
	"log/slog"

	"google.golang.org/grpc"

	billingv1 "github.com/ogen-app/ogen/gen/billing/v1"
	"github.com/ogen-app/ogen/src/kernel/logging"
)

// registerBillingAdmin wires the CON-322 BillingAdminService onto the internal
// gRPC server. With no billing usecase (nil) the service answers Unimplemented.
func registerBillingAdmin(srv *grpc.Server, svc BillingAdmin) {
	if svc == nil {
		return
	}
	billingv1.RegisterBillingAdminServiceServer(srv, newBillingAdminService(svc))
}

// checkVariantsAfterPublish checks the variant map once a tier version is
// published, off the request: a new sellable price usually has no variant
// yet, which the check logs and ListBillingAttention reports. It never blocks
// or fails the publish.
func checkVariantsAfterPublish(svc BillingAdmin) func(context.Context) {
	return func(ctx context.Context) {
		if !svc.Enabled() {
			return
		}
		go func() {
			ctx := context.WithoutCancel(ctx)
			if _, err := svc.CheckVariants(ctx); err != nil {
				slog.WarnContext(ctx, "billing variant check after publish failed", logging.AttrComponent, "grpcserver", logging.AttrError, err)
			}
		}()
	}
}
