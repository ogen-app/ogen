package server

import (
	"context"

	"google.golang.org/grpc"

	plansv1 "github.com/ogen-app/ogen/gen/plans/v1"
	"github.com/ogen-app/ogen/src/domain/entitlements"
	"github.com/ogen-app/ogen/src/infra/eventhub"
	"github.com/ogen-app/ogen/src/infra/repository"
)

// registerPlanAdmin wires the CON-294 PlanAdminService onto the internal gRPC
// server, alongside Secrets + TenantAdmin + PlatformAdmin. afterPublish, when
// set, runs after every publish.
func registerPlanAdmin(
	srv *grpc.Server,
	versionRepo repository.TenantTierVersionRepository,
	assignmentRepo repository.TenantTierAssignmentRepository,
	catalog *entitlements.Catalog,
	resolver *entitlements.Resolver,
	hub eventhub.Hub,
	afterPublish func(context.Context),
) {
	svc := newPlanAdminService(versionRepo, assignmentRepo, catalog, resolver, hub)
	svc.afterPublish = afterPublish
	plansv1.RegisterPlanAdminServiceServer(srv, svc)
}
