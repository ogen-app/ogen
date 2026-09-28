// Package jobs hosts Ogen's background-job machinery. The typed
// queues live in the queues sub-package and are registered against a single
// River (riverqueue/river) client at boot.
//
// River replaced the SQLite-only backlite in CON-87: it is Postgres-native,
// type-safe via Go generics, transaction-aware (an enqueue can join a
// Post-status-change transaction via the shared database/sql pool), and
// provides native retry/backoff plus periodic jobs for the recurring sweeps.
package jobs

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/riverqueue/river/riverdriver/riverdatabasesql"
	"github.com/riverqueue/river/rivermigrate"
)

// MigrateRiver installs or updates River's own tables (river_job,
// river_leader, river_queue, …) on the shared Postgres database. Idempotent;
// safe to run on every boot after the application's bun migrations.
func MigrateRiver(ctx context.Context, db *sql.DB) error {
	migrator, err := rivermigrate.New(riverdatabasesql.New(db), nil)
	if err != nil {
		return fmt.Errorf("jobs: river migrator: %w", err)
	}
	if _, err := migrator.Migrate(ctx, rivermigrate.DirectionUp, nil); err != nil {
		return fmt.Errorf("jobs: river migrate: %w", err)
	}
	return nil
}
