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
	"log"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/uptrace/bun"

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

func main() {
	cfg, err := config.Load()
	if err != nil {
		// The logger needs cfg to build, so this one boot error necessarily
		// predates it — fail fast on the stdlib logger.
		log.Fatalf("load config: %v", err)
	}

	// Install the structured logger before anything else logs, so even early
	// boot errors are structured and any stray stdlib log.Print is bridged.
	logging.New(cfg)

	// CON-303: error monitoring + OpenTelemetry tracing (Sentry). Must be
	// installed before anything creates OTel spans — the DB pool, the gRPC
	// stack, and especially Genkit, which binds to whatever global
	// TracerProvider exists when it first traces. A no-op when SENTRY_DSN is
	// unset; an init failure is never fatal (fail-open, mirrors analytics).
	telemetryShutdown, err := telemetry.Init(context.Background(), cfg)
	if err != nil {
		slog.Warn("telemetry init failed, disabling (fail-open)",
			logging.AttrComponent, "boot", logging.AttrError, err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := telemetryShutdown(ctx); err != nil {
			slog.Warn("telemetry shutdown", logging.AttrComponent, "boot", logging.AttrError, err)
		}
	}()

	db, err := database.New(cfg.DSN, cfg.Debug)
	if err != nil {
		fatal("connect to database", err)
	}
	db.DB.SetMaxOpenConns(cfg.DBMaxOpenConns)
	db.DB.SetMaxIdleConns(cfg.DBMaxIdleConns)
	defer db.Close()

	if err := database.Migrate(context.Background(), db); err != nil {
		fatal("run migrations", err)
	}

	// CON-86: the isolated analytics (TimescaleDB) pool for vendor_usage_events. When
	// ANALYTICS_DSN is unset, analytics is disabled. A connect/migrate failure
	// at boot is NOT fatal — usage is analytics-grade and must never take down
	// the API (fail-open, FR10) — so we log and proceed with it disabled.
	var analyticsDB *bun.DB
	if cfg.AnalyticsDSN != "" {
		adb, aerr := database.NewAnalytics(cfg.AnalyticsDSN, cfg.Debug)
		switch {
		case aerr != nil:
			slog.Warn("usage analytics connect failed, disabling (fail-open)",
				logging.AttrComponent, "boot", logging.AttrError, aerr)
		default:
			if merr := database.MigrateAnalytics(context.Background(), adb); merr != nil {
				slog.Warn("usage analytics migrate failed, disabling (fail-open)",
					logging.AttrComponent, "boot", logging.AttrError, merr)
				_ = adb.Close()
			} else {
				analyticsDB = adb
				defer analyticsDB.Close()
			}
		}
	}

	// Envelope encryption: load (or generate) the KEK, build a
	// Cipher, then expose Get/Set through SecretStore. Boot fails on
	// any KEK file error — running without an unwrapper is worse than
	// not booting because rotated keys would silently be unrecoverable.
	cipher, kekSrc, err := secrets.InitCipher(cfg.KEKPath)
	if err != nil {
		fatal("init secret cipher", err)
	}
	store := secrets.NewStore(repository.NewSecretRepository(db), cipher)

	bootResult, err := secrets.MigrateFromEnv(context.Background(), store, []secrets.EnvSource{
		{Name: secrets.NameAnthropicAPIKey, EnvValue: cfg.AnthropicAPIKey},
		{Name: secrets.NameZernioAPIKey, EnvValue: cfg.ZernioAPIKey},
		// GEMINI_API_KEY is read straight from the env (it is not a typed Config
		// field) — first-boot seed only; thereafter set/rotated via the gRPC
		// secrets service (CON-104).
		{Name: secrets.NameGeminiAPIKey, EnvValue: os.Getenv("GEMINI_API_KEY")},
		// CON-154 email subsystem: first-boot seed of the Resend send key +
		// webhook signing secret (empty is fine — sending/webhook degrade off).
		{Name: secrets.NameResendAPIKey, EnvValue: cfg.ResendAPIKey},
		{Name: secrets.NameResendWebhookSecret, EnvValue: cfg.ResendWebhookSecret},
		{Name: secrets.NameEmailLinkSecret, EnvValue: cfg.EmailLinkSecret},
		// CON-222 URL assets: first-boot seed of the Firecrawl scrape key (empty
		// is fine — URL ingestion degrades off, the endpoint returns 409).
		{Name: secrets.NameFirecrawlAPIKey, EnvValue: cfg.FirecrawlAPIKey},
	})
	if err != nil {
		fatal("migrate secrets from env", err)
	}
	// CON-154: the unsubscribe-link HMAC key has no operator source; generate and
	// store one on first boot if none was seeded, so one-click unsubscribe works
	// out of the box and the key stays stable across restarts.
	if err := secrets.EnsureGenerated(context.Background(), store, secrets.NameEmailLinkSecret); err != nil {
		fatal("ensure email link secret", err)
	}
	secrets.LogBootSummary(kekSrc, filepath.Join(cfg.KEKPath, secrets.KEKFilename), bootResult)

	// In-process event hub, created here so it is shared by the HTTP server (SSE
	// producers + /api/events) and the internal gRPC server below — an operator
	// tier change over gRPC invalidates a tenant's open tabs on the same bus
	// (CON-295 §4).
	//
	// CON-286: the per-user cap is counted across BOTH SSE streams (/api/events
	// and /api/notifications/stream) and every device/tab. The library default of
	// 10 is too tight once the `activity` feature opens a second stream per tab
	// (2 streams/tab → only ~5 tabs before the cap): past the cap, evict-oldest
	// doesn't settle, it rotates — each tab's reconnect evicts another's, and
	// every eviction triggers a full cache reconcile in the victim. 30 (≥ 2× the
	// tabs a normal person opens) keeps eviction off the normal path while staying
	// a bound on runaway clients.
	hub := eventhub.New(eventhub.Config{MaxSubscribersPerUser: 30})

	app, err := server.New(context.Background(), db, analyticsDB, cfg, store, hub)
	if err != nil {
		fatal("init server", err)
	}

	// Internal operator gRPC surface (secrets management for Harbor). Started
	// only when both an address and an auth token are configured — an empty
	// token keeps it off rather than running unauthenticated. It shares the
	// process lifetime with the HTTP server below; a listen/serve failure is
	// logged but non-fatal so the primary HTTP surface still comes up.
	if cfg.GRPCAddr != "" && cfg.GRPCAuthToken != "" {
		// CON-229: an insert-only River enqueuer for the admin-registration send RPC
		// (NotifyOperatorsTenantRegistered). It lives here because the gRPC server is
		// built independently of the HTTP app that owns the processing client. A
		// build failure degrades to no notifications (nil enqueuer ⇒ soft no-op)
		// rather than blocking the internal gRPC surface.
		var adminEmailEnqueuer grpcserver.AdminEmailEnqueuer
		if enq, eerr := queues.NewInsertOnlyEnqueuer(db); eerr != nil {
			slog.Error("grpc admin-email enqueuer init failed; registration notifications disabled (non-fatal)", logging.AttrComponent, "boot", logging.AttrError, eerr)
		} else {
			adminEmailEnqueuer = enq
		}
		if lis, err := net.Listen("tcp", cfg.GRPCAddr); err != nil {
			slog.Error("grpc listen failed; internal grpc disabled (non-fatal)", logging.AttrComponent, "boot", logging.AttrError, err)
		} else if gs, err := grpcserver.New(
			cfg.GRPCAuthToken, store,
			// CON-208: the operator-facing TenantAdminService reads/writes the
			// global tenant-classification tables. These repos need only the DB
			// pool, so they are built here rather than plumbed out of server.New.
			repository.NewTenantTierRepository(db),
			repository.NewTenantGroupRepository(db),
			repository.NewTenantRepository(db),
			// CON-292: PlatformAdminService operates on the global platform catalog
			// + single-row global limits.
			repository.NewPlatformRepository(db),
			repository.NewPlatformGlobalLimitsRepository(db),
			// CON-294: PlanAdminService (tier-version authoring/assignment) + the
			// SetTenantTier assignment stamp use the tier-version + assignment repos.
			repository.NewTenantTierVersionRepository(db),
			repository.NewTenantTierAssignmentRepository(db),
			// CON-298: EmailAdminService reads a tenant's email history + events and
			// fetches the rendered body live from Resend (per-call key resolution, so
			// a key set/rotated via the secrets API takes effect with no reboot; an
			// unset key = body unavailable, summary + timeline still served).
			repository.NewEmailLogRepository(db),
			repository.NewEmailEventRepository(db),
			// CON-306: the body persisted at send (preferred over the live fetch below).
			repository.NewEmailBodyRepository(db),
			resend.New(func(ctx context.Context) (string, error) { return store.Get(ctx, secrets.NameResendAPIKey) }, cfg.EmailBaseURL, cfg.EmailHTTPTimeout),
			// CON-229: NotifyOperatorsTenantRegistered resolves the tenant owner (user
			// repo), fans out one durable send per operator recipient (insert-only
			// enqueuer), and links to Harbor's tenant page (base URL; empty ⇒ no link).
			repository.NewUserRepository(db),
			adminEmailEnqueuer,
			cfg.HarborBaseURL,
			// CON-230: AnnouncementAdminService (author + measure tenant announcements).
			repository.NewAnnouncementRepository(db),
			// CON-295: the shared event hub, so an operator tier change (SetTenantTier
			// / SetTenantTierVersion) publishes an entitlement-invalidation event that
			// reaches the affected tenant's open tabs.
			hub,
		); err != nil {
			slog.Error("grpc init failed; internal grpc disabled (non-fatal)", logging.AttrComponent, "boot", logging.AttrError, err)
			_ = lis.Close()
		} else {
			slog.Info("internal grpc listening", logging.AttrComponent, "boot", "addr", cfg.GRPCAddr)
			// A loopback bind is only reachable from within this container, so a
			// separate service (e.g. Harbor over Railway's private network) can
			// never connect. Warn rather than fail: a host-run binary legitimately
			// wants loopback (the default), but a containerised deploy almost never
			// does — surfacing it in the boot logs makes the misconfig obvious.
			if grpcAddrIsLoopback(cfg.GRPCAddr) {
				slog.Warn("internal grpc bound to loopback; unreachable from other containers/hosts — set GRPC_ADDR=:9091 to reach it over a private network (e.g. Railway)",
					logging.AttrComponent, "boot", "addr", cfg.GRPCAddr)
			}
			go func() {
				if err := gs.Serve(lis); err != nil {
					slog.Error("internal grpc exited", logging.AttrComponent, "boot", logging.AttrError, err)
				}
			}()
		}
	}

	// CON-301: run the best-effort boot backfills in the background so the HTTP +
	// gRPC listeners come up immediately after migrations instead of waiting out
	// their (up to 2 min each) timeouts inline. Both are idempotent + restart-safe,
	// and serving before they finish is safe by design — the CON-243 entitlement
	// resolver has a tier_id fallback and the activity history self-heals — so a
	// redeploy no longer holds the listeners down and surfaces DeadlineExceeded on
	// callers such as Harbor.
	//
	// bootedAt is captured here, before app.Listen opens the HTTP surface, and
	// bounds the activity backfill's scan: it only migrates post_logs that predate
	// this process, so it can never race the live post-transition path (which
	// writes its own activity event) into a duplicate.
	bootedAt := time.Now()
	go runBootBackfills(db, analyticsDB, bootedAt)

	slog.Info("server listening", logging.AttrComponent, "boot", "addr", cfg.Addr)

	// Run the HTTP server in a goroutine and wait for either a listen failure or
	// a termination signal. app.Listen blocks until the server stops, so serving
	// inline would mean SIGTERM kills the process before the deferred telemetry
	// flush and DB close can run (CON-303). On signal we drain in-flight requests
	// with a bounded grace period, then return so the defers execute.
	serveErr := make(chan error, 1)
	go func() { serveErr <- app.Listen(cfg.Addr) }()

	sigCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	select {
	case err := <-serveErr:
		if err != nil {
			fatal("server exited", err)
		}
	case <-sigCtx.Done():
		stop() // restore default signal handling so a second signal force-quits
		slog.Info("shutdown signal received; draining", logging.AttrComponent, "boot")
		shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := app.ShutdownWithContext(shutCtx); err != nil {
			slog.Error("graceful shutdown failed", logging.AttrComponent, "boot", logging.AttrError, err)
		}
	}
}

// runBootBackfills executes the process's best-effort, non-fatal boot backfills
// off the critical boot path (CON-301). Each runs under its own bounded context
// so a slow DB can't wedge it, and both are idempotent + restart-safe, so a
// redeploy that kills a run mid-flight simply resumes on the next boot.
func runBootBackfills(db, analyticsDB *bun.DB, bootedAt time.Time) {
	const backfillTimeout = 2 * time.Minute

	// CON-125: historical post_logs audit trail → tenant_activity_events (curated
	// to the activity taxonomy). Only runs when usage analytics is enabled. Bounded
	// to rows older than bootedAt so it never races the live path (CON-301).
	if analyticsDB != nil {
		func() {
			ctx, cancel := context.WithTimeout(context.Background(), backfillTimeout)
			defer cancel()
			if n, berr := repository.BackfillPostLogsToActivity(ctx, db, analyticsDB, bootedAt); berr != nil {
				slog.Warn("post_logs → tenant_activity_events backfill failed (non-fatal)",
					logging.AttrComponent, "boot", logging.AttrError, berr)
			} else if n > 0 {
				slog.Info("post_logs migrated to tenant_activity_events",
					logging.AttrComponent, "boot", "rows", n)
			}
		}()
	}

	// CON-243: assign every tenant lacking an open tier-version assignment to its
	// tier's latest active version. The entitlement resolver has a tier_id
	// fallback, so a skipped run only delays the explicit history rows.
	func() {
		ctx, cancel := context.WithTimeout(context.Background(), backfillTimeout)
		defer cancel()
		if rep, berr := repository.BackfillTenantTierAssignments(ctx, db, false); berr != nil {
			slog.Warn("tenant tier-assignment backfill failed (non-fatal)",
				logging.AttrComponent, "boot", logging.AttrError, berr)
		} else if rep.Assigned > 0 || rep.SkippedNoVersion > 0 {
			slog.Info("tenant tier-assignment backfill",
				logging.AttrComponent, "boot",
				"assigned", rep.Assigned, "skipped_no_version", rep.SkippedNoVersion, "candidates", rep.Candidates)
		}
	}()
}

// grpcAddrIsLoopback reports whether addr binds only a loopback interface, i.e.
// it is unreachable from any other container or host. A wildcard bind (empty
// host, as in ":9091") is NOT loopback. Unparseable / hostname addresses return
// false so we never emit a spurious warning.
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

// fatal logs an unrecoverable boot error at ERROR level and exits non-zero.
// slog has no Fatal; this is its idiomatic replacement and, like log.Fatal, it
// intentionally skips deferred cleanup — acceptable for a boot failure.
func fatal(msg string, err error) {
	slog.Error(msg, logging.AttrComponent, "boot", logging.AttrError, err)
	os.Exit(1)
}
