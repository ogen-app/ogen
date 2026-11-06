package assistantundo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
)

// BrandFact is a fact's state, as an undo log stores it; nil is a fact that
// doesn't exist (before a create, after a delete).
func BrandFact(f *models.BrandFact) models.JSONMap {
	if f == nil {
		return models.JSONMap{"exists": "no"}
	}
	date := func(d *models.CalendarDate) any {
		if d == nil {
			return nil
		}
		return d.String()
	}
	return models.JSONMap{
		"exists": "yes", "statement": f.Statement, "subject": string(f.Subject), "kind": string(f.Kind),
		"source": f.Source, "addedAt": date(f.AddedAt), "checkedAt": date(f.CheckedAt), "expiresAt": date(f.ExpiresAt),
	}
}

// WithBrand lets s undo fact ledger changes.
func (s *Service) WithBrand(brand repository.BrandRepository) *Service {
	s.undoers[models.AssistantActionBrandFact] = brandFact{brand}
	return s
}

type brandFact struct{ brand repository.BrandRepository }

func (u brandFact) Undo(ctx context.Context, a *models.AssistantAction) error {
	cur, err := u.brand.GetFact(ctx, a.EntityID)
	if errors.Is(err, sql.ErrNoRows) {
		cur, err = nil, nil
	}
	if err != nil {
		return fmt.Errorf("load fact: %w", err)
	}
	if !same(BrandFact(cur), a.After) {
		return ErrChangedSince
	}
	if a.Before["exists"] != "yes" {
		// It was added: take it away.
		_, err := u.brand.DeleteFact(ctx, a.EntityID)
		return err
	}
	f := &models.BrandFact{ID: a.EntityID, UpdatedAt: time.Now().UTC().Truncate(time.Microsecond)}
	f.Statement, _ = a.Before["statement"].(string)
	subject, _ := a.Before["subject"].(string)
	kind, _ := a.Before["kind"].(string)
	f.Subject, f.Kind = models.FactSubject(subject), models.FactKind(kind)
	f.Source, _ = a.Before["source"].(string)
	for key, dst := range map[string]**models.CalendarDate{"addedAt": &f.AddedAt, "checkedAt": &f.CheckedAt, "expiresAt": &f.ExpiresAt} {
		if s, ok := a.Before[key].(string); ok && s != "" {
			d, err := models.ParseCalendarDate(s)
			if err != nil {
				return fmt.Errorf("restore %s: %w", key, err)
			}
			*dst = &d
		}
	}
	if cur == nil {
		// It was removed: put it back.
		f.CreatedBy, f.CreatedAt = &a.CreatedBy, f.UpdatedAt
		return u.brand.CreateFact(ctx, f)
	}
	_, err = u.brand.UpdateFact(ctx, f)
	return err
}
