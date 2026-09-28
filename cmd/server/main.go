// @title           Content Control Center API
// @version         1.0
// @description     REST API for the Content Control Center application.
// @host            localhost:9001
// @BasePath        /
//
// @securityDefinitions.apikey  CookieAuth
// @in                          cookie
// @name                        c3_session
// @description                 Session token obtained from POST /api/sessions (login).
package main

import (
	"context"
	"fmt"
	"log"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/uptrace/bun"
	"google.golang.org/grpc"

	_ "github.com/ogen-app/ogen/docs"
	"github.com/ogen-app/ogen/src/infra/database"
	"github.com/ogen-app/ogen/src/infra/email/resend"
	"github.com/ogen-app/ogen/src/infra/eventhub"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/infra/secrets"
	"github.com/ogen-app/ogen/src/jobs/queues"
	"github.com/ogen-app/ogen/src/kernel/config"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/kernel/telemetry"
	grpcserver "github.com/ogen-app/ogen/src/transport/grpc/server"
	"github.com/ogen-app/ogen/src/transport/server"
)

const (
	shutdownGrace   = 10 * time.Second
	backfillTimeout = 2 * time.Minute
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		// The structured logger is built from cfg, so this error predates it.
		log.Fatalf("load config: %v", err)
	}
	logging.New(cfg)

	if err := run(cfg); err != nil {
		slog.Error("server exited", logging.AttrComponent, "boot", logging.AttrError, err)
		os.Exit(1)
	}
}

// run boots and serves until a termination signal or a listener failure. It
// returns rather than exiting so every deferred cleanup (telemetry flush, pool
// close, gRPC drain) runs on both paths.
func run(cfg *config.Config) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Must precede anything that creates OTel spans (DB pool, gRPC, Genkit).
	// Init failure is fail-open: telemetry never blocks boot.
	telemetryShutdown, err := telemetry.Init(ctx, cfg)
	if err != nil {
		slog.Warn("telemetry init failed, disabling (fail-open)",
			logging.AttrComponent, "boot", logging.AttrError, err)
	}
	defer func() {
		flushCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := telemetryShutdown(flushCtx); err != nil {
			slog.Warn("telemetry shutdown", logging.AttrComponent, "boot", logging.AttrError, err)
		}
	}()

	db, err := database.New(cfg.DSN, cfg.Debug)
	if err != nil {
		return fmt.Errorf("connect to database: %w", err)
	}
	db.DB.SetMaxOpenConns(cfg.DBMaxOpenConns)
	db.DB.SetMaxIdleConns(cfg.DBMaxIdleConns)
	defer db.Close()

	if err := database.Migrate(ctx, db); err != nil {
		return fmt.Errorf("run migrations: %w", err)
	}

	analyticsDB := openAnalytics(ctx, cfg)
	if analyticsDB != nil {
		defer func() { _ = analyticsDB.Close() }()
	}

	store, err := initSecrets(ctx, cfg, db)
	if err != nil {
		return err
	}

	// Shared by the HTTP server and the internal gRPC server, so an operator
	// tier change over gRPC reaches the tenant's open tabs. The per-user cap
	// counts both SSE streams across every tab; 30 keeps evict-oldest off the
	// normal path while still bounding runaway clients.
	hub := eventhub.New(eventhub.Config{MaxSubscribersPerUser: 30})

	// Detached from the signal: server.New starts River and refresh loops with
	// this ctx, and River hard-cancels running jobs when its Start ctx ends.
	// Those are drained by the app's shutdown hooks instead.
	app, err := server.New(context.WithoutCancel(ctx), db, analyticsDB, cfg, store, hub)
	if err != nil {
		return fmt.Errorf("init server: %w", err)
	}

	if gs := startInternalGRPC(cfg, db, store, hub); gs != nil {
		defer stopGRPC(gs, shutdownGrace)
	}

	// bootedAt bounds the activity backfill to post_logs that predate this
	// process, so it can never duplicate events the live path writes.
	go runBootBackfills(ctx, db, analyticsDB, time.Now())

	slog.Info("server listening", logging.AttrComponent, "boot", "addr", cfg.Addr)
	serveErr := make(chan error, 1)
	go func() { serveErr <- app.Listen(cfg.Addr) }()

	select {
	case err := <-serveErr:
		return err
	case <-ctx.Done():
		stop() // restore default signal handling so a second signal force-quits
		slog.Info("shutdown signal received; draining", logging.AttrComponent, "boot")
		shutCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
		defer cancel()
		if err := app.ShutdownWithContext(shutCtx); err != nil {
			slog.Error("graceful shutdown failed", logging.AttrComponent, "boot", logging.AttrError, err)
		}
		return nil
	}
}

// openAnalytics connects and migrates the isolated TimescaleDB pool. Usage and
// activity analytics are fail-open: any failure logs and returns nil, and the
// API runs with analytics disabled.
func openAnalytics(ctx context.Context, cfg *config.Config) *bun.DB {
	if cfg.AnalyticsDSN == "" {
		return nil
	}
	adb, err := database.NewAnalytics(cfg.AnalyticsDSN, cfg.Debug)
	if err != nil {
		slog.Warn("usage analytics connect failed, disabling (fail-open)",
			logging.AttrComponent, "boot", logging.AttrError, err)
		return nil
	}
	if err := database.MigrateAnalytics(ctx, adb); err != nil {
		slog.Warn("usage analytics migrate failed, disabling (fail-open)",
			logging.AttrComponent, "boot", logging.AttrError, err)
		_ = adb.Close()
		return nil
	}
	return adb
}

// initSecrets builds the envelope-encrypted secret store and seeds it from the
// environment on first boot. A KEK error is fatal: running without an
// unwrapper would make rotated keys silently unrecoverable.
func initSecrets(ctx context.Context, cfg *config.Config, db *bun.DB) (secrets.Store, error) {
	cipher, kekSrc, err := secrets.InitCipher(cfg.KEKPath)
	if err != nil {
		return nil, fmt.Errorf("init secret cipher: %w", err)
	}
	store := secrets.NewStore(repository.NewSecretRepository(db), cipher)

	// First-boot seeds only; afterwards secrets are set and rotated over the
	// gRPC secrets service. Empty values are fine: each feature degrades off.
	bootResult, err := secrets.MigrateFromEnv(ctx, store, []secrets.EnvSource{
		{Name: secrets.NameAnthropicAPIKey, EnvValue: cfg.AnthropicAPIKey},
		{Name: secrets.NameZernioAPIKey, EnvValue: cfg.ZernioAPIKey},
		{Name: secrets.NameGeminiAPIKey, EnvValue: os.Getenv("GEMINI_API_KEY")},
		{Name: secrets.NameResendAPIKey, EnvValue: cfg.ResendAPIKey},
		{Name: secrets.NameResendWebhookSecret, EnvValue: cfg.ResendWebhookSecret},
		{Name: secrets.NameEmailLinkSecret, EnvValue: cfg.EmailLinkSecret},
		{Name: secrets.NameFirecrawlAPIKey, EnvValue: cfg.FirecrawlAPIKey},
	})
	if err != nil {
		return nil, fmt.Errorf("migrate secrets from env: %w", err)
	}
	// The unsubscribe-link HMAC key has no operator source; generate a stable
	// one on first boot so one-click unsubscribe works out of the box.
	if err := secrets.EnsureGenerated(ctx, store, secrets.NameEmailLinkSecret); err != nil {
		return nil, fmt.Errorf("ensure email link secret: %w", err)
	}
	secrets.LogBootSummary(kekSrc, filepath.Join(cfg.KEKPath, secrets.KEKFilename), bootResult)
	return store, nil
}

// startInternalGRPC starts the operator gRPC surface used by Harbor. It runs
// only when both an address and an auth token are configured, and any failure
// is logged and non-fatal so the primary HTTP surface still comes up. It
// returns the running server, or nil when it is disabled or failed to start.
func startInternalGRPC(cfg *config.Config, db *bun.DB, store secrets.Store, hub eventhub.Hub) *grpc.Server {
	if cfg.GRPCAddr == "" || cfg.GRPCAuthToken == "" {
		return nil
	}

	// A nil enqueuer turns registration notifications into a soft no-op.
	var adminEmailEnqueuer grpcserver.AdminEmailEnqueuer
	if enq, err := queues.NewInsertOnlyEnqueuer(db); err != nil {
		slog.Error("grpc admin-email enqueuer init failed; registration notifications disabled (non-fatal)",
			logging.AttrComponent, "boot", logging.AttrError, err)
	} else {
		adminEmailEnqueuer = enq
	}

	gs, err := grpcserver.New(
		cfg.GRPCAuthToken, store,
		repository.NewTenantTierRepository(db),
		repository.NewTenantGroupRepository(db),
		repository.NewTenantRepository(db),
		repository.NewPlatformRepository(db),
		repository.NewPlatformGlobalLimitsRepository(db),
		repository.NewFlowModelConfigRepository(db),
		repository.NewTenantTierVersionRepository(db),
		repository.NewTenantTierAssignmentRepository(db),
		repository.NewEmailLogRepository(db),
		repository.NewEmailEventRepository(db),
		repository.NewEmailBodyRepository(db),
		// Per-call key resolution, so a rotated Resend key applies without a reboot.
		resend.New(func(ctx context.Context) (string, error) { return store.Get(ctx, secrets.NameResendAPIKey) }, cfg.EmailBaseURL, cfg.EmailHTTPTimeout),
		repository.NewUserRepository(db),
		adminEmailEnqueuer,
		cfg.HarborBaseURL,
		repository.NewAnnouncementRepository(db),
		hub,
	)
	if err != nil {
		slog.Error("grpc init failed; internal grpc disabled (non-fatal)", logging.AttrComponent, "boot", logging.AttrError, err)
		return nil
	}

	lis, err := net.Listen("tcp", cfg.GRPCAddr)
	if err != nil {
		slog.Error("grpc listen failed; internal grpc disabled (non-fatal)", logging.AttrComponent, "boot", logging.AttrError, err)
		return nil
	}
	slog.Info("internal grpc listening", logging.AttrComponent, "boot", "addr", cfg.GRPCAddr)
	// A containerised deploy almost never wants loopback: other services on the
	// private network could never connect.
	if grpcAddrIsLoopback(cfg.GRPCAddr) {
		slog.Warn("internal grpc bound to loopback; unreachable from other containers/hosts — set GRPC_ADDR=:9091 to reach it over a private network (e.g. Railway)",
			logging.AttrComponent, "boot", "addr", cfg.GRPCAddr)
	}
	go func() {
		if err := gs.Serve(lis); err != nil {
			slog.Error("internal grpc exited", logging.AttrComponent, "boot", logging.AttrError, err)
		}
	}()
	return gs
}

// stopGRPC drains in-flight RPCs, falling back to a hard stop after grace so a
// long-lived stream can't hold shutdown open.
func stopGRPC(gs *grpc.Server, grace time.Duration) {
	done := make(chan struct{})
	go func() {
		gs.GracefulStop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(grace):
		gs.Stop()
	}
}

// runBootBackfills runs the idempotent, restart-safe boot backfills off the
// critical path so the listeners come up right after migrations. Each gets its
// own bounded context derived from ctx, so shutdown cancels them.
func runBootBackfills(ctx context.Context, db, analyticsDB *bun.DB, bootedAt time.Time) {
	if analyticsDB != nil {
		func() {
			ctx, cancel := context.WithTimeout(ctx, backfillTimeout)
			defer cancel()
			if n, err := repository.BackfillPostLogsToActivity(ctx, db, analyticsDB, bootedAt); err != nil {
				slog.Warn("post_logs → tenant_activity_events backfill failed (non-fatal)",
					logging.AttrComponent, "boot", logging.AttrError, err)
			} else if n > 0 {
				slog.Info("post_logs migrated to tenant_activity_events",
					logging.AttrComponent, "boot", "rows", n)
			}
		}()
	}

	// The entitlement resolver falls back to tier_id, so a skipped run only
	// delays the explicit assignment history rows.
	ctx, cancel := context.WithTimeout(ctx, backfillTimeout)
	defer cancel()
	if rep, err := repository.BackfillTenantTierAssignments(ctx, db, false); err != nil {
		slog.Warn("tenant tier-assignment backfill failed (non-fatal)",
			logging.AttrComponent, "boot", logging.AttrError, err)
	} else if rep.Assigned > 0 || rep.SkippedNoVersion > 0 {
		slog.Info("tenant tier-assignment backfill",
			logging.AttrComponent, "boot",
			"assigned", rep.Assigned, "skipped_no_version", rep.SkippedNoVersion, "candidates", rep.Candidates)
	}
}

// grpcAddrIsLoopback reports whether addr binds only a loopback interface. A
// wildcard bind (":9091") and unparseable or hostname addresses report false,
// so the warning is never spurious.
func grpcAddrIsLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil || host == "" {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
