package repository

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
)

// InvitationRepository is the persistence surface for workspace invitations.
// The token-minting insert and the accept flow run inside the calling
// handler's transaction (mirroring password reset), so they are not here; this
// interface covers the plain reads + the revoke used by the management endpoints
// and the public preview. Invitations are not TenantScoped — the accept/preview
// paths look them up by token before any tenant is in context — so tenant-scoped
// methods take the tenant id explicitly.
type InvitationRepository interface {
	// Create inserts an invitation on the repository's default DB (tests / any
	// non-transactional caller).
	Create(ctx context.Context, inv *models.Invitation) error
	// CreateReplacingPendingTx clears any pending invite for the same (tenant,
	// email) — live or expired, since either occupies the partial-unique pending
	// slot — then inserts inv, all on the provided bun.IDB so it joins the caller's
	// tx (which also enqueues the email). Re-inviting an address is idempotent:
	// it returns replaced=true when an existing pending invite was
	// cleared, so the handler can answer 200 (re-issue) vs 201 (new). It first takes
	// a transaction-scoped advisory lock keyed on (tenant, email) so concurrent
	// re-issues serialize rather than racing the delete+insert — the partial-unique
	// index already guarantees at most one live invite, but without the lock the
	// loser of the race would 409 instead of re-issuing. Requires the caller's tx
	// for the lock to span the delete+insert; passing nil falls back to the
	// repository's default DB (no cross-statement serialization, used only by
	// non-concurrent callers).
	CreateReplacingPendingTx(ctx context.Context, tx bun.IDB, inv *models.Invitation) (replaced bool, err error)
	// ConsumeByTokenTx atomically spends a usable invitation on the provided
	// bun.IDB (the accept tx, which also creates the user + session): it resolves
	// the invite by token hash and flips it pending->accepted only while still
	// pending and unexpired, returning the consumed invite. Any unusable token
	// (unknown, expired, revoked, already accepted) returns sql.ErrNoRows so the
	// caller can answer one indistinguishable 410. Passing nil uses the default DB.
	ConsumeByTokenTx(ctx context.Context, tx bun.IDB, tokenHash string, now time.Time) (*models.Invitation, error)
	// GetByTokenHash resolves an invitation by the sha256 of its token — the
	// capability the preview/accept endpoints hold. Not tenant-scoped by design.
	GetByTokenHash(ctx context.Context, tokenHash string) (*models.Invitation, error)
	// ListByTenant returns a workspace's invitations, newest first.
	ListByTenant(ctx context.Context, tenantID string) ([]models.Invitation, error)
	// Revoke marks a pending invite revoked. Returns false if no pending invite
	// with that id exists in the tenant (already accepted/revoked, or not found).
	Revoke(ctx context.Context, id, tenantID string) (bool, error)
}

type invitationRepository struct {
	db *bun.DB
}

// NewInvitationRepository returns a Bun-backed InvitationRepository.
func NewInvitationRepository(db *bun.DB) InvitationRepository {
	return &invitationRepository{db: db}
}

func (r *invitationRepository) Create(ctx context.Context, inv *models.Invitation) error {
	_, err := r.db.NewInsert().Model(inv).Exec(ctx)
	return err
}

func (r *invitationRepository) CreateReplacingPendingTx(ctx context.Context, tx bun.IDB, inv *models.Invitation) (bool, error) {
	db := bun.IDB(r.db)
	if tx != nil {
		db = tx
	}
	// Serialize re-issues for the same (tenant, email): a transaction-scoped
	// advisory lock (released on commit/rollback) makes a concurrent second invite
	// wait for the first to commit, then see its row and replace it — so both
	// re-issue idempotently instead of one hitting the partial-unique
	// pending index and 409ing. hashtext maps each key to an int4; a collision only
	// costs unnecessary serialization, never correctness.
	if _, err := db.NewRaw("SELECT pg_advisory_xact_lock(hashtext(?), hashtext(?))", inv.TenantID, inv.Email).Exec(ctx); err != nil {
		return false, err
	}
	res, err := db.NewDelete().Model((*models.Invitation)(nil)).
		Where("tenant_id = ?", inv.TenantID).
		Where("email = ?", inv.Email).
		Where("status = ?", models.InvitationPending).
		Exec(ctx)
	if err != nil {
		return false, err
	}
	replaced, _ := res.RowsAffected()
	if _, err := db.NewInsert().Model(inv).Exec(ctx); err != nil {
		return false, err
	}
	return replaced > 0, nil
}

func (r *invitationRepository) ConsumeByTokenTx(ctx context.Context, tx bun.IDB, tokenHash string, now time.Time) (*models.Invitation, error) {
	db := bun.IDB(r.db)
	if tx != nil {
		db = tx
	}
	inv := new(models.Invitation)
	if err := db.NewSelect().Model(inv).Where("token_hash = ?", tokenHash).Scan(ctx); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, sql.ErrNoRows
		}
		return nil, err
	}
	// Atomic single-use consume: flip pending->accepted only while still pending
	// AND unexpired. A concurrent double-submit races here; exactly one UPDATE
	// affects a row, so the invite can't create two users.
	res, err := db.NewUpdate().Model((*models.Invitation)(nil)).
		Set("status = ?", models.InvitationAccepted).
		Set("accepted_at = ?", now).
		Where("id = ?", inv.ID).
		Where("status = ?", models.InvitationPending).
		Where("expires_at > ?", now).
		Exec(ctx)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, sql.ErrNoRows // revoked, expired, or already accepted
	}
	inv.Status = models.InvitationAccepted
	inv.AcceptedAt = &now
	return inv, nil
}

func (r *invitationRepository) GetByTokenHash(ctx context.Context, tokenHash string) (*models.Invitation, error) {
	inv := new(models.Invitation)
	err := r.db.NewSelect().Model(inv).Where("token_hash = ?", tokenHash).Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, sql.ErrNoRows
		}
		return nil, err
	}
	return inv, nil
}

func (r *invitationRepository) ListByTenant(ctx context.Context, tenantID string) ([]models.Invitation, error) {
	var invs []models.Invitation
	err := r.db.NewSelect().Model(&invs).
		Where("tenant_id = ?", tenantID).
		OrderExpr("created_at DESC").
		Scan(ctx)
	if err != nil {
		return nil, err
	}
	return invs, nil
}

func (r *invitationRepository) Revoke(ctx context.Context, id, tenantID string) (bool, error) {
	res, err := r.db.NewUpdate().Model((*models.Invitation)(nil)).
		Set("status = ?", models.InvitationRevoked).
		Where("id = ?", id).
		Where("tenant_id = ?", tenantID).
		Where("status = ?", models.InvitationPending).
		Exec(ctx)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}
