package server

import (
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	platformsv1 "github.com/ogen-app/ogen/gen/platforms/v1"
	"github.com/ogen-app/ogen/src/domain/models"
)

func TestValidatePlatformWriteAltTextCap(t *testing.T) {
	limits := models.DefaultPlatformGlobalLimits() // 2000-char global ceiling
	cases := []struct {
		name   string
		altCap int32
		want   codes.Code
	}{
		{name: "zero means no alt text", altCap: 0, want: codes.OK},
		{name: "at the global ceiling", altCap: 2000, want: codes.OK},
		{name: "above the global ceiling", altCap: 2001, want: codes.InvalidArgument},
		{name: "negative", altCap: -1, want: codes.InvalidArgument},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pb := &platformsv1.Platform{
				Name:             "X",
				ZernioId:         "twitter",
				ImageConstraints: &platformsv1.ImageConstraints{MaxAltTextChars: tc.altCap},
			}
			if got := status.Code(validatePlatformWrite(pb, limits)); got != tc.want {
				t.Fatalf("code = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestImageConstraintsProtoRoundTripKeepsAltTextCap(t *testing.T) {
	in := models.ImageConstraints{MaxFileSizeBytes: 1 << 20, AllowedFormats: []string{"png"}, MaxAttachmentsPerPost: 4, MaxAltTextChars: 1000}
	if out := fromImageProto(toImageProto(in)); out.MaxAltTextChars != 1000 {
		t.Fatalf("MaxAltTextChars = %d after round trip, want 1000", out.MaxAltTextChars)
	}
}
