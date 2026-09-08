package database

import (
	"context"
	"embed"
	"fmt"
	"log/slog"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/migrate"

	"github.com/ogen-app/ogen/src/kernel/logging"
)

//go:embed migrations/*.sql
var sqlMigrations embed.FS

// Migrate applies all pending SQL migrations on every startup.
// Applied migrations are recorded in the bun_migrations table and never re-run.
func Migrate(ctx context.Context, db *bun.DB) error {
	migrations := migrate.NewMigrations()
	if err := migrations.Discover(sqlMigrations); err != nil {
		return fmt.Errorf("discover migrations: %w", err)
	}

	// WithMarkAppliedOnSuccess: bun's default marks a migration applied BEFORE
	// running its Up. A migration whose Up then errors (e.g. a non-transactional
	// multi-statement file that rolls back) is left recorded as applied but not
	// actually applied — the deploy dies once, restarts skip the "applied"
	// migration, and the schema silently drifts (this is exactly how CON-165's
	// published_url column went missing in prod). Marking only on success makes a
	// failing migration fail loud and retry instead of being lost.
	migrator := migrate.NewMigrator(db, migrations, migrate.WithMarkAppliedOnSuccess(true))

	if err := migrator.Init(ctx); err != nil {
		return fmt.Errorf("init migrator: %w", err)
	}

	group, err := migrator.Migrate(ctx)
	if err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}

	if group.IsZero() {
		slog.InfoContext(ctx, "no new migrations to apply", logging.AttrComponent, "db.migrate")
		return nil
	}

	slog.InfoContext(ctx, "applied migration group", logging.AttrComponent, "db.migrate", "group", group)
	return nil
}
