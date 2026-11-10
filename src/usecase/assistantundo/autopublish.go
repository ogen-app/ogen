package assistantundo

import (
	"context"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
)

type allowlistState struct {
	Platforms []string `json:"platforms"`
}

// AutoPublish is the auto-publish allowlist for the undo log.
func AutoPublish(platforms []string) models.JSONMap {
	if platforms == nil {
		platforms = []string{}
	}
	return toMap(allowlistState{Platforms: platforms})
}

// WithAutoPublish lets s undo a change to the auto-publish allowlist.
func (s *Service) WithAutoPublish(allowlist repository.AutoPublishAllowlistRepository) *Service {
	s.undoers[models.AssistantActionAutoPublish] = autoPublish{allowlist}
	return s
}

type autoPublish struct {
	allowlist repository.AutoPublishAllowlistRepository
}

// Undo puts the allowlist back while it is still as the change left it.
func (u autoPublish) Undo(ctx context.Context, tx bun.IDB, a *models.AssistantAction) error {
	var before, after allowlistState
	if err := fromMap(a.Before, &before); err != nil {
		return err
	}
	if err := fromMap(a.After, &after); err != nil {
		return err
	}
	ok, err := u.allowlist.ReplaceIf(ctx, tx, after.Platforms, before.Platforms)
	if err != nil {
		return err
	}
	if !ok {
		return ErrChangedSince
	}
	return nil
}
