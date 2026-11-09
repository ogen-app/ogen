// Package assistantundo reverses the changes the assistant made. Each change
// is logged with the entity's state before and after; undo restores the
// before state only while the entity still matches the after state, so an
// edit made since is never overwritten.
package assistantundo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
)

var (
	// ErrNotFound means there is no such change for this tenant.
	ErrNotFound = errors.New("assistant action not found")
	// ErrAlreadyUndone means the change was undone before.
	ErrAlreadyUndone = errors.New("this change was already undone")
	// ErrChangedSince means the entity was edited after the change, so
	// undoing it would overwrite that edit.
	ErrChangedSince = errors.New("this was changed again since, so it can't be undone")
)

// Undoer restores one kind of change.
type Undoer interface {
	// Undo restores a.Before on the entity within tx, or returns
	// ErrChangedSince when the entity no longer matches a.After. It locks
	// the entity before comparing, so no write lands between the two.
	Undo(ctx context.Context, tx bun.IDB, a *models.AssistantAction) error
}

// Service undoes logged assistant changes.
type Service struct {
	db      *bun.DB
	actions repository.AssistantActionRepository
	undoers map[models.AssistantActionKind]Undoer
}

// New returns a service that can undo campaign date, brief, platform and
// asset changes and a campaign's creation; WithIdeas adds idea verdicts.
func New(db *bun.DB, actions repository.AssistantActionRepository, campaigns repository.CampaignRepository) *Service {
	return &Service{
		db:      db,
		actions: actions,
		undoers: map[models.AssistantActionKind]Undoer{
			models.AssistantActionCampaignDates:     campaignDates{campaigns},
			models.AssistantActionCampaignBrief:     campaignBrief{campaigns},
			models.AssistantActionCampaignPlatforms: campaignPlatforms{campaigns},
			models.AssistantActionCampaignAssets:    campaignAssets{campaigns},
			models.AssistantActionCampaignCreated:   campaignCreated{campaigns},
		},
	}
}

// Undo reverses the change with the given id on behalf of user by.
func (s *Service) Undo(ctx context.Context, id, by string) (*models.AssistantAction, error) {
	a, err := s.actions.GetByID(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("load assistant action: %w", err)
	}
	if a.UndoneAt != nil {
		return nil, ErrAlreadyUndone
	}
	u, ok := s.undoers[a.Kind]
	if !ok {
		return nil, fmt.Errorf("no undo for %q changes", a.Kind)
	}
	// The record and the restore commit together, so a failure in either
	// leaves the change undoable. Recording first locks the action row: a
	// concurrent undo of the same change waits, then finds it undone.
	now := time.Now().UTC()
	err = s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		recorded, err := s.actions.MarkUndone(ctx, tx, a.ID, by, now)
		if err != nil {
			return fmt.Errorf("record undo: %w", err)
		}
		if !recorded {
			return ErrAlreadyUndone
		}
		return u.Undo(ctx, tx, a)
	})
	if err != nil {
		return nil, err
	}
	a.UndoneAt, a.UndoneBy = &now, by
	return a, nil
}

// Record logs a reversible change. It is best effort for the caller: a change
// that could not be logged is simply not undoable.
func Record(ctx context.Context, actions repository.AssistantActionRepository, a *models.AssistantAction) (string, error) {
	if actions == nil {
		return "", errors.New("assistant actions are not stored")
	}
	id, err := models.NewID()
	if err != nil {
		return "", err
	}
	a.ID = id
	if err := actions.Create(ctx, a); err != nil {
		return "", fmt.Errorf("log assistant action: %w", err)
	}
	return id, nil
}
