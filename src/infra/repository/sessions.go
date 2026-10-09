package repository

import (
	"context"
	"database/sql"
	"errors"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
)

// SessionRepository defines all persistence operations for the Session domain.
type SessionRepository interface {
	Create(ctx context.Context, session *models.Session) error
	// CreateTx inserts a session on the provided bun.IDB so it can join an outer
	// transaction (e.g. invitation accept creates the user + session atomically).
	// Passing nil falls back to the repository's default DB.
	CreateTx(ctx context.Context, tx bun.IDB, session *models.Session) error
	// CreateForCredential inserts a login's session only if the account's
	// password hash is still verifiedHash, the one the password was checked
	// against. It reads the hash under a share lock on the account row, so a
	// password reset, change or secure-account action either commits first (and
	// the login is refused) or waits until the session exists (and then revokes
	// it). Reports whether the session was created.
	CreateForCredential(ctx context.Context, session *models.Session, verifiedHash string) (bool, error)
	GetByID(ctx context.Context, id string) (*models.Session, error)
	// GetForAuth loads a session together with the account's membership of the
	// workspace a request acts in (workspaceID, or the session's stored default
	// when empty), in one query. The membership is nil when the account has no
	// membership there or the workspace is not active.
	GetForAuth(ctx context.Context, id, workspaceID string) (*models.Session, *Membership, error)
	// SetDefaultWorkspace repoints a session's stored default workspace (CON-147
	// switch): it moves user_id + tenant_id to the given membership/workspace so a
	// fresh tab or the next login seeds there. It does NOT scope live requests —
	// those resolve per request from the X-Workspace-Id header — so the cookie
	// stays valid and other tabs are unaffected.
	SetDefaultWorkspace(ctx context.Context, sessionID, userID, tenantID string) error
	Delete(ctx context.Context, id string) (bool, error)
	// DeleteAllForAccount revokes every session of an account except
	// exceptSessionID ("" keeps none), returning how many it removed. db lets it
	// join the caller's transaction; nil uses the repository's DB.
	DeleteAllForAccount(ctx context.Context, db bun.IDB, accountID, exceptSessionID string) (int, error)
}

type sessionRepository struct {
	db *bun.DB
}

// NewSessionRepository returns a Bun-backed SessionRepository.
func NewSessionRepository(db *bun.DB) SessionRepository {
	return &sessionRepository{db: db}
}

func (r *sessionRepository) Create(ctx context.Context, session *models.Session) error {
	return r.CreateTx(ctx, nil, session)
}

func (r *sessionRepository) CreateTx(ctx context.Context, tx bun.IDB, session *models.Session) error {
	db := bun.IDB(r.db)
	if tx != nil {
		db = tx
	}
	_, err := db.NewInsert().Model(session).Exec(ctx)
	return err
}

func (r *sessionRepository) CreateForCredential(ctx context.Context, session *models.Session, verifiedHash string) (bool, error) {
	created := false
	err := r.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		var current string
		err := tx.NewSelect().Table("accounts").Column("password_hash").
			Where("id = ?", session.AccountID).For("SHARE").Scan(ctx, &current)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if current != verifiedHash {
			return nil
		}
		if _, err := tx.NewInsert().Model(session).Exec(ctx); err != nil {
			return err
		}
		created = true
		return nil
	})
	return created, err
}

func (r *sessionRepository) GetByID(ctx context.Context, id string) (*models.Session, error) {
	session := new(models.Session)
	err := r.db.NewSelect().Model(session).Where("s.id = ?", id).Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, sql.ErrNoRows
		}
		return nil, err
	}
	return session, nil
}

// Membership is the account's user row in one workspace, as far as request
// authentication needs it.
type Membership struct {
	UserID   string
	TenantID string
}

type sessionWithMembership struct {
	models.Session `bun:",extend"`

	MemberID       string `bun:"member_id,scanonly"`
	MemberTenantID string `bun:"member_tenant_id,scanonly"`
}

func (r *sessionRepository) GetForAuth(ctx context.Context, id, workspaceID string) (*models.Session, *Membership, error) {
	// Unscoped for the same reason as userRepository.GetMembership: this lookup
	// is what authorises scoping the request to a tenant. The tenants predicate
	// matches GetMembership's, so a suspended or soft-deleted workspace yields no
	// membership.
	row := new(sessionWithMembership)
	err := r.db.NewSelect().Model(row).
		ColumnExpr("s.*").
		ColumnExpr("COALESCE(u.id, '') AS member_id").
		ColumnExpr("COALESCE(u.tenant_id, '') AS member_tenant_id").
		Join(`LEFT JOIN users AS u ON u.account_id = s.account_id
			AND u.tenant_id = COALESCE(NULLIF(?, ''), s.tenant_id)
			AND EXISTS (SELECT 1 FROM tenants AS t WHERE t.id = u.tenant_id AND t.status = ?)`,
			workspaceID, models.TenantStatusActive).
		Where("s.id = ?", id).
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil, sql.ErrNoRows
		}
		return nil, nil, err
	}
	session := row.Session
	if row.MemberID == "" {
		return &session, nil, nil
	}
	return &session, &Membership{UserID: row.MemberID, TenantID: row.MemberTenantID}, nil
}

func (r *sessionRepository) SetDefaultWorkspace(ctx context.Context, sessionID, userID, tenantID string) error {
	_, err := r.db.NewUpdate().Model((*models.Session)(nil)).
		Set("user_id = ?", userID).
		Set("tenant_id = ?", tenantID).
		Where("id = ?", sessionID).
		Exec(ctx)
	return err
}

func (r *sessionRepository) Delete(ctx context.Context, id string) (bool, error) {
	res, err := r.db.NewDelete().Model((*models.Session)(nil)).Where("id = ?", id).Exec(ctx)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func (r *sessionRepository) DeleteAllForAccount(ctx context.Context, db bun.IDB, accountID, exceptSessionID string) (int, error) {
	if db == nil {
		db = r.db
	}
	q := db.NewDelete().Model((*models.Session)(nil)).Where("account_id = ?", accountID)
	if exceptSessionID != "" {
		q = q.Where("id != ?", exceptSessionID)
	}
	res, err := q.Exec(ctx)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}
