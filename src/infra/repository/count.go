package repository

import (
	"context"

	"github.com/uptrace/bun"
)

// bun runs a model's BeforeSelect hook only for a scanning select: Count,
// Exists and the count half of ScanAndCount skip it, and with it the tenant
// predicate models.TenantScoped adds, so they read every tenant's rows. These
// two scan instead. On a model that isn't TenantScoped they read the same rows
// bun's would, so every count and existence probe goes through them; the lint
// config forbids bun's.

// CountRows counts the rows q selects. q must not order, limit or group.
func CountRows(ctx context.Context, q *bun.SelectQuery) (int, error) {
	var n int
	err := q.ColumnExpr("count(*)").Scan(ctx, &n)
	return n, err
}

// RowsExist reports whether q selects any row. q must not choose columns.
func RowsExist(ctx context.Context, q *bun.SelectQuery) (bool, error) {
	var ones []int
	err := q.ColumnExpr("1").Limit(1).Scan(ctx, &ones)
	return len(ones) > 0, err
}
