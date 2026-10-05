package plugins

import (
	"context"
	"database/sql"
	"errors"

	"github.com/ogen-app/ogen/src/domain/models"
)

var (
	// ErrConnectionNotFound: no live plugin connection with that id in the
	// workspace (404).
	ErrConnectionNotFound = errors.New("plugin connection not found")
	// ErrConnectionForbidden: a member tried to end someone else's connection
	// (403). Only its own member or a workspace owner may.
	ErrConnectionForbidden = errors.New("only the member who connected the plugin or a workspace owner can disconnect it")
)

// ListConnections returns the live plugin connections caller may see in
// tenantID: every one for an owner, their own for a member.
func (s *Service) ListConnections(ctx context.Context, tenantID string, caller *models.User) ([]models.PluginConnection, error) {
	userID := caller.ID
	if caller.Role == models.RoleOwner {
		userID = ""
	}
	return s.d.Tokens.ListActive(ctx, tenantID, userID)
}

// Disconnect revokes connection id in tenantID on behalf of caller, who must
// own it or be a workspace owner. It returns the revoked connection.
func (s *Service) Disconnect(ctx context.Context, tenantID string, caller *models.User, id string) (*models.PluginToken, error) {
	tok, err := s.d.Tokens.GetActive(ctx, tenantID, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrConnectionNotFound
	}
	if err != nil {
		return nil, err
	}
	if caller.Role != models.RoleOwner && tok.UserID != caller.ID {
		return nil, ErrConnectionForbidden
	}
	if err := s.Revoke(ctx, tenantID, id); err != nil {
		return nil, err
	}
	return tok, nil
}

// Revoke ends a plugin connection in tenantID. The plugin's next call gets a
// 401 and goes back to its connect screen.
func (s *Service) Revoke(ctx context.Context, tenantID, id string) error {
	ok, err := s.d.Tokens.Revoke(ctx, tenantID, id, s.now())
	if err != nil {
		return err
	}
	if !ok {
		return ErrConnectionNotFound
	}
	ConnectionsRevoked.Add(1)
	return nil
}
