package database

import (
	"database/sql"
	"testing"
	"time"
)

const (
	con353Up   = "migrations/20261008000353_campaign_posts_changed_at.up.sql"
	con353Down = "migrations/20261008000353_campaign_posts_changed_at.down.sql"
)

// TestCampaignPostsChangedAt checks the CON-353 backfill and trigger: post
// writes a Figma board shows move the campaign's posts_changed_at forward,
// and the others leave it alone.
func TestCampaignPostsChangedAt(t *testing.T) {
	ctx := t.Context()
	db, m := throwawayDB(t, "con353")
	if _, err := m.Migrate(ctx); err != nil {
		t.Fatalf("migrate up: %v", err)
	}

	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := db.DB.ExecContext(ctx, query, args...); err != nil {
			t.Fatalf("exec %q: %v", query, err)
		}
	}
	execFile := func(path string) {
		t.Helper()
		body, err := sqlMigrations.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		exec(string(body))
	}
	changedAt := func(campaign string) sql.NullTime {
		t.Helper()
		var v sql.NullTime
		if err := db.DB.QueryRowContext(ctx, "SELECT posts_changed_at FROM campaigns WHERE id = $1", campaign).Scan(&v); err != nil {
			t.Fatalf("read posts_changed_at of %s: %v", campaign, err)
		}
		return v
	}

	exec(`INSERT INTO accounts (id, email, password_hash, name) VALUES ('acc', 'o@x.test', 'x', 'Owner')`)
	exec(`INSERT INTO users (id, name, email, tenant_id, account_id) VALUES ('u', 'Owner', 'o@x.test', 'default', 'acc')`)
	exec(`INSERT INTO campaigns_types (id, name, is_system, tenant_id) VALUES ('launch', 'launch', FALSE, 'default')`)

	// Backfill: re-run the migration over campaigns that already have posts.
	execFile(con353Down)
	exec(`INSERT INTO campaigns (id, name, campaign_type_id, created_by, tenant_id) VALUES
		('c-old', 'Old', 'launch', 'u', 'default'), ('c-empty', 'Empty', 'launch', 'u', 'default')`)
	exec(`INSERT INTO posts (id, campaign_id, title, created_by, tenant_id, updated_at) VALUES
		('p-old1', 'c-old', 'a', 'u', 'default', '2026-01-01T00:00:00Z'),
		('p-old2', 'c-old', 'b', 'u', 'default', '2026-02-01T00:00:00Z')`)
	execFile(con353Up)
	if got := changedAt("c-old"); !got.Valid || !got.Time.Equal(time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("backfilled posts_changed_at = %v, want the latest post update", got)
	}
	if got := changedAt("c-empty"); got.Valid {
		t.Fatalf("a campaign without posts has posts_changed_at %v, want NULL", got.Time)
	}

	exec(`INSERT INTO campaigns (id, name, campaign_type_id, created_by, tenant_id) VALUES
		('c1', 'One', 'launch', 'u', 'default'), ('c2', 'Two', 'launch', 'u', 'default')`)

	// moves runs a write and reports whether it moved each campaign's stamp.
	moves := func(name, query string, campaigns ...string) {
		t.Helper()
		before := make([]sql.NullTime, len(campaigns))
		for i, c := range campaigns {
			before[i] = changedAt(c)
		}
		exec(query)
		for i, c := range campaigns {
			after := changedAt(c)
			if !after.Valid || before[i].Valid && !after.Time.After(before[i].Time) {
				t.Errorf("%s: %s posts_changed_at %v -> %v, want it moved forward", name, c, before[i], after)
			}
		}
	}
	stays := func(name, query, campaign string) {
		t.Helper()
		before := changedAt(campaign)
		exec(query)
		if after := changedAt(campaign); after != before {
			t.Errorf("%s: posts_changed_at %v -> %v, want it unchanged", name, before, after)
		}
	}

	moves("create", `INSERT INTO posts (id, campaign_id, title, created_by, tenant_id) VALUES ('p1', 'c1', 'Teaser', 'u', 'default')`, "c1")
	stays("edit body", `UPDATE posts SET content = 'new body', updated_at = now() WHERE id = 'p1'`, "c1")
	stays("same schedule", `UPDATE posts SET scheduled_at = NULL WHERE id = 'p1'`, "c1")
	moves("reschedule", `UPDATE posts SET scheduled_at = '2026-11-01T10:00:00Z' WHERE id = 'p1'`, "c1")
	moves("retitle", `UPDATE posts SET title = 'Teaser #1' WHERE id = 'p1'`, "c1")
	moves("change platform", `UPDATE posts SET platform_id = 'rzgpTkARLH0L' WHERE id = 'p1'`, "c1")
	moves("change post type", `UPDATE posts SET platform_post_type = 'story' WHERE id = 'p1'`, "c1")
	moves("change status", `UPDATE posts SET status = 'scheduled' WHERE id = 'p1'`, "c1")
	moves("move campaign", `UPDATE posts SET campaign_id = 'c2' WHERE id = 'p1'`, "c1", "c2")
	moves("delete", `DELETE FROM posts WHERE id = 'p1'`, "c2")

	// Deleting a campaign cascades to its posts; their trigger must not fail
	// on the campaign row being deleted.
	exec(`INSERT INTO posts (id, campaign_id, title, created_by, tenant_id) VALUES ('p2', 'c1', 'x', 'u', 'default')`)
	exec(`DELETE FROM campaigns WHERE id = 'c1'`)
}
