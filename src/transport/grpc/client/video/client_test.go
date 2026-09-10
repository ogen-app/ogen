package video

import (
	"errors"
	"testing"

	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"
)

func TestNew_DisabledWhenAddrEmpty(t *testing.T) {
	c, err := New(Config{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c != nil {
		t.Fatalf("empty addr must yield a nil (disabled) client, got %v", c)
	}
}

func TestProbe_NilClientReportsDisabled(t *testing.T) {
	var c *Client // disabled sentinel
	_, err := c.Probe(t.Context(), ProbeOptions{SourceURL: "https://example/v.mp4"})
	if !errors.Is(err, ErrDisabled) {
		t.Fatalf("want ErrDisabled, got %v", err)
	}
}

func TestClose_NilClientSafe(t *testing.T) {
	var c *Client
	if err := c.Close(); err != nil {
		t.Fatalf("Close on nil client must be safe, got %v", err)
	}
}

func TestIsInvalidVideo(t *testing.T) {
	// InvalidArgument is the terminal "not a usable video" verdict.
	if !IsInvalidVideo(grpcstatus.Error(codes.InvalidArgument, "corrupt")) {
		t.Error("InvalidArgument must classify as invalid video")
	}
	// Transient failures must NOT — callers degrade gracefully on these.
	for _, code := range []codes.Code{codes.Unavailable, codes.DeadlineExceeded, codes.Internal} {
		if IsInvalidVideo(grpcstatus.Error(code, "transient")) {
			t.Errorf("%v must not classify as invalid video", code)
		}
	}
	// A plain non-gRPC error is not a terminal video verdict.
	if IsInvalidVideo(errors.New("boom")) {
		t.Error("plain error must not classify as invalid video")
	}
}
