package database

import (
	"strings"
	"testing"
)

const (
	plannedUp   = "migrations/20261106180001_planned_post_status.up.sql"
	plannedDown = "migrations/20261106180001_planned_post_status.down.sql"
)

// TestPlannedPostStatusBackfill re-runs the planned backfill over posts of
// every kind: only an empty-bodied draft with a draft_thesis note is planned.
// The down migration turns planned posts back into drafts.
func TestPlannedPostStatusBackfill(t *testing.T) {
	ctx := t.Context()
	db, m := throwawayDB(t, "planned")
	if _, err := m.Migrate(ctx); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	exec := func(query string) {
		t.Helper()
		if _, err := db.DB.ExecContext(ctx, query); err != nil {
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
	statuses := func() map[string]string {
		t.Helper()
		rows, err := db.DB.QueryContext(ctx, `SELECT id, status FROM posts`)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = rows.Close() }()
		out := map[string]string{}
		for rows.Next() {
			var id, status string
			if err := rows.Scan(&id, &status); err != nil {
				t.Fatal(err)
			}
			out[id] = status
		}
		return out
	}

	exec(`INSERT INTO accounts (id, email, password_hash, name) VALUES ('acc', 'o@x.test', 'x', 'Owner')`)
	exec(`INSERT INTO users (id, name, email, tenant_id, account_id) VALUES ('u', 'Owner', 'o@x.test', 'default', 'acc')`)
	exec(`INSERT INTO campaigns_types (id, name, label, is_system, tenant_id, created_at) VALUES ('t', 't', 'T', FALSE, 'default', now())`)
	exec(`INSERT INTO campaigns (id, name, campaign_type_id, created_by, tenant_id) VALUES ('c', 'C', 't', 'u', 'default')`)
	exec(`INSERT INTO posts (id, campaign_id, title, content, status, created_by, tenant_id) VALUES
		('theses',    'c', 'a', '',      'draft',             'u', 'default'),
		('blank',     'c', 'b', E' \n\t', 'draft',          'u', 'default'),
		('written',   'c', 'c', 'Body.', 'draft',             'u', 'default'),
		('no_theses', 'c', 'd', '',      'draft',             'u', 'default'),
		('ready',     'c', 'e', '',      'ready_for_publish', 'u', 'default')`)
	exec(`INSERT INTO post_notes (id, post_id, type, body, origin, tenant_id, created_by) VALUES
		('n1', 'theses',  'draft_thesis', '- point', 'content_plan', 'default', 'u'),
		('n2', 'blank',   'draft_thesis', '- point', 'content_plan', 'default', 'u'),
		('n3', 'written', 'draft_thesis', '- point', 'content_plan', 'default', 'u'),
		('n4', 'ready',   'draft_thesis', '- point', 'content_plan', 'default', 'u'),
		('n5', 'no_theses', 'research',   'text',    'manual',       'default', 'u')`)

	// Bodies with a thesis note: a body strings.TrimSpace empties is planned,
	// anything else stays a draft, as models.PostStatus.WithBody decides.
	bodies := map[string]string{
		"nbsp":   "\u00a0",
		"ff_vt":  "\f\v",
		"wide":   "\u0085\u1680\u2000\u200a\u2028\u2029\u202f\u205f\u3000",
		"zwsp":   "\u200b", // not white space to TrimSpace
		"padded": "\u00a0x\u3000",
	}
	for id, body := range bodies {
		if _, err := db.DB.ExecContext(ctx, `INSERT INTO posts (id, campaign_id, title, content, status, created_by, tenant_id) VALUES ($1, 'c', $1, $2, 'draft', 'u', 'default')`, id, body); err != nil {
			t.Fatal(err)
		}
		if _, err := db.DB.ExecContext(ctx, `INSERT INTO post_notes (id, post_id, type, body, origin, tenant_id, created_by) VALUES ('n_' || $1, $1, 'draft_thesis', '- point', 'content_plan', 'default', 'u')`, id); err != nil {
			t.Fatal(err)
		}
	}

	execFile(plannedUp)
	want := map[string]string{
		"theses": "planned", "blank": "planned",
		"written": "draft", "no_theses": "draft", "ready": "ready_for_publish",
	}
	for id, body := range bodies {
		want[id] = "draft"
		if strings.TrimSpace(body) == "" {
			want[id] = "planned"
		}
	}
	got := statuses()
	for id, w := range want {
		if got[id] != w {
			t.Errorf("%s = %s, want %s", id, got[id], w)
		}
	}

	execFile(plannedDown)
	if got := statuses(); got["theses"] != "draft" || got["blank"] != "draft" || got["ready"] != "ready_for_publish" {
		t.Errorf("after down: %v", got)
	}
}
