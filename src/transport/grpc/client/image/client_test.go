package image

import (
	"errors"
	"testing"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	imagev1 "github.com/ogen-app/ogen/gen/image/v1"
	"github.com/ogen-app/ogen/src/domain/models"
)

func rejectErr(t *testing.T, domain, reason string) error {
	t.Helper()
	st, err := grpcstatus.New(codes.InvalidArgument, "nope").
		WithDetails(&errdetails.ErrorInfo{Domain: domain, Reason: reason})
	if err != nil {
		t.Fatalf("build detail: %v", err)
	}
	return st.Err()
}

// TestRejectedCode covers parsing the ErrorInfo reason image-service attaches to a
// terminal reject back into the generated image.v1.RejectedCode enum (CON-281).
func TestRejectedCode(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want imagev1.RejectedCode
	}{
		{"vector", rejectErr(t, "image.v1", "REJECTED_CODE_VECTOR"), imagev1.RejectedCode_REJECTED_CODE_VECTOR},
		{"too_large", rejectErr(t, "image.v1", "REJECTED_CODE_TOO_LARGE"), imagev1.RejectedCode_REJECTED_CODE_TOO_LARGE},
		{"dimensions", rejectErr(t, "image.v1", "REJECTED_CODE_DIMENSIONS_EXCEEDED"), imagev1.RejectedCode_REJECTED_CODE_DIMENSIONS_EXCEEDED},
		{"unknown reason", rejectErr(t, "image.v1", "REJECTED_CODE_MYSTERY"), imagev1.RejectedCode_REJECTED_CODE_UNSPECIFIED},
		{"wrong domain ignored", rejectErr(t, "other", "REJECTED_CODE_VECTOR"), imagev1.RejectedCode_REJECTED_CODE_UNSPECIFIED},
		{"status without detail", grpcstatus.Error(codes.InvalidArgument, "nope"), imagev1.RejectedCode_REJECTED_CODE_UNSPECIFIED},
		{"plain error", errors.New("boom"), imagev1.RejectedCode_REJECTED_CODE_UNSPECIFIED},
		{"nil", nil, imagev1.RejectedCode_REJECTED_CODE_UNSPECIFIED},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := RejectedCode(tc.err); got != tc.want {
				t.Fatalf("RejectedCode = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestUploadCode covers the typed enum → stable upload-code mapping (CON-281),
// including the "" fallback so the caller keeps its coarse bucket.
func TestUploadCode(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"vector", rejectErr(t, "image.v1", "REJECTED_CODE_VECTOR"), models.UploadCodeVectorRejected},
		{"unsupported", rejectErr(t, "image.v1", "REJECTED_CODE_UNSUPPORTED_MEDIA_TYPE"), models.UploadCodeUnsupportedMediaType},
		{"too_large", rejectErr(t, "image.v1", "REJECTED_CODE_TOO_LARGE"), models.UploadCodeTooLarge},
		{"dimensions", rejectErr(t, "image.v1", "REJECTED_CODE_DIMENSIONS_EXCEEDED"), models.UploadCodeDimensionsExceeded},
		{"corrupt", rejectErr(t, "image.v1", "REJECTED_CODE_CORRUPT"), models.UploadCodeInvalidFile},
		{"unspecified → empty", rejectErr(t, "image.v1", "REJECTED_CODE_UNSPECIFIED"), ""},
		{"no detail → empty", grpcstatus.Error(codes.InvalidArgument, "nope"), ""},
		{"nil → empty", nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := UploadCode(tc.err); got != tc.want {
				t.Fatalf("UploadCode = %q, want %q", got, tc.want)
			}
		})
	}
}
