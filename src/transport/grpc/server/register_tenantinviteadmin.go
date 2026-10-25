package server

import (
	"google.golang.org/grpc"

	tenantinvitesv1 "github.com/ogen-app/ogen/gen/tenantinvites/v1"
)

// registerTenantInviteAdmin wires TenantInviteAdminService onto the internal
// gRPC server. With no usecase (nil) the service answers Unimplemented.
func registerTenantInviteAdmin(srv *grpc.Server, svc TenantInvites) {
	if svc == nil {
		return
	}
	tenantinvitesv1.RegisterTenantInviteAdminServiceServer(srv, newTenantInviteAdminService(svc))
}
