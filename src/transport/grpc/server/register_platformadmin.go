//go:build platformadmin

package server

import (
	"google.golang.org/grpc"

	platformsv1 "github.com/ogen-app/ogen/gen/platforms/v1"
	"github.com/ogen-app/ogen/src/infra/repository"
)

// registerPlatformAdmin wires the CON-292 PlatformAdminService onto the internal
// gRPC server. Active only under the `platformadmin` build tag (see platforms.go);
// the default build uses the no-op stub in register_platformadmin_stub.go until
// gen/platforms/v1 exists.
func registerPlatformAdmin(
	srv *grpc.Server,
	platformRepo repository.PlatformRepository,
	limitsRepo repository.PlatformGlobalLimitsRepository,
) {
	platformsv1.RegisterPlatformAdminServiceServer(srv, newPlatformAdminService(platformRepo, limitsRepo))
}
