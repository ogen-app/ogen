package repository

import (
	"context"
	"time"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
)

// AssistantDismissal is a finding the user dismissed in a thread.
type AssistantDismissal struct {
	bun.BaseModel `bun:"table:assistant_dismissals,alias:adm"`
	models.TenantScoped

	ScopeKind  models.AssistantScopeKind `bun:"scope_kind,pk"`
	ScopeID    string                    `bun:"scope_id,pk"`
	FindingKey string                    `bun:"finding_key,pk"`
	CreatedBy  string                    `bun:"created_by,notnull"`
	CreatedAt  time.Time                 `bun:"created_at,notnull,default:current_timestamp"`
}

// AssistantDismissalRepository remembers dismissed findings per thread.
type AssistantDismissalRepository interface {
	// Dismiss records a dismissal; dismissing twice is one.
	Dismiss(ctx context.Context, kind models.AssistantScopeKind, scopeID, key, by string) error
	// Keys returns the thread's dismissed finding keys.
	Keys(ctx context.Context, kind models.AssistantScopeKind, scopeID string) (map[string]bool, error)
	// List returns the thread's dismissals, the latest first.
	List(ctx context.Context, kind models.AssistantScopeKind, scopeID string) ([]AssistantDismissal, error)
	// Restore takes a dismissal back, so checks raise the finding again;
	// false when it wasn't dismissed there.
	Restore(ctx context.Context, kind models.AssistantScopeKind, scopeID, key string) (bool, error)
}

type assistantDismissalRepository struct {
	db *bun.DB
}

// NewAssistantDismissalRepository returns an AssistantDismissalRepository.
func NewAssistantDismissalRepository(db *bun.DB) AssistantDismissalRepository {
	return &assistantDismissalRepository{db: db}
}

func (r *assistantDismissalRepository) Dismiss(ctx context.Context, kind models.AssistantScopeKind, scopeID, key, by string) error {
	_, err := r.db.NewInsert().Model(&AssistantDismissal{ScopeKind: kind, ScopeID: scopeID, FindingKey: key, CreatedBy: by}).
		On("CONFLICT (tenant_id, scope_kind, scope_id, finding_key) DO NOTHING").Exec(ctx)
	return err
}

func (r *assistantDismissalRepository) Keys(ctx context.Context, kind models.AssistantScopeKind, scopeID string) (map[string]bool, error) {
	var keys []string
	err := r.db.NewSelect().Model((*AssistantDismissal)(nil)).Column("adm.finding_key").
		Where("adm.scope_kind = ?", kind).Where("adm.scope_id = ?", scopeID).Scan(ctx, &keys)
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(keys))
	for _, k := range keys {
		out[k] = true
	}
	return out, nil
}

func (r *assistantDismissalRepository) List(ctx context.Context, kind models.AssistantScopeKind, scopeID string) ([]AssistantDismissal, error) {
	out := []AssistantDismissal{}
	err := r.db.NewSelect().Model(&out).
		Where("adm.scope_kind = ?", kind).Where("adm.scope_id = ?", scopeID).
		OrderExpr("adm.created_at DESC, adm.finding_key").Scan(ctx)
	return out, err
}

func (r *assistantDismissalRepository) Restore(ctx context.Context, kind models.AssistantScopeKind, scopeID, key string) (bool, error) {
	res, err := r.db.NewDelete().Model((*AssistantDismissal)(nil)).
		Where("scope_kind = ?", kind).Where("scope_id = ?", scopeID).Where("finding_key = ?", key).Exec(ctx)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}
