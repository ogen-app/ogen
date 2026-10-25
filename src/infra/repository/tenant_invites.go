package repository

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
)

// ErrTenantInviteNotPending: the invite exists but is redeemed or revoked.
var ErrTenantInviteNotPending = errors.New("tenant invite is not pending")

// TenantInviteFilter selects one page of the operator invite list, newest
// first. Status is a derived status (pending excludes expired invites);
// EmailQuery matches a substring of the address, case-insensitively.
type TenantInviteFilter struct {
	Status     string
	EmailQuery string
	Limit      int
	// AfterCreatedAt/AfterID is the keyset cursor: the last row of the
	// previous page. Zero starts from the newest.
	AfterCreatedAt time.Time
	AfterID        string
	// Now decides expiry.
	Now time.Time
}

// TenantInviteRepository persists tester invites. The table is global (an
// invite has no tenant until it is redeemed); methods taking a bun.IDB join the
// caller's transaction, nil meaning the repository's own handle.
type TenantInviteRepository interface {
	// CreateReplacingPendingTx inserts inv with its groups. A pending invite
	// for the same address is revoked first, so its link stops working;
	// replaced reports that. An advisory lock on the address serialises
	// concurrent invites to it.
	CreateReplacingPendingTx(ctx context.Context, db bun.IDB, inv *models.TenantInvite, groupIDs []string) (replaced bool, err error)
	// Get returns sql.ErrNoRows when absent.
	Get(ctx context.Context, id string) (*models.TenantInvite, error)
	// GetView returns the invite with its names and delivery state.
	GetView(ctx context.Context, id string) (*models.TenantInviteView, error)
	List(ctx context.Context, f TenantInviteFilter) ([]models.TenantInviteView, error)
	// GetUsable returns the pending, unexpired invite with this token hash,
	// or sql.ErrNoRows for any other.
	GetUsable(ctx context.Context, tokenHash string, now time.Time) (*models.TenantInvite, error)
	// ConsumeTx spends a usable invite (pending -> redeemed) and returns it;
	// sql.ErrNoRows for an unknown, expired, revoked or redeemed token, so the
	// caller can answer one indistinguishable 410.
	ConsumeTx(ctx context.Context, db bun.IDB, tokenHash string, now time.Time) (*models.TenantInvite, error)
	// RecordRedemptionTx stores the workspace and account a consumed invite
	// produced.
	RecordRedemptionTx(ctx context.Context, db bun.IDB, id, tenantID, accountID string, now time.Time) error
	GroupIDs(ctx context.Context, db bun.IDB, inviteID string) ([]string, error)
	// ResendTx gives a pending invite (expired included) a new token and
	// expiry and counts the send. ErrTenantInviteNotPending for a redeemed or
	// revoked one; sql.ErrNoRows when absent.
	ResendTx(ctx context.Context, db bun.IDB, id, tokenHash string, expiresAt, now time.Time) (*models.TenantInvite, error)
	// Revoke marks a pending invite revoked. Same errors as ResendTx.
	Revoke(ctx context.Context, id string, now time.Time) (*models.TenantInvite, error)
}

type tenantInviteRepository struct {
	db *bun.DB
}

// NewTenantInviteRepository returns a Bun-backed TenantInviteRepository.
func NewTenantInviteRepository(db *bun.DB) TenantInviteRepository {
	return &tenantInviteRepository{db: db}
}

func (r *tenantInviteRepository) idb(db bun.IDB) bun.IDB {
	if db == nil {
		return r.db
	}
	return db
}

func (r *tenantInviteRepository) CreateReplacingPendingTx(ctx context.Context, db bun.IDB, inv *models.TenantInvite, groupIDs []string) (bool, error) {
	db = r.idb(db)
	if _, err := db.NewRaw("SELECT pg_advisory_xact_lock(hashtext('tenant_invite'), hashtext(?))", inv.Email).Exec(ctx); err != nil {
		return false, err
	}
	res, err := db.NewUpdate().Model((*models.TenantInvite)(nil)).
		Set("status = ?", models.TenantInviteRevoked).
		Set("revoked_at = ?", inv.CreatedAt).
		Set("updated_at = ?", inv.CreatedAt).
		Where("email = ?", inv.Email).
		Where("status = ?", models.TenantInvitePending).
		Exec(ctx)
	if err != nil {
		return false, err
	}
	replaced, _ := res.RowsAffected()
	if _, err := db.NewInsert().Model(inv).Exec(ctx); err != nil {
		return false, err
	}
	if len(groupIDs) > 0 {
		rows := make([]models.TenantInviteGroup, 0, len(groupIDs))
		for _, g := range groupIDs {
			rows = append(rows, models.TenantInviteGroup{InviteID: inv.ID, GroupID: g})
		}
		if _, err := db.NewInsert().Model(&rows).On("CONFLICT DO NOTHING").Exec(ctx); err != nil {
			return false, err
		}
	}
	return replaced > 0, nil
}

func (r *tenantInviteRepository) Get(ctx context.Context, id string) (*models.TenantInvite, error) {
	inv := new(models.TenantInvite)
	if err := r.db.NewSelect().Model(inv).Where("ti.id = ?", id).Scan(ctx); err != nil {
		return nil, err
	}
	return inv, nil
}

// tenantInviteRow is an invite with the joined columns the operator view needs.
type tenantInviteRow struct {
	models.TenantInvite `bun:",extend"`

	TierName                    string     `bun:"tier_name,scanonly"`
	TierPurchasable             bool       `bun:"tier_purchasable,scanonly"`
	EmailStatus                 string     `bun:"email_status,scanonly"`
	EmailLastEventAt            *time.Time `bun:"email_last_event_at,scanonly"`
	RedeemedTenantName          string     `bun:"redeemed_tenant_name,scanonly"`
	RedeemedTenantComplimentary bool       `bun:"redeemed_tenant_complimentary,scanonly"`
}

// viewQuery selects invites with their tier, the latest send of the invite
// email and the workspace they became. Each send is keyed
// tenant_invite:<id>:<send> and sent_count numbers the latest, so its log row
// is one equality lookup on the unique idempotency-key index.
func (r *tenantInviteRepository) viewQuery(rows *[]tenantInviteRow) *bun.SelectQuery {
	return r.db.NewSelect().Model(rows).
		ColumnExpr("ti.*").
		ColumnExpr("COALESCE(tt.name, '') AS tier_name").
		ColumnExpr(`EXISTS (SELECT 1 FROM tenant_tier_versions AS v
			WHERE v.tier_id = ti.tier_id AND v.status = ? AND v.purchasable) AS tier_purchasable`, models.TierVersionStatusActive).
		ColumnExpr("COALESCE(em.status, '') AS email_status").
		ColumnExpr("em.at AS email_last_event_at").
		ColumnExpr("COALESCE(rt.name, '') AS redeemed_tenant_name").
		ColumnExpr("COALESCE(rt.complimentary, false) AS redeemed_tenant_complimentary").
		Join("LEFT JOIN tenant_tiers AS tt ON tt.id = ti.tier_id").
		Join("LEFT JOIN tenants AS rt ON rt.id = ti.redeemed_tenant_id").
		Join(`LEFT JOIN LATERAL (
			SELECT COALESCE(el.last_event, el.status) AS status, COALESCE(el.last_event_at, el.created_at) AS at
			FROM email_logs AS el
			WHERE el.idempotency_key = 'tenant_invite:' || ti.id || ':' || ti.sent_count
			LIMIT 1) AS em ON true`)
}

func (r *tenantInviteRepository) GetView(ctx context.Context, id string) (*models.TenantInviteView, error) {
	var rows []tenantInviteRow
	if err := r.viewQuery(&rows).Where("ti.id = ?", id).Scan(ctx); err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, sql.ErrNoRows
	}
	views, err := r.views(ctx, rows)
	if err != nil {
		return nil, err
	}
	return &views[0], nil
}

func (r *tenantInviteRepository) List(ctx context.Context, f TenantInviteFilter) ([]models.TenantInviteView, error) {
	var rows []tenantInviteRow
	q := r.viewQuery(&rows).OrderExpr("ti.created_at DESC, ti.id DESC").Limit(f.Limit)
	switch f.Status {
	case models.TenantInvitePending:
		q = q.Where("ti.status = ? AND ti.expires_at > ?", models.TenantInvitePending, f.Now)
	case models.TenantInviteExpired:
		q = q.Where("ti.status = ? AND ti.expires_at <= ?", models.TenantInvitePending, f.Now)
	case models.TenantInviteRedeemed, models.TenantInviteRevoked:
		q = q.Where("ti.status = ?", f.Status)
	}
	if s := strings.TrimSpace(f.EmailQuery); s != "" {
		q = q.Where(`ti.email ILIKE ? ESCAPE '\'`, "%"+escapeLike(s)+"%")
	}
	if !f.AfterCreatedAt.IsZero() {
		q = q.Where("(ti.created_at, ti.id) < (?, ?)", f.AfterCreatedAt, f.AfterID)
	}
	if err := q.Scan(ctx); err != nil {
		return nil, err
	}
	return r.views(ctx, rows)
}

// views hydrates each row's groups in one query.
func (r *tenantInviteRepository) views(ctx context.Context, rows []tenantInviteRow) ([]models.TenantInviteView, error) {
	out := make([]models.TenantInviteView, len(rows))
	if len(rows) == 0 {
		return out, nil
	}
	ids := make([]string, len(rows))
	for i := range rows {
		ids[i] = rows[i].ID
	}
	var groups []struct {
		InviteID string `bun:"invite_id"`
		models.TenantGroup
	}
	if err := r.db.NewSelect().TableExpr("tenant_invite_groups AS tig").
		ColumnExpr("tig.invite_id, tg.*").
		Join("JOIN tenant_groups AS tg ON tg.id = tig.group_id").
		Where("tig.invite_id IN (?)", bun.List(ids)).
		OrderExpr("tg.name ASC").
		Scan(ctx, &groups); err != nil {
		return nil, err
	}
	byInvite := map[string][]models.TenantGroup{}
	for _, g := range groups {
		byInvite[g.InviteID] = append(byInvite[g.InviteID], g.TenantGroup)
	}
	for i, row := range rows {
		out[i] = models.TenantInviteView{
			TenantInvite: row.TenantInvite, TierName: row.TierName, TierPurchasable: row.TierPurchasable,
			Groups: byInvite[row.ID], EmailStatus: row.EmailStatus, EmailLastEventAt: row.EmailLastEventAt,
			RedeemedTenantName: row.RedeemedTenantName, RedeemedTenantComplimentary: row.RedeemedTenantComplimentary,
		}
	}
	return out, nil
}

func (r *tenantInviteRepository) GetUsable(ctx context.Context, tokenHash string, now time.Time) (*models.TenantInvite, error) {
	inv := new(models.TenantInvite)
	err := r.db.NewSelect().Model(inv).
		Where("ti.token_hash = ?", tokenHash).
		Where("ti.status = ?", models.TenantInvitePending).
		Where("ti.expires_at > ?", now).
		Scan(ctx)
	if err != nil {
		return nil, err
	}
	return inv, nil
}

func (r *tenantInviteRepository) ConsumeTx(ctx context.Context, db bun.IDB, tokenHash string, now time.Time) (*models.TenantInvite, error) {
	inv := new(models.TenantInvite)
	err := r.idb(db).NewUpdate().Model(inv).
		Set("status = ?", models.TenantInviteRedeemed).
		Set("redeemed_at = ?", now).
		Set("updated_at = ?", now).
		Where("token_hash = ?", tokenHash).
		Where("status = ?", models.TenantInvitePending).
		Where("expires_at > ?", now).
		Returning("*").
		Scan(ctx)
	if err != nil {
		return nil, err
	}
	return inv, nil
}

func (r *tenantInviteRepository) RecordRedemptionTx(ctx context.Context, db bun.IDB, id, tenantID, accountID string, now time.Time) error {
	_, err := r.idb(db).NewUpdate().Model((*models.TenantInvite)(nil)).
		Set("redeemed_tenant_id = ?", tenantID).
		Set("redeemed_account_id = ?", accountID).
		Set("updated_at = ?", now).
		Where("id = ?", id).
		Exec(ctx)
	return err
}

func (r *tenantInviteRepository) GroupIDs(ctx context.Context, db bun.IDB, inviteID string) ([]string, error) {
	var ids []string
	err := r.idb(db).NewSelect().Model((*models.TenantInviteGroup)(nil)).
		Column("group_id").
		Where("invite_id = ?", inviteID).
		OrderExpr("group_id ASC").
		Scan(ctx, &ids)
	return ids, err
}

func (r *tenantInviteRepository) ResendTx(ctx context.Context, db bun.IDB, id, tokenHash string, expiresAt, now time.Time) (*models.TenantInvite, error) {
	inv := new(models.TenantInvite)
	err := r.idb(db).NewUpdate().Model(inv).
		Set("token_hash = ?", tokenHash).
		Set("expires_at = ?", expiresAt).
		Set("sent_count = sent_count + 1").
		Set("last_sent_at = ?", now).
		Set("updated_at = ?", now).
		Where("id = ?", id).
		Where("status = ?", models.TenantInvitePending).
		Returning("*").
		Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, r.notPendingOrMissing(ctx, db, id)
	}
	if err != nil {
		return nil, err
	}
	return inv, nil
}

func (r *tenantInviteRepository) Revoke(ctx context.Context, id string, now time.Time) (*models.TenantInvite, error) {
	inv := new(models.TenantInvite)
	err := r.db.NewUpdate().Model(inv).
		Set("status = ?", models.TenantInviteRevoked).
		Set("revoked_at = ?", now).
		Set("updated_at = ?", now).
		Where("id = ?", id).
		Where("status = ?", models.TenantInvitePending).
		Returning("*").
		Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, r.notPendingOrMissing(ctx, nil, id)
	}
	if err != nil {
		return nil, err
	}
	return inv, nil
}

// notPendingOrMissing tells an absent invite (sql.ErrNoRows) from one that is
// no longer pending.
func (r *tenantInviteRepository) notPendingOrMissing(ctx context.Context, db bun.IDB, id string) error {
	exists, err := r.idb(db).NewSelect().Model((*models.TenantInvite)(nil)).Where("id = ?", id).Exists(ctx)
	if err != nil {
		return err
	}
	if !exists {
		return sql.ErrNoRows
	}
	return ErrTenantInviteNotPending
}
