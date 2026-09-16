package server

import (
	"google.golang.org/grpc"

	emailv1 "github.com/ogen-app/ogen/gen/email/v1"
	"github.com/ogen-app/ogen/src/infra/repository"
)

// registerEmailAdmin wires the CON-298 EmailAdminService onto the internal gRPC
// server, alongside Secrets + TenantAdmin + PlatformAdmin + PlanAdmin.
func registerEmailAdmin(
	srv *grpc.Server,
	logs repository.EmailLogRepository,
	events repository.EmailEventRepository,
	bodies EmailBodyGetter,
) {
	emailv1.RegisterEmailAdminServiceServer(srv, newEmailAdminService(logs, events, bodies))
}
