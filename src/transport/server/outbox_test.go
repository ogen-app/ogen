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
	if err := RequireJobPipeline(ctx, full); err != nil {
		t.Fatalf("complete config: %v", err)
	}
	partial := *full
	partial.JobRPCAddr, partial.RiverServiceAuthToken = "", " "
	err := RequireJobPipeline(ctx, &partial)
	if err == nil || !strings.Contains(err.Error(), "RIVER_SERVICE_AUTH_TOKEN, JOB_RPC_ADDR") || !strings.Contains(err.Error(), "JOBS_DISABLED") {
		t.Fatalf("missing settings: err = %v", err)
	}
	partial.JobsDisabled = true
	if err := RequireJobPipeline(ctx, &partial); err != nil {
		t.Errorf("with JOBS_DISABLED: %v", err)
	}
}

// TestJobsDisabledRunsNoJobs checks JOBS_DISABLED stops jobs even when the
// pipeline is configured (docker compose always sets RIVER_SERVICE_ADDR):
// neither the relay nor the job RPC listener starts.
func TestJobsDisabledRunsNoJobs(t *testing.T) {
	d := &deps{shutdown: &shutdownPlan{}, cfg: &config.Config{
		RiverServiceAddr: "dns:///river-service:9092", RiverServiceAuthToken: "a",
		JobRPCAddr: ":9093", RiverRPCAuthToken: "b", JobsDisabled: true,
	}}
	if err := d.initRelay(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := d.initJobRPC(t.Context()); err != nil {
		t.Fatal(err)
	}
	if n := len(d.shutdown.ordered()); n != 0 {
		t.Fatalf("JOBS_DISABLED started %d job components, want none", n)
	}
}
