package database

import (
	"database/sql"
	"fmt"
	"time"

	// Registers the "pgx" database/sql driver. bun runs on the resulting
	// *sql.DB; the same pool (exposed as db.DB) is shared with the River job
	// queue so bun and River draw from one connection pool.
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/extra/bundebug"
	"github.com/uptrace/bun/extra/bunotel"
)

// Default connection-pool sizing. Postgres removes SQLite's single-writer
// ceiling, so we open a real pool. cmd/server overrides these from config.
const (
	defaultMaxOpenConns = 25
	defaultMaxIdleConns = 5

	// Recycle backends periodically so each server-side process's private
	// caches (relcache/catcache/CacheMemoryContext, prepared plans) can't grow
	// unbounded over a long-lived connection — a common cause of a slowly
	// climbing Postgres memory line. Idle backends are released sooner still.
	defaultConnMaxLifetime = 30 * time.Minute
	defaultConnMaxIdleTime = 5 * time.Minute
)

// New opens a Postgres connection pool via the pgx stdlib driver and wraps it
// with bun using the Postgres dialect.
func New(dsn string, debug bool) (*bun.DB, error) {
	sqldb, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("open postgres: %w", err)
	}

	sqldb.SetMaxOpenConns(defaultMaxOpenConns)
	sqldb.SetMaxIdleConns(defaultMaxIdleConns)
	sqldb.SetConnMaxLifetime(defaultConnMaxLifetime)
	sqldb.SetConnMaxIdleTime(defaultConnMaxIdleTime)

	db := bun.NewDB(sqldb, pgdialect.New())

	// CON-303: per-query OpenTelemetry spans (operation + table + placeholder SQL,
	// never bound arg values — bunotel's default omits them). A no-op unless a
	// real TracerProvider is installed (telemetry.Init runs before this), and the
	// parentless-client sampler drops any query that runs outside a request/job
	// trace, so this only surfaces DB work under an actual trace.
	db.AddQueryHook(bunotel.NewQueryHook(bunotel.WithDBName("ogen")))

	if debug {
		db.AddQueryHook(bundebug.NewQueryHook(bundebug.WithVerbose(true)))
	}

	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("ping database: %w", err)
	}

	return db, nil
}
