package server

import (
	"google.golang.org/grpc"

	billingv1 "github.com/ogen-app/ogen/gen/billing/v1"
)

// registerBillingAdmin wires the CON-322 BillingAdminService onto the internal
// gRPC server. With no billing usecase (nil) the service answers Unimplemented.
func registerBillingAdmin(srv *grpc.Server, svc BillingAdmin) {
	if svc == nil {
		return
	}
	billingv1.RegisterBillingAdminServiceServer(srv, newBillingAdminService(svc))
}
