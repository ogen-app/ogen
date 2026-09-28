package repository

import (
	"context"

	"github.com/uptrace/bun"
)

// WorkspaceListItem is one row of the workspace switcher: a workspace
// the account belongs to, with the caller's role and the workspace's size. role
// and is_default are caller-relative (they come from the membership / session),
// so this can't be a plain SELECT over tenants — it's a join over memberships.
type WorkspaceListItem struct {
	ID          string `bun:"id"          json:"id"`
	Name        string `bun:"name"        json:"name"`
	Slug        string `bun:"slug"        json:"slug"`
	Role        string `bun:"role"        json:"role"`
	MemberCount int    `bun:"member_count" json:"member_count"`
	IsDefault   bool   `bun:"-"           json:"is_default"`
}

// WorkspaceRepository reads the set of workspaces an account can reach. It is
// deliberately account-scoped rather than tenant-scoped — it spans every tenant
// the account is a member of — so it does not use the TenantScoped query layer.
type WorkspaceRepository interface {
	// ListForAccount returns every workspace accountID is a member of, with the
	// caller's role and the member count, ordered oldest-first.
	ListForAccount(ctx context.Context, accountID string) ([]WorkspaceListItem, error)
	// ListForAccountTx is ListForAccount on the provided bun.IDB, so a caller can
	// read an account's live workspaces inside its own transaction — e.g. the
	// last-workspace guard in workspace delete, which must count against the
	// soft-delete it just made in the same tx. Passing nil uses the default DB.
	ListForAccountTx(ctx context.Context, tx bun.IDB, accountID string) ([]WorkspaceListItem, error)
}

type workspaceRepository struct {
	db *bun.DB
}

// NewWorkspaceRepository returns a Bun-backed WorkspaceRepository.
func NewWorkspaceRepository(db *bun.DB) WorkspaceRepository {
	return &workspaceRepository{db: db}
}

func (r *workspaceRepository) ListForAccount(ctx context.Context, accountID string) ([]WorkspaceListItem, error) {
	return r.ListForAccountTx(ctx, nil, accountID)
}

func (r *workspaceRepository) ListForAccountTx(ctx context.Context, tx bun.IDB, accountID string) ([]WorkspaceListItem, error) {
	db := bun.IDB(r.db)
	if tx != nil {
		db = tx
	}
	var items []WorkspaceListItem
	// Join the account's memberships to their tenants; member_count is a
	// correlated subquery over users so it counts every member of each workspace,
	// not just the caller. account_id is the only filter — this read intentionally
	// crosses tenants, so it bypasses the tenant-scoped model layer.
	err := db.NewSelect().
		TableExpr("users AS u").
		Join("JOIN tenants AS t ON t.id = u.tenant_id").
		ColumnExpr("t.id AS id").
		ColumnExpr("t.name AS name").
		ColumnExpr("t.slug AS slug").
		ColumnExpr("u.role AS role").
		ColumnExpr("(SELECT count(*) FROM users m WHERE m.tenant_id = t.id) AS member_count").
		Where("u.account_id = ?", accountID).
		Where("t.deleted_at IS NULL"). // a soft-deleted workspace leaves every member's list
		OrderExpr("t.created_at ASC").
		Scan(ctx, &items)
	if err != nil {
		return nil, err
	}
	return items, nil
}
