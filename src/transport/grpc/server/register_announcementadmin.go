package server

import (
	"google.golang.org/grpc"

	announcementsv1 "github.com/ogen-app/ogen/gen/announcements/v1"
	"github.com/ogen-app/ogen/src/infra/repository"
)

// registerAnnouncementAdmin wires the CON-230 AnnouncementAdminService onto the
// internal gRPC server, alongside Secrets + TenantAdmin + PlatformAdmin +
// PlanAdmin + EmailAdmin.
func registerAnnouncementAdmin(srv *grpc.Server, repo repository.AnnouncementRepository) {
	announcementsv1.RegisterAnnouncementAdminServiceServer(srv, newAnnouncementAdminService(repo))
}
