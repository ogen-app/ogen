package repository

import (
	"context"
	"database/sql"
	"errors"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
)

// EmailTemplateRepository is the persistence surface for editable email
// templates. The DB is the source of truth; defaults are seeded on
// boot via InsertIfAbsent so operator edits are never clobbered.
type EmailTemplateRepository interface {
	GetByKey(ctx context.Context, key string) (*models.EmailTemplate, error)
	// InsertIfAbsent inserts the template only when no row with its key exists
	// yet. Returns true when a row was created (seeded), false when one was
	// already present (operator copy preserved). Idempotent — safe every boot.
	InsertIfAbsent(ctx context.Context, t *models.EmailTemplate) (bool, error)
	// SyncVariables refreshes the code-owned `variables` docs for a template,
	// leaving operator-edited copy (subject/html/text) untouched. A no-op when
	// the key doesn't exist. Called by the boot seeder so the docs always track
	// the code, even for rows seeded before a variable was added.
	SyncVariables(ctx context.Context, key string, vars models.StringMap) error
	List(ctx context.Context) ([]models.EmailTemplate, error)
}

type emailTemplateRepository struct {
	db *bun.DB
}

// NewEmailTemplateRepository returns a Bun-backed EmailTemplateRepository.
func NewEmailTemplateRepository(db *bun.DB) EmailTemplateRepository {
	return &emailTemplateRepository{db: db}
}

func (r *emailTemplateRepository) GetByKey(ctx context.Context, key string) (*models.EmailTemplate, error) {
	t := new(models.EmailTemplate)
	err := r.db.NewSelect().Model(t).Where("et.key = ?", key).Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, sql.ErrNoRows
		}
		return nil, err
	}
	return t, nil
}

func (r *emailTemplateRepository) InsertIfAbsent(ctx context.Context, t *models.EmailTemplate) (bool, error) {
	res, err := r.db.NewInsert().Model(t).On("CONFLICT (key) DO NOTHING").Exec(ctx)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func (r *emailTemplateRepository) SyncVariables(ctx context.Context, key string, vars models.StringMap) error {
	if vars == nil {
		vars = models.StringMap{}
	}
	_, err := r.db.NewUpdate().
		Model((*models.EmailTemplate)(nil)).
		Set("variables = ?", vars).
		Where("key = ?", key).
		Exec(ctx)
	return err
}

func (r *emailTemplateRepository) List(ctx context.Context) ([]models.EmailTemplate, error) {
	var out []models.EmailTemplate
	if err := r.db.NewSelect().Model(&out).OrderExpr("key ASC").Scan(ctx); err != nil {
		return nil, err
	}
	return out, nil
}
