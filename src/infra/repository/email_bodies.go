package repository

import (
	"context"
	"database/sql"
	"errors"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
)

// EmailBodyRepository persists and serves the rendered body captured at send
// time (CON-306). It is a 1:1 side table of email_logs; the body is written
// best-effort after the parent log row and served in preference to the live
// Resend fetch by the operator Emails tab (CON-192 / CON-298).
type EmailBodyRepository interface {
	// Insert stores one rendered body. It is idempotent on the primary key
	// (email_log_id): a redundant write — e.g. a retried job that already logged
	// and stored this send — is a no-op rather than a duplicate-key error, so the
	// caller can treat it as best-effort.
	Insert(ctx context.Context, b *models.EmailBody) error
	// GetByEmailLogID returns the stored body for a log, or (nil, nil) when none
	// was persisted (e.g. a row sent before CON-306 shipped) so the caller falls
	// back to the live Resend fetch.
	GetByEmailLogID(ctx context.Context, emailLogID string) (*models.EmailBody, error)
}

type emailBodyRepository struct {
	db *bun.DB
}

// NewEmailBodyRepository returns a Bun-backed EmailBodyRepository.
func NewEmailBodyRepository(db *bun.DB) EmailBodyRepository {
	return &emailBodyRepository{db: db}
}

func (r *emailBodyRepository) Insert(ctx context.Context, b *models.EmailBody) error {
	if b == nil || b.EmailLogID == "" {
		return nil
	}
	_, err := r.db.NewInsert().
		Model(b).
		On("CONFLICT (email_log_id) DO NOTHING").
		Exec(ctx)
	return err
}

func (r *emailBodyRepository) GetByEmailLogID(ctx context.Context, emailLogID string) (*models.EmailBody, error) {
	if emailLogID == "" {
		return nil, nil
	}
	b := new(models.EmailBody)
	err := r.db.NewSelect().
		Model(b).
		Where("email_log_id = ?", emailLogID).
		Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return b, nil
}
