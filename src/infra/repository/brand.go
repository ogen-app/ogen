package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
)

// BrandRepository is the persistence for the Brand module (CON-228): one
// aggregate read plus per-section whole-resource writes. Everything is
// tenant-scoped automatically by the TenantScoped bun hooks on the models —
// these methods never add a tenant predicate by hand, and inserts never carry a
// client-supplied tenant.
type BrandRepository interface {
	// GetAll returns every slot for the caller's tenant. Lists are non-nil
	// (empty, not null); singletons are nil when unset.
	GetAll(ctx context.Context) (*models.BrandData, error)

	GetVoice(ctx context.Context, id string) (*models.BrandVoice, error) // nil when not in tenant
	CreateVoice(ctx context.Context, v *models.BrandVoice) error
	UpdateVoice(ctx context.Context, v *models.BrandVoice) error // sql.ErrNoRows when unknown
	DeleteVoice(ctx context.Context, id string) (bool, error)

	GetAudience(ctx context.Context, id string) (*models.BrandAudience, error) // nil when not in tenant
	CreateAudience(ctx context.Context, a *models.BrandAudience) error
	UpdateAudience(ctx context.Context, a *models.BrandAudience) error // sql.ErrNoRows when unknown
	DeleteAudience(ctx context.Context, id string) (bool, error)

	// GetGuardrails returns nil when unset. Facts is the ledger projection.
	GetGuardrails(ctx context.Context) (*models.BrandGuardrails, error)
	// SaveGuardrails upserts the row and clears the stance; a non-nil facts
	// reconciles the ledger by statement (CON-316 FR6).
	SaveGuardrails(ctx context.Context, g *models.BrandGuardrails, facts []string, author *string) (FactsReconciled, error)
	DeleteGuardrails(ctx context.Context) (bool, error) // leaves the ledger alone

	// Facts ledger (CON-316).
	ListFacts(ctx context.Context) ([]models.BrandFact, error)
	GetFact(ctx context.Context, id string) (*models.BrandFact, error) // nil when not in tenant
	CreateFact(ctx context.Context, f *models.BrandFact) error         // ErrFactDuplicate, ErrFactLimit
	UpdateFact(ctx context.Context, f *models.BrandFact) (*models.BrandFact, error)
	DeleteFact(ctx context.Context, id string) (*models.BrandFact, error) // nil when not in tenant

	// Guardrails stance (CON-316 FR7). nil = undecided.
	GetGuardrailsStance(ctx context.Context) (*models.BrandGuardrailsStanceRecord, error)
	SetGuardrailsStance(ctx context.Context, s *models.BrandGuardrailsStanceRecord) (*models.BrandGuardrailsStanceRecord, error) // ErrGuardrailsExist
	DeleteGuardrailsStance(ctx context.Context) error

	GetLook(ctx context.Context) (*models.BrandLook, error) // nil when unset
	UpsertLook(ctx context.Context, l *models.BrandLook) error
	DeleteLook(ctx context.Context) (bool, error)

	CreateTemplate(ctx context.Context, t *models.BrandTemplate) error
	UpdateTemplate(ctx context.Context, t *models.BrandTemplate) error // sql.ErrNoRows when unknown
	DeleteTemplate(ctx context.Context, id string) (bool, error)
}

type brandRepository struct {
	db *bun.DB
}

// NewBrandRepository returns a Bun-backed BrandRepository.
func NewBrandRepository(db *bun.DB) BrandRepository {
	return &brandRepository{db: db}
}

// ── Aggregate ───────────────────────────────────────────────────────────────

func (r *brandRepository) GetAll(ctx context.Context) (*models.BrandData, error) {
	voices := []models.BrandVoice{}
	if err := r.db.NewSelect().Model(&voices).OrderExpr("created_at ASC").Scan(ctx); err != nil {
		return nil, err
	}
	audiences := []models.BrandAudience{}
	if err := r.db.NewSelect().Model(&audiences).OrderExpr("created_at ASC").Scan(ctx); err != nil {
		return nil, err
	}
	templates := []models.BrandTemplate{}
	if err := r.db.NewSelect().Model(&templates).OrderExpr("created_at ASC").Scan(ctx); err != nil {
		return nil, err
	}
	facts, err := listFacts(ctx, r.db)
	if err != nil {
		return nil, err
	}
	guardrails, err := r.getGuardrailsRow(ctx)
	if err != nil {
		return nil, err
	}
	if guardrails != nil {
		guardrails.Facts = factStatements(facts)
	}
	stance, err := getStance(ctx, r.db)
	if err != nil {
		return nil, err
	}
	look, err := r.GetLook(ctx)
	if err != nil {
		return nil, err
	}
	// bun leaves the slices nil when zero rows come back on some paths; the
	// aggregate contract is `[]`, never null.
	if voices == nil {
		voices = []models.BrandVoice{}
	}
	if audiences == nil {
		audiences = []models.BrandAudience{}
	}
	if templates == nil {
		templates = []models.BrandTemplate{}
	}
	// CON-245: fill the derived draft/published usage counts from the post refs.
	if err := r.fillUsage(ctx, voices, audiences); err != nil {
		return nil, err
	}
	return &models.BrandData{
		Voices:     voices,
		Audiences:  audiences,
		Guardrails: guardrails,
		Look:       look,
		Templates:  templates,
		Facts:      facts,

		GuardrailsStance: models.StanceOf(stance),
	}, nil
}

// fillUsage sets each voice's/audience's derived usage {drafts, published}
// (CON-245, unblocks CON-228 FR7): a voice counts the posts stamped with it; an
// audience counts posts whose effective audience — the post's own, else its
// campaign's — is that audience. "published" is the terminal published status;
// everything else counts as a still-ours draft. A missing tenant leaves the
// zero counts in place rather than counting across tenants.
func (r *brandRepository) fillUsage(ctx context.Context, voices []models.BrandVoice, audiences []models.BrandAudience) error {
	tid, ok := tenantctx.From(ctx)
	if !ok {
		return nil
	}
	type usageRow struct {
		Key       string `bun:"key"`
		Published int    `bun:"published"`
		Drafts    int    `bun:"drafts"`
	}
	pub := string(models.PostStatusPublished)

	var vrows []usageRow
	if err := r.db.NewSelect().
		ColumnExpr("brand_voice_id AS key").
		ColumnExpr("COUNT(*) FILTER (WHERE status = ?) AS published", pub).
		ColumnExpr("COUNT(*) FILTER (WHERE status <> ?) AS drafts", pub).
		TableExpr("posts").
		Where("tenant_id = ?", tid).
		Where("brand_voice_id IS NOT NULL").
		GroupExpr("brand_voice_id").
		Scan(ctx, &vrows); err != nil {
		return err
	}
	vmap := make(map[string]models.BrandUsage, len(vrows))
	for _, row := range vrows {
		vmap[row.Key] = models.BrandUsage{Drafts: row.Drafts, Published: row.Published}
	}
	for i := range voices {
		voices[i].Usage = vmap[voices[i].ID]
	}

	var arows []usageRow
	if err := r.db.NewSelect().
		ColumnExpr("COALESCE(p.brand_audience_id, c.brand_audience_id) AS key").
		ColumnExpr("COUNT(*) FILTER (WHERE p.status = ?) AS published", pub).
		ColumnExpr("COUNT(*) FILTER (WHERE p.status <> ?) AS drafts", pub).
		TableExpr("posts AS p").
		Join("JOIN campaigns AS c ON c.id = p.campaign_id").
		Where("p.tenant_id = ?", tid).
		Where("COALESCE(p.brand_audience_id, c.brand_audience_id) IS NOT NULL").
		GroupExpr("COALESCE(p.brand_audience_id, c.brand_audience_id)").
		Scan(ctx, &arows); err != nil {
		return err
	}
	amap := make(map[string]models.BrandUsage, len(arows))
	for _, row := range arows {
		amap[row.Key] = models.BrandUsage{Drafts: row.Drafts, Published: row.Published}
	}
	for i := range audiences {
		audiences[i].Usage = amap[audiences[i].ID]
	}
	return nil
}

// ── Voices ──────────────────────────────────────────────────────────────────

// GetVoice returns the voice by id within the caller's tenant, or nil when
// absent (unknown id, or a foreign-tenant id the scoping hook filters out).
func (r *brandRepository) GetVoice(ctx context.Context, id string) (*models.BrandVoice, error) {
	v := new(models.BrandVoice)
	err := r.db.NewSelect().Model(v).Where("bv.id = ?", id).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return v, nil
}

func (r *brandRepository) CreateVoice(ctx context.Context, v *models.BrandVoice) error {
	return r.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		if v.IsDefault {
			if err := demoteDefault(ctx, tx, (*models.BrandVoice)(nil), ""); err != nil {
				return err
			}
		}
		_, err := tx.NewInsert().Model(v).Exec(ctx)
		return err
	})
}

func (r *brandRepository) UpdateVoice(ctx context.Context, v *models.BrandVoice) error {
	return r.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		existing := new(models.BrandVoice)
		if err := tx.NewSelect().Model(existing).Where("bv.id = ?", v.ID).For("UPDATE").Scan(ctx); err != nil {
			return err // sql.ErrNoRows → handler maps to 404
		}
		// Server-owned / write-once fields survive a replace.
		v.TenantID = existing.TenantID
		v.CreatedAt = existing.CreatedAt
		v.Origin = existing.Origin // FR6: origin is provenance, write-once.
		if v.IsDefault {
			if err := demoteDefault(ctx, tx, (*models.BrandVoice)(nil), v.ID); err != nil {
				return err
			}
		}
		_, err := tx.NewUpdate().Model(v).WherePK().Exec(ctx)
		return err
	})
}

func (r *brandRepository) DeleteVoice(ctx context.Context, id string) (bool, error) {
	var deleted bool
	err := r.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		target := new(models.BrandVoice)
		err := tx.NewSelect().Model(target).Where("bv.id = ?", id).For("UPDATE").Scan(ctx)
		if errors.Is(err, sql.ErrNoRows) {
			return nil // not found → deleted stays false
		}
		if err != nil {
			return err
		}
		if _, err := tx.NewDelete().Model((*models.BrandVoice)(nil)).Where("id = ?", id).Exec(ctx); err != nil {
			return err
		}
		deleted = true
		if target.IsDefault {
			return promoteEarliestVoice(ctx, tx)
		}
		return nil
	})
	return deleted, err
}

// promoteEarliestVoice hands the default flag to the earliest remaining voice.
// FR4: a workspace with voices and no default is a state the UI cannot repair.
//
// The survivor is selected FOR UPDATE so a concurrent delete of that same row
// cannot slip between the select and the update and leave the library with no
// default; the row-count check turns any such surprise into a rolled-back
// (fail-closed) delete rather than a silently broken invariant.
func promoteEarliestVoice(ctx context.Context, tx bun.Tx) error {
	survivor := new(models.BrandVoice)
	err := tx.NewSelect().Model(survivor).OrderExpr("created_at ASC").Limit(1).For("UPDATE").Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil // last voice deleted; nothing to promote
	}
	if err != nil {
		return err
	}
	survivor.IsDefault = true
	survivor.UpdatedAt = time.Now().UTC()
	res, err := tx.NewUpdate().Model(survivor).Column("is_default", "updated_at").WherePK().Exec(ctx)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("promote voice: expected 1 row updated, got %d", n)
	}
	return nil
}

// ── Audiences ───────────────────────────────────────────────────────────────

// GetAudience returns the audience by id within the caller's tenant, or nil
// when absent. See GetVoice.
func (r *brandRepository) GetAudience(ctx context.Context, id string) (*models.BrandAudience, error) {
	a := new(models.BrandAudience)
	err := r.db.NewSelect().Model(a).Where("ba.id = ?", id).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return a, nil
}

func (r *brandRepository) CreateAudience(ctx context.Context, a *models.BrandAudience) error {
	_, err := r.db.NewInsert().Model(a).Exec(ctx)
	return err
}

func (r *brandRepository) UpdateAudience(ctx context.Context, a *models.BrandAudience) error {
	return r.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		existing := new(models.BrandAudience)
		if err := tx.NewSelect().Model(existing).Where("ba.id = ?", a.ID).Scan(ctx); err != nil {
			return err // sql.ErrNoRows → 404
		}
		a.TenantID = existing.TenantID
		a.CreatedAt = existing.CreatedAt
		a.Origin = existing.Origin // FR6
		_, err := tx.NewUpdate().Model(a).WherePK().Exec(ctx)
		return err
	})
}

func (r *brandRepository) DeleteAudience(ctx context.Context, id string) (bool, error) {
	res, err := r.db.NewDelete().Model((*models.BrandAudience)(nil)).Where("id = ?", id).Exec(ctx)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ── Guardrails (singleton) ──────────────────────────────────────────────────

func (r *brandRepository) GetGuardrails(ctx context.Context) (*models.BrandGuardrails, error) {
	g, err := r.getGuardrailsRow(ctx)
	if err != nil || g == nil {
		return g, err
	}
	facts, err := listFacts(ctx, r.db)
	if err != nil {
		return nil, err
	}
	g.Facts = factStatements(facts)
	return g, nil
}

// getGuardrailsRow reads the row as stored. Its facts column is legacy
// (CON-316); callers project the ledger over it.
func (r *brandRepository) getGuardrailsRow(ctx context.Context) (*models.BrandGuardrails, error) {
	g := new(models.BrandGuardrails)
	err := r.db.NewSelect().Model(g).Limit(1).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return g, nil
}

func (r *brandRepository) DeleteGuardrails(ctx context.Context) (bool, error) {
	return deleteSingleton(ctx, r.db, (*models.BrandGuardrails)(nil))
}

// ── Look (singleton) ────────────────────────────────────────────────────────

func (r *brandRepository) GetLook(ctx context.Context) (*models.BrandLook, error) {
	l := new(models.BrandLook)
	err := r.db.NewSelect().Model(l).Limit(1).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return l, nil
}

// UpsertLook is UpsertGuardrails for the look singleton — same atomic ON
// CONFLICT rationale.
func (r *brandRepository) UpsertLook(ctx context.Context, l *models.BrandLook) error {
	_, err := r.db.NewInsert().Model(l).
		On("CONFLICT (tenant_id) DO UPDATE").
		Set("logos = EXCLUDED.logos").
		Set("palette = EXCLUDED.palette").
		Set("typefaces = EXCLUDED.typefaces").
		Set("reference_images = EXCLUDED.reference_images").
		Set("updated_at = EXCLUDED.updated_at").
		Exec(ctx)
	return err
}

func (r *brandRepository) DeleteLook(ctx context.Context) (bool, error) {
	return deleteSingleton(ctx, r.db, (*models.BrandLook)(nil))
}

// ── Templates ───────────────────────────────────────────────────────────────

func (r *brandRepository) CreateTemplate(ctx context.Context, t *models.BrandTemplate) error {
	return r.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		if t.IsDefault {
			if err := demoteDefault(ctx, tx, (*models.BrandTemplate)(nil), ""); err != nil {
				return err
			}
		}
		_, err := tx.NewInsert().Model(t).Exec(ctx)
		return err
	})
}

func (r *brandRepository) UpdateTemplate(ctx context.Context, t *models.BrandTemplate) error {
	return r.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		existing := new(models.BrandTemplate)
		if err := tx.NewSelect().Model(existing).Where("bt.id = ?", t.ID).For("UPDATE").Scan(ctx); err != nil {
			return err // sql.ErrNoRows → 404
		}
		t.TenantID = existing.TenantID
		t.CreatedAt = existing.CreatedAt
		t.Origin = existing.Origin // FR6
		if t.IsDefault {
			if err := demoteDefault(ctx, tx, (*models.BrandTemplate)(nil), t.ID); err != nil {
				return err
			}
		}
		_, err := tx.NewUpdate().Model(t).WherePK().Exec(ctx)
		return err
	})
}

func (r *brandRepository) DeleteTemplate(ctx context.Context, id string) (bool, error) {
	var deleted bool
	err := r.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		target := new(models.BrandTemplate)
		err := tx.NewSelect().Model(target).Where("bt.id = ?", id).For("UPDATE").Scan(ctx)
		if errors.Is(err, sql.ErrNoRows) {
			return nil // not found → deleted stays false
		}
		if err != nil {
			return err
		}
		if _, err := tx.NewDelete().Model((*models.BrandTemplate)(nil)).Where("id = ?", id).Exec(ctx); err != nil {
			return err
		}
		deleted = true
		if target.IsDefault {
			return promoteEarliestTemplate(ctx, tx)
		}
		return nil
	})
	return deleted, err
}

// promoteEarliestTemplate mirrors promoteEarliestVoice: templates carry the same
// one-default invariant (FR4), so deleting the default hands the flag on rather
// than leaving the workspace with a default-less template set.
func promoteEarliestTemplate(ctx context.Context, tx bun.Tx) error {
	survivor := new(models.BrandTemplate)
	err := tx.NewSelect().Model(survivor).OrderExpr("created_at ASC").Limit(1).For("UPDATE").Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil // last template deleted; nothing to promote
	}
	if err != nil {
		return err
	}
	survivor.IsDefault = true
	survivor.UpdatedAt = time.Now().UTC()
	res, err := tx.NewUpdate().Model(survivor).Column("is_default", "updated_at").WherePK().Exec(ctx)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("promote template: expected 1 row updated, got %d", n)
	}
	return nil
}

// ── shared helpers ──────────────────────────────────────────────────────────

// demoteDefault clears is_default on every row of the model except exceptID
// (empty on create). Run inside the same tx as the insert/update, and before it,
// so the partial-unique-default index never sees two defaults.
//
// The TenantScoped BeforeUpdate hook already scopes this to the caller's tenant,
// but this is a cross-row write with no bound instance, so the tenant predicate
// is also added explicitly (and fails closed with no tenant in context) —
// belt-and-braces on the one operation that touches rows other than the target.
func demoteDefault(ctx context.Context, tx bun.Tx, model any, exceptID string) error {
	tid, ok := tenantctx.From(ctx)
	if !ok {
		return tenantctx.ErrNoTenant
	}
	q := tx.NewUpdate().Model(model).
		Set("is_default = ?", false).
		Where("tenant_id = ?", tid).
		Where("is_default = ?", true)
	if exceptID != "" {
		q = q.Where("id <> ?", exceptID)
	}
	_, err := q.Exec(ctx)
	return err
}

// deleteSingleton removes the caller's one row for a per-tenant singleton. The
// explicit tenant predicate is belt-and-braces alongside the model hook and
// fails closed when no tenant is in context.
func deleteSingleton(ctx context.Context, db *bun.DB, model any) (bool, error) {
	tid, ok := tenantctx.From(ctx)
	if !ok {
		return false, tenantctx.ErrNoTenant
	}
	res, err := db.NewDelete().Model(model).Where("tenant_id = ?", tid).Exec(ctx)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}
