package repository

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
)

// Facts ledger and guardrails stance (CON-316). Facts are rows of their own,
// written one at a time; guardrails.facts on the wire is a projection of them.

// MaxBrandFacts caps the ledger per workspace. Every current fact is read on
// each generation turn.
const MaxBrandFacts = 200

var (
	// ErrFactDuplicate: another fact in the workspace has the same statement.
	ErrFactDuplicate = errors.New("a fact with this statement already exists")
	// ErrFactLimit: the workspace already holds MaxBrandFacts facts.
	ErrFactLimit = errors.New("fact limit reached")
	// ErrGuardrailsExist: the stance cannot be set while guardrails exist.
	ErrGuardrailsExist = errors.New("guardrails exist")
)

// FactsReconciled counts what a guardrails save with a facts list changed.
type FactsReconciled struct {
	Added   int `json:"added"`
	Removed int `json:"removed"`
}

const factsUniqueIndex = "idx_brand_facts_tenant_statement"

func isFactDuplicate(err error) bool {
	pgErr, ok := errors.AsType[*pgconn.PgError](err)
	return ok && pgErr.Code == "23505" && pgErr.ConstraintName == factsUniqueIndex
}

// lockFacts serialises ledger writes within a tenant for the rest of the tx, so
// the MaxBrandFacts count and the duplicate check cannot race.
func lockFacts(ctx context.Context, tx bun.Tx) error {
	return tenantLock(ctx, tx, "brand_facts")
}

// lockGuardrails serialises guardrails saves against setting the stance.
func lockGuardrails(ctx context.Context, tx bun.Tx) error {
	return tenantLock(ctx, tx, "brand_guardrails")
}

func tenantLock(ctx context.Context, tx bun.Tx, name string) error {
	tid, ok := tenantctx.From(ctx)
	if !ok {
		return tenantctx.ErrNoTenant
	}
	_, err := tx.NewRaw("SELECT pg_advisory_xact_lock(hashtext(?), hashtext(?))", name, tid).Exec(ctx)
	return err
}

// authorName is the name snapshot kept beside an author id, since users are
// hard-deleted: the member's name, else their email, else "".
func authorName(ctx context.Context, db bun.IDB, userID *string) (string, error) {
	if userID == nil || *userID == "" {
		return "", nil
	}
	var row struct {
		Name  string `bun:"name"`
		Email string `bun:"email"`
	}
	err := db.NewRaw("SELECT name, email FROM users WHERE id = ?", *userID).Scan(ctx, &row)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if name := strings.TrimSpace(row.Name); name != "" {
		return name, nil
	}
	return strings.TrimSpace(row.Email), nil
}

func listFacts(ctx context.Context, db bun.IDB) ([]models.BrandFact, error) {
	facts := []models.BrandFact{}
	if err := db.NewSelect().Model(&facts).OrderExpr("bf.created_at ASC, bf.id ASC").Scan(ctx); err != nil {
		return nil, err
	}
	if facts == nil {
		facts = []models.BrandFact{}
	}
	return facts, nil
}

func factStatements(facts []models.BrandFact) models.StringSlice {
	out := make(models.StringSlice, 0, len(facts))
	for _, f := range facts {
		out = append(out, f.Statement)
	}
	return out
}

// ListFacts returns the whole ledger, expired facts included.
func (r *brandRepository) ListFacts(ctx context.Context) ([]models.BrandFact, error) {
	return listFacts(ctx, r.db)
}

// GetFact returns the fact by id within the caller's tenant, or nil when absent.
func (r *brandRepository) GetFact(ctx context.Context, id string) (*models.BrandFact, error) {
	f := new(models.BrandFact)
	err := r.db.NewSelect().Model(f).Where("bf.id = ?", id).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return f, nil
}

// CreateFact inserts f, stamping CreatedByName from f.CreatedBy. It returns
// ErrFactDuplicate or ErrFactLimit.
func (r *brandRepository) CreateFact(ctx context.Context, f *models.BrandFact) error {
	return r.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		if err := lockFacts(ctx, tx); err != nil {
			return err
		}
		n, err := tx.NewSelect().Model((*models.BrandFact)(nil)).Count(ctx)
		if err != nil {
			return err
		}
		if n >= MaxBrandFacts {
			return ErrFactLimit
		}
		name, err := authorName(ctx, tx, f.CreatedBy)
		if err != nil {
			return err
		}
		f.CreatedByName = name
		if _, err := tx.NewInsert().Model(f).Exec(ctx); err != nil {
			if isFactDuplicate(err) {
				return ErrFactDuplicate
			}
			return err
		}
		return nil
	})
}

// UpdateFact replaces the editable fields of fact f.ID and returns the row as
// it was before. Author and created_at are kept from the stored row, and f is
// filled with them. When nothing editable changed the row is left alone and f
// carries the stored updated_at. It returns sql.ErrNoRows or ErrFactDuplicate.
func (r *brandRepository) UpdateFact(ctx context.Context, f *models.BrandFact) (before *models.BrandFact, err error) {
	err = r.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		existing := new(models.BrandFact)
		if err := tx.NewSelect().Model(existing).Where("bf.id = ?", f.ID).For("UPDATE").Scan(ctx); err != nil {
			return err // sql.ErrNoRows → 404
		}
		before = existing
		f.TenantID = existing.TenantID
		f.CreatedBy = existing.CreatedBy
		f.CreatedByName = existing.CreatedByName
		f.CreatedAt = existing.CreatedAt
		if factsEqual(existing, f) {
			f.UpdatedAt = existing.UpdatedAt
			return nil
		}
		_, err := tx.NewUpdate().Model(f).
			Column("statement", "subject", "kind", "source", "added_on", "checked_on", "expires_on", "updated_at").
			WherePK().Exec(ctx)
		if isFactDuplicate(err) {
			return ErrFactDuplicate
		}
		return err
	})
	return before, err
}

// DeleteFact hard-deletes the fact and returns it, or nil when absent.
func (r *brandRepository) DeleteFact(ctx context.Context, id string) (*models.BrandFact, error) {
	f := new(models.BrandFact)
	err := r.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		if err := tx.NewSelect().Model(f).Where("bf.id = ?", id).For("UPDATE").Scan(ctx); err != nil {
			return err
		}
		_, err := tx.NewDelete().Model((*models.BrandFact)(nil)).Where("id = ?", id).Exec(ctx)
		return err
	})
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return f, nil
}

// factsEqual compares the editable fields of two facts.
func factsEqual(a, b *models.BrandFact) bool {
	return a.Statement == b.Statement && a.Subject == b.Subject && a.Kind == b.Kind &&
		a.Source == b.Source && dateEqual(a.AddedAt, b.AddedAt) &&
		dateEqual(a.CheckedAt, b.CheckedAt) && dateEqual(a.ExpiresAt, b.ExpiresAt)
}

func dateEqual(a, b *models.CalendarDate) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(b.Time)
}

// ── Guardrails save (compat path) ───────────────────────────────────────────

// SaveGuardrails upserts the guardrails row and, in the same transaction,
// clears the stance (a written rule always overrides it) and, when facts is
// non-nil, reconciles the ledger against it by statement: matching facts keep
// their metadata, new statements are inserted with the backfill defaults and
// author, and facts whose statement is missing are deleted. facts must already
// be trimmed, non-blank and distinct. A nil facts leaves the ledger alone.
//
// The legacy brand_guardrails.facts column is no longer written; reads
// project the ledger over it.
func (r *brandRepository) SaveGuardrails(ctx context.Context, g *models.BrandGuardrails, facts []string, author *string) (FactsReconciled, error) {
	var rec FactsReconciled
	err := r.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		if err := lockGuardrails(ctx, tx); err != nil {
			return err
		}
		row := *g
		row.Facts = models.StringSlice{}
		if _, err := tx.NewInsert().Model(&row).
			On("CONFLICT (tenant_id) DO UPDATE").
			Set("may_claim = EXCLUDED.may_claim").
			Set("never_claim = EXCLUDED.never_claim").
			Set("banned_words = EXCLUDED.banned_words").
			Set("disclaimer = EXCLUDED.disclaimer").
			Set("updated_at = EXCLUDED.updated_at").
			Exec(ctx); err != nil {
			return err
		}
		if err := deleteStance(ctx, tx); err != nil {
			return err
		}
		if facts == nil {
			return nil
		}
		var err error
		rec, err = reconcileFacts(ctx, tx, facts, author, g.UpdatedAt)
		return err
	})
	return rec, err
}

func reconcileFacts(ctx context.Context, tx bun.Tx, want []string, author *string, now time.Time) (FactsReconciled, error) {
	var rec FactsReconciled
	if err := lockFacts(ctx, tx); err != nil {
		return rec, err
	}
	existing, err := listFacts(ctx, tx)
	if err != nil {
		return rec, err
	}
	wanted := make(map[string]bool, len(want))
	for _, s := range want {
		wanted[s] = true
	}
	have := make(map[string]bool, len(existing))
	var drop []string
	for _, f := range existing {
		have[f.Statement] = true
		if !wanted[f.Statement] {
			drop = append(drop, f.ID)
		}
	}
	if len(drop) > 0 {
		if _, err := tx.NewDelete().Model((*models.BrandFact)(nil)).Where("id IN (?)", bun.In(drop)).Exec(ctx); err != nil {
			return rec, err
		}
		rec.Removed = len(drop)
	}
	if len(existing)-len(drop)+countMissing(want, have) > MaxBrandFacts {
		return rec, ErrFactLimit
	}
	name, err := authorName(ctx, tx, author)
	if err != nil {
		return rec, err
	}
	for _, s := range want {
		if have[s] {
			continue
		}
		id, err := models.NewID()
		if err != nil {
			return rec, err
		}
		// Space the new rows a microsecond apart so the ledger keeps the
		// order the list arrived in.
		at := now.Add(time.Duration(rec.Added) * time.Microsecond)
		f := &models.BrandFact{
			ID:            id,
			Statement:     s,
			Subject:       models.FactSubjectUs,
			Kind:          models.FactKindDocumented,
			CreatedBy:     author,
			CreatedByName: name,
			CreatedAt:     at,
			UpdatedAt:     at,
		}
		if _, err := tx.NewInsert().Model(f).Exec(ctx); err != nil {
			return rec, err
		}
		rec.Added++
	}
	return rec, nil
}

func countMissing(want []string, have map[string]bool) int {
	n := 0
	for _, s := range want {
		if !have[s] {
			n++
		}
	}
	return n
}

// ── Stance ──────────────────────────────────────────────────────────────────

// GetGuardrailsStance returns the stored stance, or nil when undecided.
func (r *brandRepository) GetGuardrailsStance(ctx context.Context) (*models.BrandGuardrailsStanceRecord, error) {
	return getStance(ctx, r.db)
}

func getStance(ctx context.Context, db bun.IDB) (*models.BrandGuardrailsStanceRecord, error) {
	s := new(models.BrandGuardrailsStanceRecord)
	err := db.NewSelect().Model(s).Limit(1).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return s, nil
}

// SetGuardrailsStance records that the workspace needs no guardrails, stamping
// the author's name snapshot. An existing stance is kept as it is (the first
// decision stands). It returns ErrGuardrailsExist when a guardrails row exists.
func (r *brandRepository) SetGuardrailsStance(ctx context.Context, s *models.BrandGuardrailsStanceRecord) (*models.BrandGuardrailsStanceRecord, error) {
	var out *models.BrandGuardrailsStanceRecord
	err := r.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		// Serialised with SaveGuardrails, so a concurrent first save cannot
		// land between this check and the insert: the two never both stand.
		if err := lockGuardrails(ctx, tx); err != nil {
			return err
		}
		exists, err := tx.NewSelect().Model((*models.BrandGuardrails)(nil)).Count(ctx)
		if err != nil {
			return err
		}
		if exists > 0 {
			return ErrGuardrailsExist
		}
		name, err := authorName(ctx, tx, s.DecidedBy)
		if err != nil {
			return err
		}
		s.DecidedByName = name
		if _, err := tx.NewInsert().Model(s).On("CONFLICT (tenant_id) DO NOTHING").Exec(ctx); err != nil {
			return err
		}
		out, err = getStance(ctx, tx)
		return err
	})
	return out, err
}

// DeleteGuardrailsStance returns the workspace to undecided. Idempotent.
func (r *brandRepository) DeleteGuardrailsStance(ctx context.Context) error {
	return deleteStance(ctx, r.db)
}

func deleteStance(ctx context.Context, db bun.IDB) error {
	tid, ok := tenantctx.From(ctx)
	if !ok {
		return tenantctx.ErrNoTenant
	}
	_, err := db.NewDelete().Model((*models.BrandGuardrailsStanceRecord)(nil)).Where("tenant_id = ?", tid).Exec(ctx)
	return err
}
