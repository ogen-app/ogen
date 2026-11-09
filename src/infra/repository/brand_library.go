package repository

import (
	"context"
	"database/sql"
	"errors"
	"maps"
	"slices"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
)

// ErrBrandChanged is a conditional write to a voice, an audience or the
// guardrails that found them changed since they were read, or an id to
// restore that is taken.
var ErrBrandChanged = errors.New("brand material changed since it was read")

// BrandBindings are the campaigns and posts bound to a voice or an audience.
type BrandBindings struct {
	Campaigns []string `json:"campaigns"`
	Posts     []string `json:"posts"`
}

// Empty reports whether nothing is bound.
func (b BrandBindings) Empty() bool { return len(b.Campaigns) == 0 && len(b.Posts) == 0 }

// SameVoice reports whether two voices hold the same editable fields.
func SameVoice(a, b *models.BrandVoice) bool {
	return a.Name == b.Name && a.WhenToUse == b.WhenToUse && a.IsDefault == b.IsDefault && a.Rules == b.Rules &&
		slices.Equal(a.Samples, b.Samples) && maps.Equal(a.ChannelNotes, b.ChannelNotes)
}

// SameAudience reports whether two audiences hold the same editable fields.
func SameAudience(a, b *models.BrandAudience) bool {
	return a.Name == b.Name && a.Who == b.Who && a.ReadsOn == b.ReadsOn &&
		a.ScrollsPastWhen == b.ScrollsPastWhen && a.BelievesWhen == b.BelievesWhen
}

// SameGuardrails reports whether two guardrails hold the same rules; nil is
// no guardrails.
func SameGuardrails(a, b *models.BrandGuardrails) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Disclaimer == b.Disclaimer && slices.Equal(a.MayClaim, b.MayClaim) &&
		slices.Equal(a.NeverClaim, b.NeverClaim) && slices.Equal(a.BannedWords, b.BannedWords)
}

// ── Voices ──────────────────────────────────────────────────────────────────

// UpdateVoiceIf is UpdateVoice on db (nil runs its own transaction), applied
// only while the stored voice's editable fields equal was's; otherwise
// ErrBrandChanged. It returns the voice as it was; sql.ErrNoRows when gone.
func (r *brandRepository) UpdateVoiceIf(ctx context.Context, db bun.IDB, v, was *models.BrandVoice) (*models.BrandVoice, error) {
	var before *models.BrandVoice
	err := r.inTx(ctx, db, func(ctx context.Context, tx bun.IDB) error {
		existing := new(models.BrandVoice)
		if err := tx.NewSelect().Model(existing).Where("bv.id = ?", v.ID).For("UPDATE").Scan(ctx); err != nil {
			return err
		}
		if !SameVoice(existing, was) {
			return ErrBrandChanged
		}
		before = existing
		v.TenantID, v.CreatedAt, v.Origin = existing.TenantID, existing.CreatedAt, existing.Origin
		if v.IsDefault {
			if err := demoteDefault(ctx, tx, (*models.BrandVoice)(nil), v.ID); err != nil {
				return err
			}
		}
		_, err := tx.NewUpdate().Model(v).WherePK().Exec(ctx)
		return err
	})
	return before, err
}

// DeleteVoiceIf removes a voice on db (nil runs its own transaction) while
// its editable fields equal was's (nil: whatever they are), and returns it
// with the campaigns and posts it was bound to, which are unbound. A default
// voice hands the flag to the earliest one left. A missing voice is nil.
func (r *brandRepository) DeleteVoiceIf(ctx context.Context, db bun.IDB, id string, was *models.BrandVoice) (*models.BrandVoice, BrandBindings, error) {
	var (
		gone  *models.BrandVoice
		bound BrandBindings
	)
	err := r.inTx(ctx, db, func(ctx context.Context, tx bun.IDB) error {
		target := new(models.BrandVoice)
		err := tx.NewSelect().Model(target).Where("bv.id = ?", id).For("UPDATE").Scan(ctx)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if was != nil && !SameVoice(target, was) {
			return ErrBrandChanged
		}
		if bound, err = bindings(ctx, tx, "brand_voice_id", id); err != nil {
			return err
		}
		if _, err := tx.NewDelete().Model((*models.BrandVoice)(nil)).Where("id = ?", id).Exec(ctx); err != nil {
			return err
		}
		gone = target
		if target.IsDefault {
			return promoteEarliestVoice(ctx, tx)
		}
		return nil
	})
	return gone, bound, err
}

// RestoreVoice puts a removed voice back on db (nil runs its own
// transaction), with its id, and binds it again to the campaigns and posts
// in b that have no voice since. ErrBrandChanged when its id is taken.
func (r *brandRepository) RestoreVoice(ctx context.Context, db bun.IDB, v *models.BrandVoice, b BrandBindings) error {
	return r.inTx(ctx, db, func(ctx context.Context, tx bun.IDB) error {
		n, err := tx.NewSelect().Model((*models.BrandVoice)(nil)).Where("id = ?", v.ID).Count(ctx)
		if err != nil {
			return err
		}
		if n > 0 {
			return ErrBrandChanged
		}
		if v.IsDefault {
			if err := demoteDefault(ctx, tx, (*models.BrandVoice)(nil), ""); err != nil {
				return err
			}
		}
		if _, err := tx.NewInsert().Model(v).Exec(ctx); err != nil {
			return err
		}
		return rebind(ctx, tx, "brand_voice_id", v.ID, b)
	})
}

// SetDefaultVoice makes voice id the default on db (nil runs its own
// transaction); when it is gone, the earliest voice is.
func (r *brandRepository) SetDefaultVoice(ctx context.Context, db bun.IDB, id string) error {
	return r.inTx(ctx, db, func(ctx context.Context, tx bun.IDB) error {
		if err := demoteDefault(ctx, tx, (*models.BrandVoice)(nil), id); err != nil {
			return err
		}
		res, err := tx.NewUpdate().Model((*models.BrandVoice)(nil)).Set("is_default = ?", true).Where("id = ?", id).Exec(ctx)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 1 {
			return nil
		}
		return promoteEarliestVoice(ctx, tx)
	})
}

// VoiceBindings lists the campaigns and posts bound to voice id, on db (nil
// for the repository's own).
func (r *brandRepository) VoiceBindings(ctx context.Context, db bun.IDB, id string) (BrandBindings, error) {
	if db == nil {
		db = r.db
	}
	return bindings(ctx, db, "brand_voice_id", id)
}

// ── Audiences ───────────────────────────────────────────────────────────────

// UpdateAudienceIf is UpdateAudience on db (nil runs its own transaction),
// applied only while the stored audience's editable fields equal was's;
// otherwise ErrBrandChanged. It returns the audience as it was;
// sql.ErrNoRows when gone.
func (r *brandRepository) UpdateAudienceIf(ctx context.Context, db bun.IDB, a, was *models.BrandAudience) (*models.BrandAudience, error) {
	var before *models.BrandAudience
	err := r.inTx(ctx, db, func(ctx context.Context, tx bun.IDB) error {
		existing := new(models.BrandAudience)
		if err := tx.NewSelect().Model(existing).Where("ba.id = ?", a.ID).For("UPDATE").Scan(ctx); err != nil {
			return err
		}
		if !SameAudience(existing, was) {
			return ErrBrandChanged
		}
		before = existing
		a.TenantID, a.CreatedAt, a.Origin = existing.TenantID, existing.CreatedAt, existing.Origin
		_, err := tx.NewUpdate().Model(a).WherePK().Exec(ctx)
		return err
	})
	return before, err
}

// DeleteAudienceIf removes an audience on db (nil runs its own transaction)
// while its editable fields equal was's (nil: whatever they are), and
// returns it with the campaigns and posts it was bound to. A missing
// audience is nil.
func (r *brandRepository) DeleteAudienceIf(ctx context.Context, db bun.IDB, id string, was *models.BrandAudience) (*models.BrandAudience, BrandBindings, error) {
	var (
		gone  *models.BrandAudience
		bound BrandBindings
	)
	err := r.inTx(ctx, db, func(ctx context.Context, tx bun.IDB) error {
		target := new(models.BrandAudience)
		err := tx.NewSelect().Model(target).Where("ba.id = ?", id).For("UPDATE").Scan(ctx)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if was != nil && !SameAudience(target, was) {
			return ErrBrandChanged
		}
		if bound, err = bindings(ctx, tx, "brand_audience_id", id); err != nil {
			return err
		}
		if _, err := tx.NewDelete().Model((*models.BrandAudience)(nil)).Where("id = ?", id).Exec(ctx); err != nil {
			return err
		}
		gone = target
		return nil
	})
	return gone, bound, err
}

// RestoreAudience puts a removed audience back on db (nil runs its own
// transaction), with its id, and binds it again to the campaigns and posts
// in b that have no audience since. ErrBrandChanged when its id is taken.
func (r *brandRepository) RestoreAudience(ctx context.Context, db bun.IDB, a *models.BrandAudience, b BrandBindings) error {
	return r.inTx(ctx, db, func(ctx context.Context, tx bun.IDB) error {
		n, err := tx.NewSelect().Model((*models.BrandAudience)(nil)).Where("id = ?", a.ID).Count(ctx)
		if err != nil {
			return err
		}
		if n > 0 {
			return ErrBrandChanged
		}
		if _, err := tx.NewInsert().Model(a).Exec(ctx); err != nil {
			return err
		}
		return rebind(ctx, tx, "brand_audience_id", a.ID, b)
	})
}

// AudienceBindings lists the campaigns and posts bound to audience id, on db
// (nil for the repository's own).
func (r *brandRepository) AudienceBindings(ctx context.Context, db bun.IDB, id string) (BrandBindings, error) {
	if db == nil {
		db = r.db
	}
	return bindings(ctx, db, "brand_audience_id", id)
}

// ── Guardrails ──────────────────────────────────────────────────────────────

// SaveGuardrailsIf sets the rules on db (nil runs its own transaction) only
// while the stored ones equal was (nil: none set); otherwise
// ErrBrandChanged. A nil g removes them. The ledger is left alone; a save
// clears the "no guardrails" stance, as SaveGuardrails does. It returns the
// rules as they were.
func (r *brandRepository) SaveGuardrailsIf(ctx context.Context, db bun.IDB, g, was *models.BrandGuardrails) (*models.BrandGuardrails, error) {
	var before *models.BrandGuardrails
	err := r.inTx(ctx, db, func(ctx context.Context, tx bun.IDB) error {
		if err := tenantLock(ctx, tx, "brand_guardrails"); err != nil {
			return err
		}
		existing := new(models.BrandGuardrails)
		err := tx.NewSelect().Model(existing).Limit(1).Scan(ctx)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			existing = nil
		case err != nil:
			return err
		}
		if !SameGuardrails(existing, was) {
			return ErrBrandChanged
		}
		before = existing
		if g == nil {
			if existing == nil {
				return nil
			}
			_, err := tx.NewDelete().Model((*models.BrandGuardrails)(nil)).Where("id = ?", existing.ID).Exec(ctx)
			return err
		}
		row := *g
		row.Facts = models.StringSlice{}
		if existing != nil {
			row.ID, row.CreatedAt = existing.ID, existing.CreatedAt
		}
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
		return deleteStance(ctx, tx)
	})
	return before, err
}

// ── Bindings ────────────────────────────────────────────────────────────────

func bindings(ctx context.Context, db bun.IDB, column, id string) (BrandBindings, error) {
	b := BrandBindings{Campaigns: []string{}, Posts: []string{}}
	if err := db.NewSelect().Model((*models.Campaign)(nil)).Column("id").Where("? = ?", bun.Ident(column), id).
		OrderExpr("id").Scan(ctx, &b.Campaigns); err != nil {
		return b, err
	}
	err := db.NewSelect().Model((*models.Post)(nil)).Column("id").Where("? = ?", bun.Ident(column), id).
		OrderExpr("id").Scan(ctx, &b.Posts)
	return b, err
}

// rebind sets column to id on the campaigns and posts in b that have none.
func rebind(ctx context.Context, db bun.IDB, column, id string, b BrandBindings) error {
	if len(b.Campaigns) > 0 {
		if _, err := db.NewUpdate().Model((*models.Campaign)(nil)).Set("? = ?", bun.Ident(column), id).
			Where("id IN (?)", bun.List(b.Campaigns)).Where("? IS NULL", bun.Ident(column)).Exec(ctx); err != nil {
			return err
		}
	}
	if len(b.Posts) > 0 {
		if _, err := db.NewUpdate().Model((*models.Post)(nil)).Set("? = ?", bun.Ident(column), id).
			Where("id IN (?)", bun.List(b.Posts)).Where("? IS NULL", bun.Ident(column)).Exec(ctx); err != nil {
			return err
		}
	}
	return nil
}
