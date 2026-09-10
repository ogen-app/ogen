package server

import (
	"google.golang.org/grpc"

	platformsv1 "github.com/ogen-app/ogen/gen/platforms/v1"
	"github.com/ogen-app/ogen/src/infra/repository"
)

// registerPlatformAdmin wires the CON-292 PlatformAdminService onto the internal
// gRPC server, alongside Secrets + TenantAdmin.
func registerPlatformAdmin(
	srv *grpc.Server,
	platformRepo repository.PlatformRepository,
	limitsRepo repository.PlatformGlobalLimitsRepository,
) {
	platformsv1.RegisterPlatformAdminServiceServer(srv, newPlatformAdminService(platformRepo, limitsRepo))
}
