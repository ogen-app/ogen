package database

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"log/slog"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/extra/bundebug"
	"github.com/uptrace/bun/migrate"

	"github.com/ogen-app/ogen/src/kernel/logging"
)

//go:embed migrations_analytics/*.sql
var analyticsMigrations embed.FS

// The analytics pool is small: writes are batched + async (one buffered
// writer) and reads are a handful of rollup queries, so it needs far fewer
// connections than the request-serving main pool.
const (
	defaultAnalyticsMaxOpenConns = 10
	defaultAnalyticsMaxIdleConns = 2

	// Recycle backends on the same cadence as the main pool so per-connection
	// server-side caches can't accumulate over long-lived analytics connections.
	defaultAnalyticsConnMaxLifetime = 30 * time.Minute
	defaultAnalyticsConnMaxIdleTime = 5 * time.Minute
)

// NewAnalytics opens the isolated analytics database (TimescaleDB in prod, a
// plain Postgres in tests/dev) used for vendor_usage_events (CON-86 D4). It is a
// separate handle from New: the control-plane Postgres stays vanilla, and a
// failure here must never take down the request path — callers treat an
// error as "analytics disabled" and proceed (fail-open, CON-86 FR10).
func NewAnalytics(dsn string, debug bool) (*bun.DB, error) {
	sqldb, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("open analytics postgres: %w", err)
	}

	sqldb.SetMaxOpenConns(defaultAnalyticsMaxOpenConns)
	sqldb.SetMaxIdleConns(defaultAnalyticsMaxIdleConns)
	sqldb.SetConnMaxLifetime(defaultAnalyticsConnMaxLifetime)
	sqldb.SetConnMaxIdleTime(defaultAnalyticsConnMaxIdleTime)

	db := bun.NewDB(sqldb, pgdialect.New())

	// Close the pool on any early return below (e.g. a failed Ping); disarmed
	// once we successfully hand the handle to the caller, who then owns it.
	ok := false
	defer func() {
		if !ok {
			db.Close()
		}
	}()

	if debug {
		db.AddQueryHook(bundebug.NewQueryHook(bundebug.WithVerbose(true)))
	}

	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("ping analytics database: %w", err)
	}

	ok = true
	return db, nil
}

// MigrateAnalytics applies the analytics-DB migrations. Their applied state is
// tracked in a dedicated bun_migrations_analytics table (not the main
// bun_migrations) so the analytics schema can share a Postgres instance with
// the control plane in dev without the two migration histories colliding.
func MigrateAnalytics(ctx context.Context, db *bun.DB) error {
	migrations := migrate.NewMigrations()
	if err := migrations.Discover(analyticsMigrations); err != nil {
		return fmt.Errorf("discover analytics migrations: %w", err)
	}

	// WithMarkAppliedOnSuccess: mark a migration applied only after its Up
	// succeeds, so a failed migration retries on the next boot instead of being
	// silently recorded as applied while its DDL rolled back. See the note in
	// Migrate (migrate.go) — analytics migration failures are non-fatal, so a
	// silent skip here is even easier to miss.
	migrator := migrate.NewMigrator(db, migrations,
		migrate.WithTableName("bun_migrations_analytics"),
		migrate.WithLocksTableName("bun_migration_locks_analytics"),
		migrate.WithMarkAppliedOnSuccess(true),
	)

	if err := migrator.Init(ctx); err != nil {
		return fmt.Errorf("init analytics migrator: %w", err)
	}

	group, err := migrator.Migrate(ctx)
	if err != nil {
		return fmt.Errorf("apply analytics migrations: %w", err)
	}

	if group.IsZero() {
		slog.InfoContext(ctx, "no new migrations to apply", logging.AttrComponent, "db.analytics")
		return nil
	}

	slog.InfoContext(ctx, "applied migration group", logging.AttrComponent, "db.analytics", "group", group)
	return nil
}
