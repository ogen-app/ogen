package database

import (
	"strings"
	"testing"
)

// TestPostsCampaignAndDueIndexes checks the planner can serve the
// per-campaign reads and the scheduled-post sweeps from the new indexes. Seq
// scans are disabled because an empty table would otherwise always win.
func TestPostsCampaignAndDueIndexes(t *testing.T) {
	ctx := t.Context()
	db, m := throwawayDB(t, "postsidx")
	if _, err := m.Migrate(ctx); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("pin connection: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if _, err := conn.ExecContext(ctx, "SET enable_seqscan = off"); err != nil {
		t.Fatalf("disable seq scans: %v", err)
	}

	cases := map[string]string{
		"idx_posts_campaign": "SELECT id FROM posts WHERE campaign_id IN ('c1', 'c2') ORDER BY scheduled_at",
		"idx_posts_due": "SELECT id FROM posts WHERE status = 'scheduled' AND scheduled_at IS NOT NULL" +
			" AND scheduled_at < now() ORDER BY scheduled_at LIMIT 100",
	}
	for index, query := range cases {
		rows, err := conn.QueryContext(ctx, "EXPLAIN "+query)
		if err != nil {
			t.Fatalf("explain %q: %v", query, err)
		}
		var plan strings.Builder
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				t.Fatalf("scan plan: %v", err)
			}
			plan.WriteString(line + "\n")
		}
		if err := rows.Close(); err != nil {
			t.Fatalf("close plan rows: %v", err)
		}
		if !strings.Contains(plan.String(), index) {
			t.Errorf("plan for %q does not use %s:\n%s", query, index, plan.String())
		}
	}
}
