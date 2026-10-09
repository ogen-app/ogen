package database

import (
	"testing"
	"time"
)

// TestArgFreeQueriesSkipPreparedStatements checks that bun's interpolated
// queries run on the simple protocol (no server-side prepared statement is
// left on the connection) while parameterised queries still bind arguments,
// including []byte into jsonb and time.Time into timestamptz.
func TestArgFreeQueriesSkipPreparedStatements(t *testing.T) {
	ctx := t.Context()
	db, _ := throwawayDB(t, "simpleproto")

	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("pin connection: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	for i := range 5 {
		var got int
		if err := conn.NewSelect().ColumnExpr("?::int", i).Scan(ctx, &got); err != nil {
			t.Fatalf("select %d: %v", i, err)
		}
		if got != i {
			t.Fatalf("select = %d, want %d", got, i)
		}
	}

	var prepared int
	if err := conn.QueryRowContext(ctx, "SELECT count(*) FROM pg_prepared_statements").Scan(&prepared); err != nil {
		t.Fatalf("count prepared statements: %v", err)
	}
	// The count query above is itself argument-free, so a cached-statement
	// mode would show at least six entries here.
	if prepared != 0 {
		t.Fatalf("prepared statements = %d, want 0", prepared)
	}

	now := time.Now().UTC().Truncate(time.Microsecond)
	var (
		key string
		at  time.Time
	)
	// Through the raw *sql.Conn, as River sends it: bun's own QueryRowContext
	// formats ? placeholders itself and would bind nothing.
	err = conn.Conn.QueryRowContext(ctx, "SELECT $1::jsonb->>'k', $2::timestamptz", []byte(`{"k":"v"}`), now).Scan(&key, &at)
	if err != nil {
		t.Fatalf("parameterised query: %v", err)
	}
	if key != "v" || !at.Equal(now) {
		t.Fatalf("parameterised query = (%q, %v), want (\"v\", %v)", key, at, now)
	}
}
