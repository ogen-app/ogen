package server

import (
	"google.golang.org/grpc"

	emailv1 "github.com/ogen-app/ogen/gen/email/v1"
	"github.com/ogen-app/ogen/src/infra/repository"
)

// registerEmailAdmin wires the CON-298 EmailAdminService onto the internal gRPC
// server, alongside Secrets + TenantAdmin + PlatformAdmin + PlanAdmin. The
// tenants/users/enqueuer/harborBaseURL deps power the CON-229 send RPC
// (NotifyOperatorsTenantRegistered); all are nil/empty-safe.
func registerEmailAdmin(
	srv *grpc.Server,
	logs repository.EmailLogRepository,
	events repository.EmailEventRepository,
	bodyStore repository.EmailBodyRepository,
	liveBody EmailBodyGetter,
	tenants repository.TenantRepository,
	users repository.UserRepository,
	enqueuer AdminEmailEnqueuer,
	harborBaseURL string,
) {
	emailv1.RegisterEmailAdminServiceServer(srv, newEmailAdminService(logs, events, bodyStore, liveBody, tenants, users, enqueuer, harborBaseURL))
}
