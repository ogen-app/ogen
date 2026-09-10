package database_test

import (
	"context"
	"testing"

	"github.com/ogen-app/ogen/src/infra/database"
	"github.com/ogen-app/ogen/src/pgtest"
)

// postsPublishedURLColumnCount reports how many `published_url` columns the
// posts table has (0 or 1). Used to assert the column exists after migration.
func postsPublishedURLColumnCount(ctx context.Context, t *testing.T) int {
	t.Helper()
	db := pgtest.MustDB()
	// MustDB has already run the full migration chain; verify the end state,
	// then reuse the same DB to exercise the fixup's repair path below.
	var n int
	if err := db.NewSelect().
		ColumnExpr("count(*)").
		TableExpr("information_schema.columns").
		Where("table_name = 'posts' AND column_name = 'published_url'").
		Scan(ctx, &n); err != nil {
		t.Fatalf("query information_schema: %v", err)
	}
	return n
}

// TestPublishedURLColumnPresentAfterMigrate guards the CON-165 regression that
// took reconcile_scheduled_posts (and every full posts SELECT) down in prod
// with "column po.published_url does not exist": the original 20260903000001
// migration could half-apply — its ADD COLUMN rolled back with a failing
// backfill while bun recorded the migration as applied — leaving the column
// permanently missing. The chain must always end with the column present.
func TestPublishedURLColumnPresentAfterMigrate(t *testing.T) {
	ctx := t.Context()
	if got := postsPublishedURLColumnCount(ctx, t); got != 1 {
		t.Fatalf("posts.published_url column count after migrate = %d, want 1", got)
	}
}

// TestSourceAnchorColumnsPresentAfterMigrate guards CON-280's additive migration
// (20260910000003): document-service chunks are persisted with a human citation
// (source_label) and structured location (source_anchor jsonb) on assets_chunks.
// Both are nullable and CON-281/282 build on them, so the chain must always end
// with the columns present.
func TestSourceAnchorColumnsPresentAfterMigrate(t *testing.T) {
	ctx := t.Context()
	db := pgtest.MustDB() // has already run the full migration chain
	for _, col := range []string{"source_label", "source_anchor"} {
		var n int
		if err := db.NewSelect().
			ColumnExpr("count(*)").
			TableExpr("information_schema.columns").
			Where("table_name = 'assets_chunks' AND column_name = ?", col).
			Scan(ctx, &n); err != nil {
			t.Fatalf("query information_schema for %s: %v", col, err)
		}
		if n != 1 {
			t.Fatalf("assets_chunks.%s column count after migrate = %d, want 1", col, n)
		}
	}
}

// TestPublishedURLFixupRepairsMissingColumn simulates the broken-prod state
// (column dropped, fixup migration un-recorded) and re-runs the migrator,
// asserting the 20260908000001 fixup idempotently re-adds the column through
// bun's real runner. Running with no posts rows also proves the DO-block
// backfill is a safe no-op on an empty table.
func TestPublishedURLFixupRepairsMissingColumn(t *testing.T) {
	ctx := t.Context()
	db := pgtest.MustDB()

	if _, err := db.ExecContext(ctx, "ALTER TABLE posts DROP COLUMN published_url"); err != nil {
		t.Fatalf("drop column: %v", err)
	}
	// Un-record the fixup so Migrate treats it as pending again.
	if _, err := db.ExecContext(ctx,
		"DELETE FROM bun_migrations WHERE name LIKE '20260908000001%'"); err != nil {
		t.Fatalf("delete migration record: %v", err)
	}

	if err := database.Migrate(ctx, db); err != nil {
		t.Fatalf("re-run migrate: %v", err)
	}

	var n int
	if err := db.NewSelect().
		ColumnExpr("count(*)").
		TableExpr("information_schema.columns").
		Where("table_name = 'posts' AND column_name = 'published_url'").
		Scan(ctx, &n); err != nil {
		t.Fatalf("query information_schema: %v", err)
	}
	if n != 1 {
		t.Fatalf("posts.published_url column count after fixup = %d, want 1", n)
	}
}
