//go:build !platformadmin

package server

import (
	"google.golang.org/grpc"

	"github.com/ogen-app/ogen/src/infra/repository"
)

// registerPlatformAdmin is a no-op in the default build: the CON-292
// PlatformAdminService needs the generated gen/platforms/v1 package, which only
// exists once the platforms/v1 contract is published to buf.build/ogen-app/proto
// and `make proto` is run. Build with `-tags platformadmin` (and after dropping
// the tags per platforms.go's activation note) to register the real service.
func registerPlatformAdmin(
	_ *grpc.Server,
	_ repository.PlatformRepository,
	_ repository.PlatformGlobalLimitsRepository,
) {
}
