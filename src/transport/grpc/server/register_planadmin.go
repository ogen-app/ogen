package server

import (
	"google.golang.org/grpc"

	plansv1 "github.com/ogen-app/ogen/gen/plans/v1"
	"github.com/ogen-app/ogen/src/domain/entitlements"
	"github.com/ogen-app/ogen/src/infra/repository"
)

// registerPlanAdmin wires the CON-294 PlanAdminService onto the internal gRPC
// server, alongside Secrets + TenantAdmin + PlatformAdmin.
func registerPlanAdmin(
	srv *grpc.Server,
	versionRepo repository.TenantTierVersionRepository,
	assignmentRepo repository.TenantTierAssignmentRepository,
	catalog *entitlements.Catalog,
	resolver *entitlements.Resolver,
) {
	plansv1.RegisterPlanAdminServiceServer(srv, newPlanAdminService(versionRepo, assignmentRepo, catalog, resolver))
}
