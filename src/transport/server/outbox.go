package server

import (
	"context"
	"log/slog"
	"time"

	"github.com/ogen-app/ogen/src/jobs/outbox"
	"github.com/ogen-app/ogen/src/jobs/queues"
	"github.com/ogen-app/ogen/src/kernel/logging"
	jobsclient "github.com/ogen-app/ogen/src/transport/grpc/client/jobs"
)

// initRelay routes the job kinds river-service owns through the job outbox
// and starts the relay that delivers them. Without RIVER_SERVICE_ADDR every
// kind stays in-process. The relay runs whenever river-service is configured,
// so rows written while a kind was relayed are still delivered after it is
// taken off OUTBOX_RELAY_KINDS.
func (d *deps) initRelay(ctx context.Context) error {
	kinds, err := queues.RelayKinds(d.cfg.OutboxRelayKinds, d.cfg.RiverServiceAddr)
	if err != nil {
		return err
	}
	if d.cfg.RiverServiceAddr == "" {
		return nil
	}
	client, err := jobsclient.New(jobsclient.Config{Addr: d.cfg.RiverServiceAddr, Token: d.cfg.RiverServiceAuthToken})
	if err != nil {
		return err
	}
	d.enqueuer.Relay(outbox.NewWriter(d.db.DB), kinds)

	relay := outbox.NewRelay(d.db.DB, client, outbox.RelayConfig{
		DSN:          d.cfg.DSN,
		PollInterval: d.cfg.OutboxRelayPollInterval,
		BatchSize:    d.cfg.OutboxRelayBatch,
		Retention:    time.Duration(d.cfg.OutboxRetentionDays) * 24 * time.Hour,
	})
	// Detached from the boot context like River; stopped by the shutdown hook.
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
		"river_service", d.cfg.RiverServiceAddr, "relayed_kinds", kinds)
	return nil
}
