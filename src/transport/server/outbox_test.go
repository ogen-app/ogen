package server

import (
	"strings"
	"testing"

	"github.com/ogen-app/ogen/src/kernel/config"
)

func TestRequireJobPipeline(t *testing.T) {
	ctx := t.Context()
	full := &config.Config{
		RiverServiceAddr: "dns:///river-service:9092", RiverServiceAuthToken: "a",
		JobRPCAddr: ":9093", RiverRPCAuthToken: "b",
	}
	if err := requireJobPipeline(ctx, full); err != nil {
		t.Fatalf("complete config: %v", err)
	}
	partial := *full
	partial.JobRPCAddr, partial.RiverServiceAuthToken = "", " "
	err := requireJobPipeline(ctx, &partial)
	if err == nil || !strings.Contains(err.Error(), "RIVER_SERVICE_AUTH_TOKEN, JOB_RPC_ADDR") || !strings.Contains(err.Error(), "JOBS_DISABLED") {
		t.Fatalf("missing settings: err = %v", err)
	}
	partial.JobsDisabled = true
	if err := requireJobPipeline(ctx, &partial); err != nil {
		t.Errorf("with JOBS_DISABLED: %v", err)
	}
}
