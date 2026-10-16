package server

import (
	"context"
	"log/slog"
	"time"

	"github.com/ogen-app/ogen/src/jobs/outbox"
	"github.com/ogen-app/ogen/src/kernel/logging"
	jobsclient "github.com/ogen-app/ogen/src/transport/grpc/client/jobs"
)

// initRelay starts the relay that delivers the job outbox to river-service.
// Without RIVER_SERVICE_ADDR jobs wait in the outbox until one is configured.
func (d *deps) initRelay(ctx context.Context) error {
	if d.cfg.RiverServiceAddr == "" {
		slog.WarnContext(ctx, "RIVER_SERVICE_ADDR is not set: jobs stay in the outbox undelivered", logging.AttrComponent, "outbox")
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
