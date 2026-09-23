package server

import (
	"google.golang.org/grpc"

	modelconfigv1 "github.com/ogen-app/ogen/gen/modelconfig/v1"
	"github.com/ogen-app/ogen/src/infra/repository"
)

// registerModelConfigAdmin wires the CON-308 ModelConfigAdminService onto the
// internal gRPC server, alongside Secrets + TenantAdmin + PlatformAdmin.
func registerModelConfigAdmin(srv *grpc.Server, repo repository.FlowModelConfigRepository) {
	modelconfigv1.RegisterModelConfigAdminServiceServer(srv, newModelConfigAdminService(repo))
}
