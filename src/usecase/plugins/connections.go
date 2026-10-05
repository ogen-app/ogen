package plugins

import (
	"context"
	"errors"
)

// ErrConnectionNotFound: no live plugin connection with that id in the
// workspace (404).
var ErrConnectionNotFound = errors.New("plugin connection not found")

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
	return nil
}
