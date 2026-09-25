package database

import (
	"testing"
)

const (
	con314Up   = "migrations/20260925000314_campaign_types_tenant_scope.up.sql"
	con314Down = "migrations/20260925000314_campaign_types_tenant_scope.down.sql"
)

// TestCampaignTypesTenantScopeBackfill runs the CON-314 backfill over pre-fix
// data: migrate everything, step the CON-314 migration back down, seed custom
// types the old global way, then re-apply its up SQL and check ownership.
//
// Seed:
//   - "launch" is used by workspaces a (earliest campaign) and b, whose posts
//     and phase plans sit on its phases;
//   - "solo" is used by workspace c only, and so is "SOLO" (created later) —
//     legal under the old case-sensitive UNIQUE(name);
//   - "unused" is used by nobody.
func TestCampaignTypesTenantScopeBackfill(t *testing.T) {
	ctx := t.Context()
	db, m := throwawayDB(t, "con314")
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
	scanString := func(query string) string {
		t.Helper()
		var s string
		if err := db.DB.QueryRowContext(ctx, query).Scan(&s); err != nil {
			t.Fatalf("query %q: %v", query, err)
		}
		return s
	}
	scanInt := func(query string) int {
		t.Helper()
		var n int
		if err := db.DB.QueryRowContext(ctx, query).Scan(&n); err != nil {
			t.Fatalf("query %q: %v", query, err)
		}
		return n
	}

	execFile(con314Down)

	exec(`INSERT INTO tenants (id, name, slug, tier_id) VALUES
		('ta', 'A', 'a', 'default'), ('tb', 'B', 'b', 'default'), ('tc', 'C', 'c', 'default')`)
	exec(`INSERT INTO accounts (id, email, password_hash, name) VALUES ('acc', 'o@x.test', 'x', 'Owner')`)
	exec(`INSERT INTO users (id, name, email, tenant_id, account_id) VALUES ('u', 'Owner', 'o@x.test', 'default', 'acc')`)
	exec(`INSERT INTO campaigns_types (id, name, label, is_system, created_at) VALUES
		('launch', 'launch', 'Launch', FALSE, now()), ('solo', 'solo', 'Solo', FALSE, now() - interval '1 day'),
		('solo_caps', 'SOLO', 'Solo', FALSE, now()), ('unused', 'unused', 'Unused', FALSE, now())`)
	exec(`INSERT INTO campaigns_types_phases (id, campaign_type_id, name, sequence) VALUES
		('launch1', 'launch', 'Tease', 1), ('launch2', 'launch', 'Reveal', 2), ('solo1', 'solo', 'Only', 1)`)
	exec(`INSERT INTO campaigns (id, name, campaign_type_id, created_by, tenant_id, created_at, deleted_at) VALUES
		('ca', 'A launch', 'launch', 'u', 'ta', now() - interval '2 days', NULL),
		('cb', 'B launch', 'launch', 'u', 'tb', now() - interval '1 day', NULL),
		('cb_deleted', 'B old launch', 'launch', 'u', 'tb', now(), now()),
		('cc', 'C solo', 'solo', 'u', 'tc', now(), NULL),
		('cc_caps', 'C SOLO', 'solo_caps', 'u', 'tc', now(), NULL)`)
	exec(`INSERT INTO posts (id, campaign_id, title, created_by, tenant_id, campaign_type_phase_id) VALUES
		('pa', 'ca', 'a', 'u', 'ta', 'launch1'),
		('pb', 'cb', 'b', 'u', 'tb', 'launch2'),
		('pc', 'cc', 'c', 'u', 'tc', 'solo1')`)
	exec(`INSERT INTO campaign_phase_windows (campaign_id, phase_id, tenant_id, start_date, end_date) VALUES
		('ca', 'launch1', 'ta', '2026-01-01', '2026-01-10'), ('ca', 'launch2', 'ta', '2026-01-11', '2026-01-20'),
		('cb', 'launch1', 'tb', '2026-02-01', '2026-02-10'), ('cb', 'launch2', 'tb', '2026-02-11', '2026-02-20')`)

	execFile(con314Up)

	// Ownership.
	if got := scanString(`SELECT tenant_id FROM campaigns_types WHERE id = 'launch'`); got != "ta" {
		t.Errorf("launch owner = %q, want ta (earliest campaign)", got)
	}
	if got := scanString(`SELECT tenant_id FROM campaigns_types WHERE id = 'solo'`); got != "tc" {
		t.Errorf("solo owner = %q, want tc", got)
	}
	if got := scanString(`SELECT tenant_id FROM campaigns_types WHERE id = 'unused'`); got != "default" {
		t.Errorf("unused owner = %q, want default", got)
	}
	if n := scanInt(`SELECT count(*) FROM campaigns_types WHERE is_system AND tenant_id IS NOT NULL`); n != 0 {
		t.Errorf("%d system types got an owner", n)
	}

	// b got its own copy of launch, under the same name, with the same phases.
	clone := scanString(`SELECT id FROM campaigns_types WHERE tenant_id = 'tb' AND name = 'launch'`)
	if n := scanInt(`SELECT count(*) FROM campaigns_types WHERE name = 'launch'`); n != 2 {
		t.Errorf("launch copies = %d, want 2 (original + b's clone)", n)
	}
	if got := scanString(`SELECT string_agg(name || ':' || sequence, ',' ORDER BY sequence)
		FROM campaigns_types_phases WHERE campaign_type_id = '` + clone + `'`); got != "Tease:1,Reveal:2" {
		t.Errorf("clone phases = %q, want Tease:1,Reveal:2", got)
	}

	// b's campaigns (the soft-deleted one too), post phase and phase plan moved
	// to the clone; a's stayed on the original.
	if n := scanInt(`SELECT count(*) FROM campaigns WHERE tenant_id = 'tb' AND campaign_type_id = '` + clone + `'`); n != 2 {
		t.Errorf("b campaigns on the clone = %d, want 2", n)
	}
	if got := scanString(`SELECT ph.name FROM posts p JOIN campaigns_types_phases ph ON ph.id = p.campaign_type_phase_id
		WHERE p.id = 'pb' AND ph.campaign_type_id = '` + clone + `'`); got != "Reveal" {
		t.Errorf("pb phase = %q on the clone, want Reveal", got)
	}
	if got := scanString(`SELECT campaign_type_phase_id FROM posts WHERE id = 'pa'`); got != "launch1" {
		t.Errorf("pa phase = %q, want launch1", got)
	}
	if n := scanInt(`SELECT count(*) FROM campaign_phase_windows w JOIN campaigns_types_phases ph ON ph.id = w.phase_id
		WHERE w.campaign_id = 'cb' AND ph.campaign_type_id = '` + clone + `'`); n != 2 {
		t.Errorf("cb windows on the clone = %d, want 2", n)
	}
	if n := scanInt(`SELECT count(*) FROM campaign_phase_windows WHERE campaign_id = 'ca' AND phase_id IN ('launch1', 'launch2')`); n != 2 {
		t.Errorf("ca windows on the original = %d, want 2", n)
	}

	// No campaign, post phase or phase plan references another workspace's type.
	if n := scanInt(`SELECT count(*) FROM campaigns c JOIN campaigns_types ct ON ct.id = c.campaign_type_id
		WHERE ct.tenant_id IS NOT NULL AND ct.tenant_id <> c.tenant_id`); n != 0 {
		t.Errorf("%d campaigns use another workspace's type", n)
	}
	if n := scanInt(`SELECT count(*) FROM posts p JOIN campaigns c ON c.id = p.campaign_id
		JOIN campaigns_types_phases ph ON ph.id = p.campaign_type_phase_id
		WHERE ph.campaign_type_id <> c.campaign_type_id`); n != 0 {
		t.Errorf("%d posts sit on a phase outside their campaign's type", n)
	}
	if n := scanInt(`SELECT count(*) FROM campaign_phase_windows w JOIN campaigns c ON c.id = w.campaign_id
		JOIN campaigns_types_phases ph ON ph.id = w.phase_id
		WHERE ph.campaign_type_id <> c.campaign_type_id`); n != 0 {
		t.Errorf("%d phase windows sit on a phase outside their campaign's type", n)
	}

	// Names are unique per workspace ignoring case: the newer of the two names
	// differing only by case was suffixed, and a third spelling is rejected.
	if got := scanString(`SELECT name FROM campaigns_types WHERE id = 'solo'`); got != "solo" {
		t.Errorf("older solo renamed to %q, want it kept", got)
	}
	if got := scanString(`SELECT name FROM campaigns_types WHERE id = 'solo_caps'`); got != "SOLO (solo_caps)" {
		t.Errorf("newer SOLO name = %q, want %q", got, "SOLO (solo_caps)")
	}
	if _, err := db.DB.ExecContext(ctx, `INSERT INTO campaigns_types (id, name, is_system, tenant_id)
		VALUES ('solo_again', 'Solo', FALSE, 'tc')`); err == nil {
		t.Error("a case-only duplicate name in one workspace was accepted")
	}
	if _, err := db.DB.ExecContext(ctx, `INSERT INTO campaigns_types (id, name, is_system, tenant_id)
		VALUES ('solo_other', 'Solo', FALSE, 'ta')`); err != nil {
		t.Errorf("the same name in another workspace was rejected: %v", err)
	}

	// The CON-166 integrity triggers are back on.
	if n := scanInt(`SELECT count(*) FROM pg_trigger
		WHERE tgname IN ('campaigns_type_locked', 'posts_phase_matches_campaign_type_upd') AND tgenabled = 'O'`); n != 2 {
		t.Errorf("enabled CON-166 triggers = %d, want 2", n)
	}
	if _, err := db.DB.ExecContext(ctx, `UPDATE campaigns SET campaign_type_id = 'solo' WHERE id = 'ca'`); err == nil {
		t.Error("type change on a phased campaign succeeded; campaigns_type_locked is not enforcing")
	}
}
