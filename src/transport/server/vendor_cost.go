package server

import (
	"github.com/ogen-app/ogen/src/infra/anthropicadmin"
	"github.com/ogen-app/ogen/src/infra/secrets"
)

// NewAnthropicCosts builds the Anthropic cost-report reader for Harbor's
// credit reconciliation. The Admin key is read per call, so setting or
// rotating it in Harbor applies without a restart; while it is unset the
// reader answers anthropicadmin.ErrNotConfigured.
func NewAnthropicCosts(store secrets.Store) *anthropicadmin.Client {
	return anthropicadmin.New(anthropicadmin.KeyResolver(secretResolver(store, secrets.NameAnthropicAdminAPIKey)), "", 0)
}
