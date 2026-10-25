package models

import (
	"time"

	"github.com/uptrace/bun"
)

// SignupSettingsID is the key of the single platform_signup_settings row.
const SignupSettingsID = "global"

// Signup modes. The mode governs self-registration only; tester invites work
// in both.
const (
	SignupModeOpen       = "open"
	SignupModeInviteOnly = "invite_only"
)

// DefaultMaxOwnedWorkspaces is the seeded per-account workspace cap.
const DefaultMaxOwnedWorkspaces = 2

// SignupSettings decides who may create a workspace. In invite_only mode a
// stranger cannot self-register, and an account may own at most
// MaxOwnedWorkspacesPerAccount live workspaces (0 = no cap).
type SignupSettings struct {
	bun.BaseModel `bun:"table:platform_signup_settings,alias:pss" swaggerignore:"true"`

	ID                           string    `bun:"id,pk"`
	Mode                         string    `bun:"mode,notnull"`
	MaxOwnedWorkspacesPerAccount int       `bun:"max_owned_workspaces_per_account,notnull"`
	UpdatedAt                    time.Time `bun:"updated_at,notnull"`
	UpdatedBy                    string    `bun:"updated_by,notnull"`
}

// InviteOnly reports whether self-registration is closed.
func (s SignupSettings) InviteOnly() bool { return s.Mode == SignupModeInviteOnly }

// DefaultSignupSettings is what the seed row holds: open, cap 2.
func DefaultSignupSettings() SignupSettings {
	return SignupSettings{ID: SignupSettingsID, Mode: SignupModeOpen, MaxOwnedWorkspacesPerAccount: DefaultMaxOwnedWorkspaces}
}
