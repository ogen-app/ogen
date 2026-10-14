package database

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"log/slog"

	"github.com/ogen-app/ogen/src/kernel/logging"
)

// migrationLockKey is the Postgres advisory-lock key schema migrations run
// under.
const migrationLockKey int64 = 0x6f67656e_6d690001 // "ogen" "mi" 1

// WithMigrationLock runs fn while holding a session-level Postgres advisory
// lock on one pooled connection, so replicas that boot together migrate one at
// a time: the second waits for the first, then finds nothing left to apply.
// It waits for the lock until ctx ends.
//
// The migrations run on other connections from the same pool, so a pool capped
// at one connection could never run them while the lock holds that
// connection. Such pools (tests and tools that pin session settings) migrate
// without the lock.
func WithMigrationLock(ctx context.Context, db *sql.DB, fn func(context.Context) error) error {
	if db.Stats().MaxOpenConnections == 1 {
		return fn(ctx)
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("migration lock: get connection: %w", err)
	}
	defer func() { _ = conn.Close() }()

	if _, err := conn.ExecContext(ctx, "SELECT pg_advisory_lock($1)", migrationLockKey); err != nil {
		return fmt.Errorf("migration lock: acquire: %w", err)
	}
	defer func() {
		if _, err := conn.ExecContext(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock($1)", migrationLockKey); err != nil {
			slog.WarnContext(ctx, "migration unlock failed; the lock goes with the connection",
				logging.AttrComponent, "db.migrate", logging.AttrError, err)
			// Discard the connection so its session, and the lock, end.
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		}
	}()

	return fn(ctx)
}
