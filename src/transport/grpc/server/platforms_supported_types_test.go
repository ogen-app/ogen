package server

import (
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	platformsv1 "github.com/ogen-app/ogen/gen/platforms/v1"
	"github.com/ogen-app/ogen/src/domain/models"
)

func TestValidatePlatformWriteRejectsUnruledSupportedType(t *testing.T) {
	pb := &platformsv1.Platform{
		Name:               "Facebook",
		ZernioId:           "facebook",
		SupportedPostTypes: []string{"text-post", "link-post"},
	}
	limits := models.DefaultPlatformGlobalLimits()
	if err := validatePlatformWrite(pb, limits); err != nil {
		t.Fatalf("ruled types must pass, got %v", err)
	}

	pb.SupportedPostTypes = append(pb.SupportedPostTypes, "live-video")
	err := validatePlatformWrite(pb, limits)
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("an unruled supported type must be InvalidArgument, got %v", err)
	}
}
