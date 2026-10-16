package server

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/ogen-app/ogen/src/jobs/outbox"
	"github.com/ogen-app/ogen/src/kernel/config"
	"github.com/ogen-app/ogen/src/kernel/logging"
	jobsclient "github.com/ogen-app/ogen/src/transport/grpc/client/jobs"
)

// initRelay starts the relay that delivers the job outbox to river-service.
// Without RIVER_SERVICE_ADDR (allowed only with JOBS_DISABLED) jobs wait in the
// outbox.
func (d *deps) initRelay(ctx context.Context) error {
	if d.cfg.RiverServiceAddr == "" {
		return nil
	}
	client, err := jobsclient.New(jobsclient.Config{Addr: d.cfg.RiverServiceAddr, Token: d.cfg.RiverServiceAuthToken})
	if err != nil {
		return err
	}
	relay := outbox.NewRelay(d.db.DB, client, outbox.RelayConfig{
		DSN:          d.cfg.DSN,
		PollInterval: d.cfg.OutboxRelayPollInterval,
		BatchSize:    d.cfg.OutboxRelayBatch,
		Retention:    time.Duration(d.cfg.OutboxRetentionDays) * 24 * time.Hour,
	})
	// Detached from the boot context; stopped by the shutdown hook.
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	done := make(chan struct{})
	go func() {
		defer close(done)
		relay.Run(runCtx)
	}()
	d.shutdown.add(stageIntegrations, func() error {
		cancel()
		<-done
		return client.Close()
	})
	slog.InfoContext(ctx, "job outbox relay started", logging.AttrComponent, "outbox",
		"river_service", d.cfg.RiverServiceAddr)
	return nil
}

// requireJobPipeline fails the boot when river-service can't receive ogen's
// jobs or call back into ogen: every job would then sit in the outbox while
// the API looked healthy. JOBS_DISABLED makes that an explicit choice.
func requireJobPipeline(ctx context.Context, cfg *config.Config) error {
	var missing []string
	for _, v := range []struct{ name, value string }{
		{"RIVER_SERVICE_ADDR", cfg.RiverServiceAddr},
		{"RIVER_SERVICE_AUTH_TOKEN", cfg.RiverServiceAuthToken},
		{"JOB_RPC_ADDR", cfg.JobRPCAddr},
		{"RIVER_RPC_AUTH_TOKEN", cfg.RiverRPCAuthToken},
	} {
		if strings.TrimSpace(v.value) == "" {
			missing = append(missing, v.name)
		}
	}
	switch {
	case len(missing) == 0:
		return nil
	case cfg.JobsDisabled:
		slog.WarnContext(ctx, "JOBS_DISABLED: background jobs wait in the outbox and never run",
			logging.AttrComponent, "outbox", "missing", missing)
		return nil
	default:
		return fmt.Errorf("background jobs need %s; set JOBS_DISABLED=true to run the API without them",
			strings.Join(missing, ", "))
	}
}
