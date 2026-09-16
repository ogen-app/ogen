package image

import (
	"errors"
	"testing"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"
)

// TestRejectedReason covers extraction of the machine-readable reject reason that
// image-service attaches as a google.rpc.ErrorInfo detail (CON-281 Phase 2).
func TestRejectedReason(t *testing.T) {
	withInfo := func(domain, reason string) error {
		st, err := grpcstatus.New(codes.InvalidArgument, "nope").
			WithDetails(&errdetails.ErrorInfo{Domain: domain, Reason: reason})
		if err != nil {
			t.Fatalf("build detail: %v", err)
		}
		return st.Err()
	}

	cases := []struct {
		name string
		err  error
		want string
	}{
		{"vector", withInfo("image.v1", "REJECTED_CODE_VECTOR"), "REJECTED_CODE_VECTOR"},
		{"too_large", withInfo("image.v1", "REJECTED_CODE_TOO_LARGE"), "REJECTED_CODE_TOO_LARGE"},
		{"wrong domain ignored", withInfo("other", "REJECTED_CODE_VECTOR"), ""},
		{"status without detail", grpcstatus.Error(codes.InvalidArgument, "nope"), ""},
		{"plain error", errors.New("boom"), ""},
		{"nil", nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := RejectedReason(tc.err); got != tc.want {
				t.Fatalf("RejectedReason = %q, want %q", got, tc.want)
			}
		})
	}
}
