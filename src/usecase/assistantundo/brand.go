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
// doesn't exist (before a create, after a delete). Its author and creation
// time are kept so a removed fact comes back as it was; undo compares only
// the editable fields.
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
	var by any
	if f.CreatedBy != nil {
		by = *f.CreatedBy
	}
	var created any
	if !f.CreatedAt.IsZero() {
		created = timeValue(&f.CreatedAt)
	}
	return models.JSONMap{
		"exists": "yes", "statement": f.Statement, "subject": string(f.Subject), "kind": string(f.Kind),
		"source": f.Source, "addedAt": date(f.AddedAt), "checkedAt": date(f.CheckedAt), "expiresAt": date(f.ExpiresAt),
		"createdBy": by, "createdByName": f.CreatedByName, "createdAt": created,
	}
}

// brandFactOf reads a BrandFact snapshot back; nil is a fact that doesn't
// exist. Snapshots logged before the author was kept leave it unset.
func brandFactOf(id string, m models.JSONMap) (*models.BrandFact, error) {
	if m["exists"] != "yes" {
		return nil, nil
	}
	f := &models.BrandFact{ID: id}
	f.Statement, _ = m["statement"].(string)
	subject, _ := m["subject"].(string)
	kind, _ := m["kind"].(string)
	f.Subject, f.Kind = models.FactSubject(subject), models.FactKind(kind)
	f.Source, _ = m["source"].(string)
	for key, dst := range map[string]**models.CalendarDate{"addedAt": &f.AddedAt, "checkedAt": &f.CheckedAt, "expiresAt": &f.ExpiresAt} {
		if s, ok := m[key].(string); ok && s != "" {
			d, err := models.ParseCalendarDate(s)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", key, err)
			}
			*dst = &d
		}
	}
	if by, ok := m["createdBy"].(string); ok && by != "" {
		f.CreatedBy = &by
	}
	f.CreatedByName, _ = m["createdByName"].(string)
	created, err := parseTime(m["createdAt"])
	if err != nil {
		return nil, fmt.Errorf("createdAt: %w", err)
	}
	if created != nil {
		f.CreatedAt = *created
	}
	return f, nil
}

// WithBrand lets s undo fact ledger changes.
func (s *Service) WithBrand(brand repository.BrandRepository) *Service {
	s.undoers[models.AssistantActionBrandFact] = brandFact{brand}
	return s
}

type brandFact struct{ brand repository.BrandRepository }

// Undo restores a fact as it was. Taking back an add or a correction checks
// the fact against the logged after state under its row lock, in the same
// write, so an edit made since is never deleted or overwritten.
func (u brandFact) Undo(ctx context.Context, a *models.AssistantAction) error {
	was, err := brandFactOf(a.EntityID, a.After)
	if err != nil {
		return fmt.Errorf("read changed fact: %w", err)
	}
	restored, err := brandFactOf(a.EntityID, a.Before)
	if err != nil {
		return fmt.Errorf("restore fact: %w", err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	switch {
	case was != nil && restored == nil:
		// It was added: take it away.
		_, err = u.brand.DeleteFactIf(ctx, a.EntityID, was)
	case was != nil:
		restored.UpdatedAt = now
		_, err = u.brand.UpdateFactIf(ctx, restored, was)
	case restored != nil:
		// It was removed: put it back, unless it is back already.
		cur, err := u.brand.GetFact(ctx, a.EntityID)
		if err != nil {
			return fmt.Errorf("load fact: %w", err)
		}
		if cur != nil {
			return ErrChangedSince
		}
		if _, kept := a.Before["createdByName"]; !kept {
			restored.CreatedBy = &a.CreatedBy
		}
		if restored.CreatedAt.IsZero() {
			restored.CreatedAt = now
		}
		restored.UpdatedAt = now
		return u.brand.RestoreFact(ctx, restored)
	default:
		return errors.New("fact change logs no fact")
	}
	if errors.Is(err, repository.ErrFactChanged) || errors.Is(err, sql.ErrNoRows) {
		return ErrChangedSince
	}
	return err
}
