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
	// ListByIDs returns the caller's tenant's proposals among ids, in no
	// order; ids it doesn't know are left out.
	ListByIDs(ctx context.Context, ids []string) ([]models.AssistantProposal, error)
	// ListReplacing returns the caller's tenant's proposals made afresh for
	// one of ids, in no order.
	ListReplacing(ctx context.Context, ids []string) ([]models.AssistantProposal, error)
	// ListOpen returns a scope's pending, unexpired proposals, newest first,
	// leaving out one a pending offer replaces.
	ListOpen(ctx context.Context, kind models.AssistantScopeKind, scopeID string, now time.Time) ([]models.AssistantProposal, error)
	// Replacement returns the id of the pending, unexpired offer made afresh
	// for id; "" when there is none.
	Replacement(ctx context.Context, id string, now time.Time) (string, error)
	// ListAwaiting returns a scope's offers confirmed with one of the chosen
	// options at or after since, oldest first, leaving out one any offer
	// already replaces: the offers still waiting on what that option set off.
	ListAwaiting(ctx context.Context, kind models.AssistantScopeKind, scopeID string, chosen []string, since time.Time) ([]models.AssistantProposal, error)
	// Resolve moves an open proposal to status (confirmed or dismissed),
	// recording the chosen option and who resolved it. It reports false when
	// the proposal was no longer open — already resolved or expired — so a
	// button click and a typed "yes" racing each other run the work once.
	Resolve(ctx context.Context, id string, status models.AssistantProposalStatus, chosen, by string, now time.Time) (bool, error)
	// Reopen puts a confirmed proposal back to pending, unanswered, when its
	// confirmation ran nothing because what it was made on changed. It
	// reports false when the proposal wasn't confirmed.
	Reopen(ctx context.Context, id string) (bool, error)
	// Expire marks a pending proposal past its expiry as expired. It reports
	// false when the proposal wasn't pending or hasn't expired.
	Expire(ctx context.Context, id string, now time.Time) (bool, error)
	// Delete removes the caller's tenant's proposals among ids: offers made
	// but never shown, e.g. their thread turn couldn't be stored.
	Delete(ctx context.Context, ids []string) error
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
		Where("NOT EXISTS (SELECT 1 FROM assistant_proposals r WHERE r.replaces = ap.id AND r.tenant_id = ap.tenant_id AND r.status = ? AND r.expires_at > ?)",
			models.ProposalPending, now).
		OrderExpr("ap.created_at DESC, ap.id DESC").
		Scan(ctx)
	return out, err
}

func (r *assistantProposalRepository) ListByIDs(ctx context.Context, ids []string) ([]models.AssistantProposal, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	var out []models.AssistantProposal
	err := r.db.NewSelect().Model(&out).Where("ap.id IN (?)", bun.List(ids)).Scan(ctx)
	return out, err
}

func (r *assistantProposalRepository) ListReplacing(ctx context.Context, ids []string) ([]models.AssistantProposal, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	var out []models.AssistantProposal
	err := r.db.NewSelect().Model(&out).Where("ap.replaces IN (?)", bun.List(ids)).Scan(ctx)
	return out, err
}

func (r *assistantProposalRepository) Replacement(ctx context.Context, id string, now time.Time) (string, error) {
	var ids []string
	err := r.db.NewSelect().Model((*models.AssistantProposal)(nil)).Column("ap.id").
		Where("ap.replaces = ?", id).
		Where("ap.status = ? AND ap.expires_at > ?", models.ProposalPending, now).
		OrderExpr("ap.created_at DESC").Limit(1).
		Scan(ctx, &ids)
	if err != nil || len(ids) == 0 {
		return "", err
	}
	return ids[0], nil
}

func (r *assistantProposalRepository) ListAwaiting(ctx context.Context, kind models.AssistantScopeKind, scopeID string, chosen []string, since time.Time) ([]models.AssistantProposal, error) {
	if len(chosen) == 0 {
		return nil, nil
	}
	var out []models.AssistantProposal
	err := r.db.NewSelect().Model(&out).
		Where("ap.scope_kind = ? AND ap.scope_id = ?", kind, scopeID).
		Where("ap.status = ? AND ap.chosen IN (?) AND ap.resolved_at >= ?", models.ProposalConfirmed, bun.List(chosen), since).
		Where("NOT EXISTS (SELECT 1 FROM assistant_proposals r WHERE r.replaces = ap.id AND r.tenant_id = ap.tenant_id)").
		OrderExpr("ap.resolved_at, ap.id").
		Scan(ctx)
	return out, err
}

func (r *assistantProposalRepository) Delete(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := r.db.NewDelete().Model((*models.AssistantProposal)(nil)).Where("ap.id IN (?)", bun.List(ids)).Exec(ctx)
	return err
}

func (r *assistantProposalRepository) Reopen(ctx context.Context, id string) (bool, error) {
	res, err := r.db.NewUpdate().Model((*models.AssistantProposal)(nil)).
		Set("status = ?", models.ProposalPending).
		Set("chosen = ''").
		Set("resolved_by = ''").
		Set("resolved_at = NULL").
		Where("ap.id = ?", id).
		Where("ap.status = ?", models.ProposalConfirmed).
		Exec(ctx)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

func (r *assistantProposalRepository) Expire(ctx context.Context, id string, now time.Time) (bool, error) {
	res, err := r.db.NewUpdate().Model((*models.AssistantProposal)(nil)).
		Set("status = ?", models.ProposalExpired).
		Set("resolved_at = ?", now).
		Where("ap.id = ?", id).
		Where("ap.status = ? AND ap.expires_at <= ?", models.ProposalPending, now).
		Exec(ctx)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
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
