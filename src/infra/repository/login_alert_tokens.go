package repository

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
)

// LoginAlertTokenRepository persists the single-use tokens behind new-device
// alert emails. Methods taking a bun.IDB join the caller's transaction; nil
// uses the repository's DB.
type LoginAlertTokenRepository interface {
	Create(ctx context.Context, db bun.IDB, t *models.LoginAlertToken) error
	// CountSince returns how many alerts the account was sent since the cutoff;
	// it backs the per-account hourly cap.
	CountSince(ctx context.Context, db bun.IDB, accountID string, since time.Time) (int, error)
	// GetByHash returns the token or sql.ErrNoRows.
	GetByHash(ctx context.Context, tokenHash string) (*models.LoginAlertToken, error)
	// Consume spends a pending, unexpired token in one conditional UPDATE and
	// returns it. sql.ErrNoRows means it was unknown, already spent or expired,
	// so two concurrent callers can never both spend it.
	Consume(ctx context.Context, db bun.IDB, tokenHash string, now time.Time) (*models.LoginAlertToken, error)
	// VoidPending spends every other still-pending token of the account.
	VoidPending(ctx context.Context, db bun.IDB, accountID string, now time.Time) (int, error)
	// DeleteCreatedBefore drops tokens created before cutoff.
	DeleteCreatedBefore(ctx context.Context, cutoff time.Time) (int, error)
}

type loginAlertTokenRepository struct {
	db *bun.DB
}

// NewLoginAlertTokenRepository returns a Bun-backed LoginAlertTokenRepository.
func NewLoginAlertTokenRepository(db *bun.DB) LoginAlertTokenRepository {
	return &loginAlertTokenRepository{db: db}
}

func (r *loginAlertTokenRepository) idb(db bun.IDB) bun.IDB {
	if db == nil {
		return r.db
	}
	return db
}

func (r *loginAlertTokenRepository) Create(ctx context.Context, db bun.IDB, t *models.LoginAlertToken) error {
	_, err := r.idb(db).NewInsert().Model(t).Exec(ctx)
	return err
}

func (r *loginAlertTokenRepository) CountSince(ctx context.Context, db bun.IDB, accountID string, since time.Time) (int, error) {
	return r.idb(db).NewSelect().Model((*models.LoginAlertToken)(nil)).
		Where("account_id = ?", accountID).
		Where("created_at >= ?", since).
		Count(ctx)
}

func (r *loginAlertTokenRepository) GetByHash(ctx context.Context, tokenHash string) (*models.LoginAlertToken, error) {
	t := new(models.LoginAlertToken)
	if err := r.db.NewSelect().Model(t).Where("token_hash = ?", tokenHash).Scan(ctx); err != nil {
		return nil, err
	}
	return t, nil
}

func (r *loginAlertTokenRepository) Consume(ctx context.Context, db bun.IDB, tokenHash string, now time.Time) (*models.LoginAlertToken, error) {
	t := new(models.LoginAlertToken)
	err := r.idb(db).NewUpdate().Model(t).
		Set("consumed_at = ?", now).
		Where("token_hash = ?", tokenHash).
		Where("consumed_at IS NULL").
		Where("expires_at > ?", now).
		Returning("*").
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, sql.ErrNoRows
		}
		return nil, err
	}
	return t, nil
}

func (r *loginAlertTokenRepository) VoidPending(ctx context.Context, db bun.IDB, accountID string, now time.Time) (int, error) {
	res, err := r.idb(db).NewUpdate().Model((*models.LoginAlertToken)(nil)).
		Set("consumed_at = ?", now).
		Where("account_id = ?", accountID).
		Where("consumed_at IS NULL").
		Exec(ctx)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

func (r *loginAlertTokenRepository) DeleteCreatedBefore(ctx context.Context, cutoff time.Time) (int, error) {
	res, err := r.db.NewDelete().Model((*models.LoginAlertToken)(nil)).
		Where("created_at < ?", cutoff).
		Exec(ctx)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}
