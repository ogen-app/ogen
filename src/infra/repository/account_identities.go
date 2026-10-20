package repository

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
)

// AccountIdentityRepository persists the external sign-in identities linked to
// accounts. Like accounts, identities are not tenant-scoped. Methods taking a
// bun.IDB join the caller's transaction; nil uses the repository's DB.
type AccountIdentityRepository interface {
	// GetBySubject returns the identity a provider's subject is linked through,
	// or sql.ErrNoRows.
	GetBySubject(ctx context.Context, provider, subject string) (*models.AccountIdentity, error)
	// ListForAccount returns an account's identities, oldest first.
	ListForAccount(ctx context.Context, accountID string) ([]models.AccountIdentity, error)
	Create(ctx context.Context, db bun.IDB, identity *models.AccountIdentity) error
	// DeleteForAccount unlinks the account's identity at provider and reports
	// whether one was linked.
	DeleteForAccount(ctx context.Context, db bun.IDB, accountID, provider string) (bool, error)
	// MarkUsed stamps a sign-in through the identity and refreshes the
	// provider's address shown in Settings.
	MarkUsed(ctx context.Context, id, email string, at time.Time) error
}

type accountIdentityRepository struct {
	db *bun.DB
}

// NewAccountIdentityRepository returns a Bun-backed AccountIdentityRepository.
func NewAccountIdentityRepository(db *bun.DB) AccountIdentityRepository {
	return &accountIdentityRepository{db: db}
}

func (r *accountIdentityRepository) idb(db bun.IDB) bun.IDB {
	if db == nil {
		return r.db
	}
	return db
}

func (r *accountIdentityRepository) GetBySubject(ctx context.Context, provider, subject string) (*models.AccountIdentity, error) {
	identity := new(models.AccountIdentity)
	err := r.db.NewSelect().Model(identity).
		Where("ai.provider = ?", provider).
		Where("ai.subject = ?", subject).
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, sql.ErrNoRows
		}
		return nil, err
	}
	return identity, nil
}

func (r *accountIdentityRepository) ListForAccount(ctx context.Context, accountID string) ([]models.AccountIdentity, error) {
	var out []models.AccountIdentity
	err := r.db.NewSelect().Model(&out).
		Where("ai.account_id = ?", accountID).
		OrderExpr("ai.linked_at, ai.id").
		Scan(ctx)
	return out, err
}

func (r *accountIdentityRepository) Create(ctx context.Context, db bun.IDB, identity *models.AccountIdentity) error {
	_, err := r.idb(db).NewInsert().Model(identity).Exec(ctx)
	return err
}

func (r *accountIdentityRepository) DeleteForAccount(ctx context.Context, db bun.IDB, accountID, provider string) (bool, error) {
	res, err := r.idb(db).NewDelete().Model((*models.AccountIdentity)(nil)).
		Where("account_id = ?", accountID).
		Where("provider = ?", provider).
		Exec(ctx)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func (r *accountIdentityRepository) MarkUsed(ctx context.Context, id, email string, at time.Time) error {
	_, err := r.db.NewUpdate().Model((*models.AccountIdentity)(nil)).
		Set("last_used_at = ?", at).
		Set("email = ?", email).
		Where("id = ?", id).
		Exec(ctx)
	return err
}
