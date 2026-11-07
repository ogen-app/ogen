package repository

import (
	"context"
	"time"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
)

// AssistantProposalRepository stores work the assistant offered but has not
// done yet.
type AssistantProposalRepository interface {
	// Create stores a new offer, pending unless p says otherwise, with empty
	// args and options for none.
	Create(ctx context.Context, p *models.AssistantProposal) error
	// GetByID returns the caller's tenant's proposal; sql.ErrNoRows when
	// there is none.
	GetByID(ctx context.Context, id string) (*models.AssistantProposal, error)
	// ListOpen returns a scope's pending, unexpired proposals, newest first.
	ListOpen(ctx context.Context, kind models.AssistantScopeKind, scopeID string, now time.Time) ([]models.AssistantProposal, error)
	// Resolve moves an open proposal to status (confirmed or dismissed),
	// recording the chosen option and who resolved it. It reports false when
	// the proposal was no longer open — already resolved or expired — so a
	// button click and a typed "yes" racing each other run the work once.
	Resolve(ctx context.Context, id string, status models.AssistantProposalStatus, chosen, by string, now time.Time) (bool, error)
}

type assistantProposalRepository struct {
	db *bun.DB
}

// NewAssistantProposalRepository returns the bun-backed proposal store.
func NewAssistantProposalRepository(db *bun.DB) AssistantProposalRepository {
	return &assistantProposalRepository{db: db}
}

func (r *assistantProposalRepository) Create(ctx context.Context, p *models.AssistantProposal) error {
	if p.Args == nil {
		p.Args = models.JSONMap{}
	}
	if p.Options == nil {
		p.Options = []models.ProposalOption{}
	}
	if p.Status == "" {
		p.Status = models.ProposalPending
	}
	_, err := r.db.NewInsert().Model(p).Exec(ctx)
	return err
}

func (r *assistantProposalRepository) GetByID(ctx context.Context, id string) (*models.AssistantProposal, error) {
	p := new(models.AssistantProposal)
	if err := r.db.NewSelect().Model(p).Where("ap.id = ?", id).Scan(ctx); err != nil {
		return nil, err
	}
	return p, nil
}

func (r *assistantProposalRepository) ListOpen(ctx context.Context, kind models.AssistantScopeKind, scopeID string, now time.Time) ([]models.AssistantProposal, error) {
	var out []models.AssistantProposal
	err := r.db.NewSelect().Model(&out).
		Where("ap.scope_kind = ? AND ap.scope_id = ?", kind, scopeID).
		Where("ap.status = ? AND ap.expires_at > ?", models.ProposalPending, now).
		OrderExpr("ap.created_at DESC, ap.id DESC").
		Scan(ctx)
	return out, err
}

func (r *assistantProposalRepository) Resolve(ctx context.Context, id string, status models.AssistantProposalStatus, chosen, by string, now time.Time) (bool, error) {
	res, err := r.db.NewUpdate().Model((*models.AssistantProposal)(nil)).
		Set("status = ?", status).
		Set("chosen = ?", chosen).
		Set("resolved_by = ?", by).
		Set("resolved_at = ?", now).
		Where("ap.id = ?", id).
		Where("ap.status = ? AND ap.expires_at > ?", models.ProposalPending, now).
		Exec(ctx)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}
