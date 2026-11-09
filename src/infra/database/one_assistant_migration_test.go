package database

import (
	"testing"
)

const (
	oneAssistantUp   = "migrations/20261107000001_one_assistant.up.sql"
	oneAssistantDown = "migrations/20261107000001_one_assistant.down.sql"
)

// TestOneAssistantMigration moves the post conversations into the
// assistant's threads, drops the threads of posts already deleted, and the post writer's model onto the assistant's
// writer unless it has its own. The down migration puts the threads back.
func TestOneAssistantMigration(t *testing.T) {
	ctx := t.Context()
	db, m := throwawayDB(t, "oneassistant")
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
	count := func(query string) int {
		t.Helper()
		var n int
		if err := db.DB.QueryRowContext(ctx, query).Scan(&n); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
		return n
	}

	// The table as it was, with a conversation.
	execFile(oneAssistantDown)
	exec(`INSERT INTO accounts (id, email, password_hash, name) VALUES ('acc', 'o@x.test', 'x', 'Owner')`)
	exec(`INSERT INTO users (id, name, email, tenant_id, account_id) VALUES ('u', 'Owner', 'o@x.test', 'default', 'acc')`)
	exec(`INSERT INTO campaigns_types (id, name, label, is_system, tenant_id, created_at) VALUES ('t', 't', 'T', FALSE, 'default', now())`)
	exec(`INSERT INTO campaigns (id, name, campaign_type_id, created_by, tenant_id) VALUES ('c', 'C', 't', 'u', 'default')`)
	exec(`INSERT INTO posts (id, campaign_id, title, content, status, created_by, tenant_id) VALUES ('p', 'c', 'a', 'Body.', 'draft', 'u', 'default')`)
	exec(`INSERT INTO post_assistant_messages (id, post_id, role, content, tenant_id) VALUES
		('pm1', 'p', 'user', 'shorten it', 'default')`)
	// A thread of a post deleted before.
	exec(`INSERT INTO assistant_messages (id, tenant_id, scope_kind, scope_id, role, content) VALUES ('gm1', 'default', 'post', 'gone', 'user', 'hi')`)
	exec(`INSERT INTO assistant_proposals (id, tenant_id, scope_kind, scope_id, tool, summary, expires_at) VALUES ('go1', 'default', 'post', 'gone', 'schedulePost', 'Schedule it', now())`)
	exec(`DELETE FROM flow_model_config WHERE flow_key = 'assistant' AND slot_key = 'writer'`)
	exec(`INSERT INTO flow_model_config (id, tier_id, flow_key, slot_key, model_id) VALUES
		('w-global', NULL, 'post_assistant', 'writer', 'claude-opus-5-5'),
		('p-global', NULL, 'post_assistant', 'planner', 'claude-haiku-4-5-20251001')`)

	execFile(oneAssistantUp)
	if n := count(`SELECT count(*) FROM assistant_messages WHERE scope_kind = 'post' AND scope_id = 'p' AND tenant_id = 'default'`); n != 1 {
		t.Errorf("post thread = %d messages, want 1", n)
	}
	if n := count(`SELECT count(*) FROM assistant_messages WHERE scope_id = 'gone'`) + count(`SELECT count(*) FROM assistant_proposals WHERE scope_id = 'gone'`); n != 0 {
		t.Errorf("a deleted post's thread kept %d rows", n)
	}
	if n := count(`SELECT count(*) FROM information_schema.tables WHERE table_name = 'post_assistant_messages'`); n != 0 {
		t.Errorf("%d old tables left", n)
	}
	var writer string
	if err := db.DB.QueryRowContext(ctx, `SELECT model_id FROM flow_model_config WHERE flow_key = 'assistant' AND slot_key = 'writer' AND tier_id IS NULL`).Scan(&writer); err != nil || writer != "claude-opus-5-5" {
		t.Errorf("assistant writer = %q (%v), want the post writer's", writer, err)
	}
	if n := count(`SELECT count(*) FROM flow_model_config WHERE flow_key = 'post_assistant'`); n != 0 {
		t.Errorf("%d old model configs left", n)
	}

	execFile(oneAssistantDown)
	if n := count(`SELECT count(*) FROM post_assistant_messages WHERE post_id = 'p'`); n != 1 {
		t.Errorf("after down: post messages = %d, want 1", n)
	}
}
