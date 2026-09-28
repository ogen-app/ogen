package repository

import (
	"context"
	"strings"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
)

// EmailSuppressionRepository is the "do not mail" list. Entries
// are keyed by lower-cased email address; the repository normalises on both
// write and read so callers don't have to.
type EmailSuppressionRepository interface {
	// Upsert records (or refreshes) a suppression, keyed on (email, scope).
	Upsert(ctx context.Context, s *models.EmailSuppression) error
	// IsSuppressed reports whether mail of the given kind is blocked for email.
	// Marketing is blocked by any entry; transactional is blocked only by an
	// `all`-scope entry (hard bounce / complaint).
	IsSuppressed(ctx context.Context, email string, kind models.EmailKind) (bool, error)
	// RemoveMarketing deletes the marketing suppression for an address
	// (resubscribe, CON-155). Any `all`-scope row (hard bounce / complaint) is
	// left in place. A no-op when no marketing row exists.
	RemoveMarketing(ctx context.Context, email string) error
}

type emailSuppressionRepository struct {
	db *bun.DB
}

// NewEmailSuppressionRepository returns a Bun-backed EmailSuppressionRepository.
func NewEmailSuppressionRepository(db *bun.DB) EmailSuppressionRepository {
	return &emailSuppressionRepository{db: db}
}

// NormalizeEmail lower-cases and trims an address for stable suppression
// matching (email is the global identifier).
func NormalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

func (r *emailSuppressionRepository) Upsert(ctx context.Context, s *models.EmailSuppression) error {
	s.Email = NormalizeEmail(s.Email)
	_, err := r.db.NewInsert().Model(s).
		On("CONFLICT (email, scope) DO UPDATE").
		Set("reason = EXCLUDED.reason").
		Set("source = EXCLUDED.source").
		Exec(ctx)
	return err
}

func (r *emailSuppressionRepository) IsSuppressed(ctx context.Context, email string, kind models.EmailKind) (bool, error) {
	email = NormalizeEmail(email)
	q := r.db.NewSelect().Model((*models.EmailSuppression)(nil)).Where("email = ?", email)
	if kind != models.EmailKindMarketing {
		q = q.Where("scope = ?", models.EmailSuppressionScopeAll)
	}
	return q.Exists(ctx)
}

func (r *emailSuppressionRepository) RemoveMarketing(ctx context.Context, email string) error {
	_, err := r.db.NewDelete().
		Model((*models.EmailSuppression)(nil)).
		Where("email = ?", NormalizeEmail(email)).
		Where("scope = ?", models.EmailSuppressionScopeMarketing).
		Exec(ctx)
	return err
}
