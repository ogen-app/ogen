package repository

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
)

// AccountRepository persists the login identity. Accounts are not
// TenantScoped — authentication resolves an account before any tenant is known,
// exactly like the users/sessions lookups in the auth path.
type AccountRepository interface {
	// GetByEmail resolves the account for a login/reset/invite by its unique
	// email. Returns sql.ErrNoRows when no account has that address.
	GetByEmail(ctx context.Context, email string) (*models.Account, error)
	GetByID(ctx context.Context, id string) (*models.Account, error)
	Create(ctx context.Context, account *models.Account) error
	// CreateTx inserts an account on the provided bun.IDB so it can join an outer
	// transaction (e.g. signup creates account + membership + session atomically).
	// Passing nil falls back to the repository's default DB.
	CreateTx(ctx context.Context, tx bun.IDB, account *models.Account) error
	// UpdatePassword sets the credential (password change, reset). The account is
	// the sole source of the password since CON-147 PR1.
	UpdatePassword(ctx context.Context, id, passwordHash string) error
	UpdatePasswordTx(ctx context.Context, tx bun.IDB, id, passwordHash string) error
}

type accountRepository struct {
	db *bun.DB
}

// NewAccountRepository returns a Bun-backed AccountRepository.
func NewAccountRepository(db *bun.DB) AccountRepository {
	return &accountRepository{db: db}
}

func (r *accountRepository) GetByEmail(ctx context.Context, email string) (*models.Account, error) {
	account := new(models.Account)
	err := r.db.NewSelect().Model(account).Where("a.email = ?", email).Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, sql.ErrNoRows
		}
		return nil, err
	}
	return account, nil
}

func (r *accountRepository) GetByID(ctx context.Context, id string) (*models.Account, error) {
	account := new(models.Account)
	err := r.db.NewSelect().Model(account).Where("a.id = ?", id).Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, sql.ErrNoRows
		}
		return nil, err
	}
	return account, nil
}

func (r *accountRepository) Create(ctx context.Context, account *models.Account) error {
	return r.CreateTx(ctx, nil, account)
}

func (r *accountRepository) CreateTx(ctx context.Context, tx bun.IDB, account *models.Account) error {
	db := bun.IDB(r.db)
	if tx != nil {
		db = tx
	}
	_, err := db.NewInsert().Model(account).Exec(ctx)
	return err
}

func (r *accountRepository) UpdatePassword(ctx context.Context, id, passwordHash string) error {
	return r.UpdatePasswordTx(ctx, nil, id, passwordHash)
}

func (r *accountRepository) UpdatePasswordTx(ctx context.Context, tx bun.IDB, id, passwordHash string) error {
	db := bun.IDB(r.db)
	if tx != nil {
		db = tx
	}
	_, err := db.NewUpdate().Model((*models.Account)(nil)).
		Set("password_hash = ?", passwordHash).
		Set("updated_at = ?", time.Now().UTC()).
		Where("id = ?", id).
		Exec(ctx)
	return err
}
