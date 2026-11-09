package assistantundo

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
)

// voiceSnapshot is a voice's state as an undo log stores it. Voice is nil
// for a voice that doesn't exist; Bindings are what a removed voice was bound
// to, and ReplacedDefault the voice a change to the default took the flag
// from.
type voiceSnapshot struct {
	Voice           *models.BrandVoice        `json:"voice"`
	CreatedAt       time.Time                 `json:"createdAt,omitzero"`
	Bindings        *repository.BrandBindings `json:"bindings,omitempty"`
	ReplacedDefault string                    `json:"replacedDefault,omitempty"`
}

type audienceSnapshot struct {
	Audience  *models.BrandAudience     `json:"audience"`
	CreatedAt time.Time                 `json:"createdAt,omitzero"`
	Bindings  *repository.BrandBindings `json:"bindings,omitempty"`
}

type guardrailsSnapshot struct {
	Guardrails *models.BrandGuardrails `json:"guardrails"`
}

// BrandVoice is a voice's state for the undo log; nil is a voice that
// doesn't exist. bound is what a removed voice was bound to; replaced, the
// voice that was the default before a change made v it.
func BrandVoice(v *models.BrandVoice, bound *repository.BrandBindings, replaced string) models.JSONMap {
	snap := voiceSnapshot{Voice: v, Bindings: bound, ReplacedDefault: replaced}
	if v != nil {
		snap.CreatedAt = v.CreatedAt
	}
	return toMap(snap)
}

// BrandAudience is an audience's state for the undo log; nil is an audience
// that doesn't exist. bound is what a removed audience was bound to.
func BrandAudience(a *models.BrandAudience, bound *repository.BrandBindings) models.JSONMap {
	snap := audienceSnapshot{Audience: a, Bindings: bound}
	if a != nil {
		snap.CreatedAt = a.CreatedAt
	}
	return toMap(snap)
}

// BrandGuardrails is the rules' state for the undo log; nil is none set.
func BrandGuardrails(g *models.BrandGuardrails) models.JSONMap {
	return toMap(guardrailsSnapshot{Guardrails: g})
}

func toMap(v any) models.JSONMap {
	b, err := json.Marshal(v)
	if err != nil {
		return models.JSONMap{}
	}
	var m models.JSONMap
	if err := json.Unmarshal(b, &m); err != nil {
		return models.JSONMap{}
	}
	return m
}

func fromMap(m models.JSONMap, into any) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, into)
}

// changedSince maps a conditional write's refusal to ErrChangedSince.
func changedSince(err error) error {
	if errors.Is(err, repository.ErrBrandChanged) || errors.Is(err, sql.ErrNoRows) {
		return ErrChangedSince
	}
	return err
}

// WithBrandLibrary lets s undo voice, audience and guardrail changes.
func (s *Service) WithBrandLibrary(brand repository.BrandRepository) *Service {
	s.undoers[models.AssistantActionBrandVoice] = brandVoice{brand}
	s.undoers[models.AssistantActionBrandAudience] = brandAudience{brand}
	s.undoers[models.AssistantActionBrandGuardrails] = brandGuardrails{brand}
	return s
}

type brandVoice struct{ brand repository.BrandRepository }

// Undo restores a voice as it was, within tx, while it still holds what the
// change left. An added voice is taken away only while nothing is bound to
// it; a removed one comes back bound to what has had no voice since; a voice
// made the default hands the flag back.
func (u brandVoice) Undo(ctx context.Context, tx bun.IDB, a *models.AssistantAction) error {
	var before, after voiceSnapshot
	if err := fromMap(a.Before, &before); err != nil {
		return fmt.Errorf("restore voice: %w", err)
	}
	if err := fromMap(a.After, &after); err != nil {
		return fmt.Errorf("read changed voice: %w", err)
	}
	switch was, restored := after.Voice, before.Voice; {
	case was != nil && restored == nil:
		// The bindings are read under the voice's lock; any means it is in
		// use since, and the error rolls the delete back.
		gone, bound, err := u.brand.DeleteVoiceIf(ctx, tx, a.EntityID, was)
		if err == nil && gone == nil {
			return ErrChangedSince
		}
		if err != nil {
			return changedSince(err)
		}
		if !bound.Empty() {
			return ErrChangedSince
		}
		if was.IsDefault && after.ReplacedDefault != "" {
			return u.brand.SetDefaultVoice(ctx, tx, after.ReplacedDefault)
		}
		return nil
	case was != nil:
		restored.ID, restored.UpdatedAt = a.EntityID, time.Now().UTC().Truncate(time.Microsecond)
		if _, err := u.brand.UpdateVoiceIf(ctx, tx, restored, was); err != nil {
			return changedSince(err)
		}
		if was.IsDefault && !restored.IsDefault && after.ReplacedDefault != "" {
			return u.brand.SetDefaultVoice(ctx, tx, after.ReplacedDefault)
		}
		return nil
	case restored != nil:
		restored.ID, restored.CreatedAt = a.EntityID, before.CreatedAt
		restored.UpdatedAt = time.Now().UTC().Truncate(time.Microsecond)
		var bound repository.BrandBindings
		if before.Bindings != nil {
			bound = *before.Bindings
		}
		return changedSince(u.brand.RestoreVoice(ctx, tx, restored, bound))
	}
	return errors.New("voice change logs no voice")
}

type brandAudience struct{ brand repository.BrandRepository }

// Undo restores an audience as it was, within tx, as brandVoice does.
func (u brandAudience) Undo(ctx context.Context, tx bun.IDB, a *models.AssistantAction) error {
	var before, after audienceSnapshot
	if err := fromMap(a.Before, &before); err != nil {
		return fmt.Errorf("restore audience: %w", err)
	}
	if err := fromMap(a.After, &after); err != nil {
		return fmt.Errorf("read changed audience: %w", err)
	}
	switch was, restored := after.Audience, before.Audience; {
	case was != nil && restored == nil:
		gone, bound, err := u.brand.DeleteAudienceIf(ctx, tx, a.EntityID, was)
		if err == nil && gone == nil {
			return ErrChangedSince
		}
		if err != nil {
			return changedSince(err)
		}
		if !bound.Empty() {
			return ErrChangedSince
		}
		return nil
	case was != nil:
		restored.ID, restored.UpdatedAt = a.EntityID, time.Now().UTC().Truncate(time.Microsecond)
		_, err := u.brand.UpdateAudienceIf(ctx, tx, restored, was)
		return changedSince(err)
	case restored != nil:
		restored.ID, restored.CreatedAt = a.EntityID, before.CreatedAt
		restored.UpdatedAt = time.Now().UTC().Truncate(time.Microsecond)
		var bound repository.BrandBindings
		if before.Bindings != nil {
			bound = *before.Bindings
		}
		return changedSince(u.brand.RestoreAudience(ctx, tx, restored, bound))
	}
	return errors.New("audience change logs no audience")
}

type brandGuardrails struct{ brand repository.BrandRepository }

// Undo restores the rules as they were, within tx, while they still hold
// what the change left; rules there were none of are removed again.
func (u brandGuardrails) Undo(ctx context.Context, tx bun.IDB, a *models.AssistantAction) error {
	var before, after guardrailsSnapshot
	if err := fromMap(a.Before, &before); err != nil {
		return fmt.Errorf("restore guardrails: %w", err)
	}
	if err := fromMap(a.After, &after); err != nil {
		return fmt.Errorf("read changed guardrails: %w", err)
	}
	if before.Guardrails != nil {
		before.Guardrails.UpdatedAt = time.Now().UTC().Truncate(time.Microsecond)
	}
	_, err := u.brand.SaveGuardrailsIf(ctx, tx, before.Guardrails, after.Guardrails)
	return changedSince(err)
}
