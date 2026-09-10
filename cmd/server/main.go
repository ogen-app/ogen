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
	"path/filepath"
	"time"

	"github.com/uptrace/bun"

	_ "github.com/ogen-app/ogen/docs"
	"github.com/ogen-app/ogen/src/infra/database"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/infra/secrets"
	"github.com/ogen-app/ogen/src/kernel/config"
	"github.com/ogen-app/ogen/src/kernel/logging"
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

				// CON-125: one-time, idempotent backfill into the analytics DB.
				// Best-effort — never fatal to boot. It runs under a bounded
				// context so a slow/unresponsive analytics DB can't hang boot; the
				// backfill is restart-safe, so a timed-out run simply resumes on
				// the next boot.
				const backfillTimeout = 2 * time.Minute

				// Historical post_logs audit trail → tenant_activity_events (curated to
				// the activity taxonomy).
				func() {
					ctx, cancel := context.WithTimeout(context.Background(), backfillTimeout)
					defer cancel()
					if n, berr := repository.BackfillPostLogsToActivity(ctx, db, adb); berr != nil {
						slog.Warn("post_logs → tenant_activity_events backfill failed (non-fatal)",
							logging.AttrComponent, "boot", logging.AttrError, berr)
					} else if n > 0 {
						slog.Info("post_logs migrated to tenant_activity_events",
							logging.AttrComponent, "boot", "rows", n)
					}
				}()
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

	app, err := server.New(context.Background(), db, analyticsDB, cfg, store)
	if err != nil {
		fatal("init server", err)
	}

	// Internal operator gRPC surface (secrets management for Harbor). Started
	// only when both an address and an auth token are configured — an empty
	// token keeps it off rather than running unauthenticated. It shares the
	// process lifetime with the HTTP server below; a listen/serve failure is
	// logged but non-fatal so the primary HTTP surface still comes up.
	if cfg.GRPCAddr != "" && cfg.GRPCAuthToken != "" {
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

	slog.Info("server listening", logging.AttrComponent, "boot", "addr", cfg.Addr)
	if err := app.Listen(cfg.Addr); err != nil {
		fatal("server exited", err)
	}
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
