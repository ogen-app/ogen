package server

import (
	"context"
	"database/sql/driver"
	"log/slog"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/kernel/logging"
)

// zernioSyncLockKey is the Postgres advisory-lock key the Zernio sync sweep
// runs under.
const zernioSyncLockKey int64 = 0x6f67656e_7a730001 // "ogen" "zs" 1

// advisoryLock returns a try-lock over a session-level Postgres advisory lock
// held on one pooled connection, so work guarded by it runs on one replica at
// a time. A replica that can't take the lock (another holds it, or the
// database is unreachable) skips the work; a failed lock query is logged, since
// it skips the work on every replica.
//
// The queries go through bun.Conn, which formats the arguments into the SQL
// itself and sends none to the driver, so they take bun's ? placeholder: a $1
// reaches Postgres unbound and the lock query always fails.
func advisoryLock(db *bun.DB, key int64) func(context.Context) (release func(), ok bool) {
	return func(ctx context.Context) (func(), bool) {
		conn, err := db.Conn(ctx)
		if err != nil {
			slog.WarnContext(ctx, "advisory lock: get connection failed",
				logging.AttrComponent, "server", logging.AttrError, err)
			return nil, false
		}
		var locked bool
		if err := conn.QueryRowContext(ctx, "SELECT pg_try_advisory_lock(?)", key).Scan(&locked); err != nil || !locked {
			if err != nil {
				slog.WarnContext(ctx, "advisory lock query failed",
					logging.AttrComponent, "server", logging.AttrError, err)
			}
			_ = conn.Close()
			return nil, false
		}
		return func() {
			if _, err := conn.ExecContext(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock(?)", key); err != nil {
				slog.WarnContext(ctx, "advisory unlock failed; the lock goes with the connection",
					logging.AttrComponent, "server", logging.AttrError, err)
				// Discard the connection so its session, and the lock, end.
				_ = conn.Raw(func(any) error { return driver.ErrBadConn })
			}
			_ = conn.Close()
		}, true
	}
}
