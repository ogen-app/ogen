package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"time"

	"google.golang.org/grpc"

	"github.com/ogen-app/ogen/src/jobs/queues"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/transport/grpc/jobrpc"
)

// jobRPCStopGrace bounds the wait for in-flight job RPCs on shutdown.
const jobRPCStopGrace = 10 * time.Second

// initJobRPC starts the listener river-service's jobs call ogen on, when
// JOB_RPC_ADDR is set. Unlike Harbor's operator server it runs inside the app,
// so the sweeps it serves use the app's own repositories and storage.
func (d *deps) initJobRPC(ctx context.Context) error {
	if d.cfg.JobRPCAddr == "" {
		return nil
	}
	token := strings.TrimSpace(d.cfg.RiverRPCAuthToken)
	if token != "" && token == strings.TrimSpace(d.cfg.GRPCAuthToken) {
		return errors.New("RIVER_RPC_AUTH_TOKEN must differ from GRPC_AUTH_TOKEN, so Harbor's token never opens job RPCs")
	}
	qd := queueDeps(d)
	srv, err := jobrpc.New(jobrpc.Deps{
		Token:          token,
		MaxConcurrency: d.cfg.JobRPCMaxConcurrency,
		Secrets:        d.secretStore,
		LeaseTTL:       d.cfg.CredentialLeaseTTL,
		Maintenance:    queues.MaintenanceTasks(qd),
		Email:          queues.NewSendEmail(qd),
	})
	if err != nil {
		return err
	}
	var lc net.ListenConfig
	lis, err := lc.Listen(ctx, "tcp", d.cfg.JobRPCAddr)
	if err != nil {
		return fmt.Errorf("job rpc: listen %s: %w", d.cfg.JobRPCAddr, err)
	}
	go func() {
		if err := srv.Serve(lis); err != nil {
			slog.Error("job rpc server exited", logging.AttrComponent, "jobrpc", logging.AttrError, err)
		}
	}()
	d.shutdown.add(stageIntegrations, func() error {
		stopGracefully(srv, jobRPCStopGrace)
		return nil
	})
	slog.InfoContext(ctx, "job rpc listening", logging.AttrComponent, "jobrpc", "addr", d.cfg.JobRPCAddr)
	return nil
}

// stopGracefully drains in-flight calls, falling back to a hard stop after
// grace.
func stopGracefully(srv *grpc.Server, grace time.Duration) {
	done := make(chan struct{})
	go func() {
		srv.GracefulStop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(grace):
		srv.Stop()
	}
}
