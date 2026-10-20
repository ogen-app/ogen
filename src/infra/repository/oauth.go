package repository

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
)

// OAuthRepository persists the short-lived rows of a provider sign-in: the
// started-flow state and the pending signup of a newcomer. Both are spent at
// most once by a conditional UPDATE, so two concurrent callers can never both
// use one. Methods taking a bun.IDB join the caller's transaction; nil uses the
// repository's DB.
type OAuthRepository interface {
	CreateState(ctx context.Context, state *models.OAuthState) error
	// ConsumeState spends a pending, unexpired state and returns it.
	// sql.ErrNoRows means it was unknown, already spent or expired.
	ConsumeState(ctx context.Context, stateHash string, now time.Time) (*models.OAuthState, error)

	CreatePendingSignup(ctx context.Context, p *models.OAuthPendingSignup) error
	// GetPendingSignup returns a still-usable pending signup without spending
	// it, or sql.ErrNoRows.
	GetPendingSignup(ctx context.Context, tokenHash string, now time.Time) (*models.OAuthPendingSignup, error)
	// ConsumePendingSignup spends a still-usable pending signup and returns it,
	// or sql.ErrNoRows.
	ConsumePendingSignup(ctx context.Context, db bun.IDB, tokenHash string, now time.Time) (*models.OAuthPendingSignup, error)

	// DeleteExpiredBefore drops states and pending signups that expired before
	// cutoff and reports how many rows went.
	DeleteExpiredBefore(ctx context.Context, cutoff time.Time) (int, error)
}

type oauthRepository struct {
	db *bun.DB
}

// NewOAuthRepository returns a Bun-backed OAuthRepository.
func NewOAuthRepository(db *bun.DB) OAuthRepository {
	return &oauthRepository{db: db}
}

func (r *oauthRepository) CreateState(ctx context.Context, state *models.OAuthState) error {
	_, err := r.db.NewInsert().Model(state).Exec(ctx)
	return err
}

func (r *oauthRepository) ConsumeState(ctx context.Context, stateHash string, now time.Time) (*models.OAuthState, error) {
	state := new(models.OAuthState)
	err := r.db.NewUpdate().Model(state).
		Set("consumed_at = ?", now).
		Where("state_hash = ?", stateHash).
		Where("consumed_at IS NULL").
		Where("expires_at > ?", now).
		Returning("*").
		Scan(ctx)
	return noRows(state, err)
}

func (r *oauthRepository) CreatePendingSignup(ctx context.Context, p *models.OAuthPendingSignup) error {
	_, err := r.db.NewInsert().Model(p).Exec(ctx)
	return err
}

func (r *oauthRepository) GetPendingSignup(ctx context.Context, tokenHash string, now time.Time) (*models.OAuthPendingSignup, error) {
	p := new(models.OAuthPendingSignup)
	err := r.db.NewSelect().Model(p).
		Where("token_hash = ?", tokenHash).
		Where("consumed_at IS NULL").
		Where("expires_at > ?", now).
		Scan(ctx)
	return noRows(p, err)
}

func (r *oauthRepository) ConsumePendingSignup(ctx context.Context, db bun.IDB, tokenHash string, now time.Time) (*models.OAuthPendingSignup, error) {
	if db == nil {
		db = r.db
	}
	p := new(models.OAuthPendingSignup)
	err := db.NewUpdate().Model(p).
		Set("consumed_at = ?", now).
		Where("token_hash = ?", tokenHash).
		Where("consumed_at IS NULL").
		Where("expires_at > ?", now).
		Returning("*").
		Scan(ctx)
	return noRows(p, err)
}

func (r *oauthRepository) DeleteExpiredBefore(ctx context.Context, cutoff time.Time) (int, error) {
	total := 0
	for _, model := range []any{(*models.OAuthState)(nil), (*models.OAuthPendingSignup)(nil)} {
		res, err := r.db.NewDelete().Model(model).Where("expires_at < ?", cutoff).Exec(ctx)
		if err != nil {
			return total, err
		}
		n, _ := res.RowsAffected()
		total += int(n)
	}
	return total, nil
}

// noRows returns v, or sql.ErrNoRows unwrapped when the query matched nothing.
func noRows[T any](v *T, err error) (*T, error) {
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, sql.ErrNoRows
		}
		return nil, err
	}
	return v, nil
}
