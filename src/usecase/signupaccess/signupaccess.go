// Package signupaccess decides who may create a workspace: the platform
// signup mode (open or invite-only, which governs self-registration only),
// operator-issued tester invites, and the per-account workspace cap that
// applies while signup is invite-only.
//
// A tester invite creates a complimentary workspace when it is redeemed; the
// redemption itself runs inside the signup transaction (package signup).
package signupaccess

import (
	"context"
	"database/sql"
	"errors"
	"expvar"
	"fmt"
	"net/mail"
	"strings"
	"sync"
	"time"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
)

// Errors callers map to HTTP or gRPC answers.
var (
	// ErrInvalidArgument wraps a field-level validation failure.
	ErrInvalidArgument = errors.New("invalid argument")
	// ErrAccountExists: the address already has an Ogen account, so it
	// cannot receive a tester invite; comp its workspace instead.
	ErrAccountExists = errors.New("account_exists")
	// ErrTierNotActive: the tier has no published version to start on.
	ErrTierNotActive = errors.New("tier_not_active")
	// ErrNotPending: resend or revoke of a redeemed or revoked invite.
	ErrNotPending = errors.New("invite is not pending")
	// ErrNotFound: unknown invite.
	ErrNotFound = errors.New("invite not found")
	// ErrInviteInvalid: an unknown, expired, revoked or redeemed token. One
	// error for all of them, so the public preview is not an oracle.
	ErrInviteInvalid = errors.New("invite invalid")
	// ErrWorkspaceLimit: the account owns as many workspaces as invite-only
	// signup allows.
	ErrWorkspaceLimit = errors.New("workspace_limit")
)

var (
	invitesCreated     = expvar.NewInt("tenant_invites_created_total")
	invitesResent      = expvar.NewInt("tenant_invites_resent_total")
	invitesRevoked     = expvar.NewInt("tenant_invites_revoked_total")
	workspaceLimitHits = expvar.NewInt("workspace_limit_total")
)

// settingsTTL bounds how long another replica serves a stale mode after an
// operator flips it.
const settingsTTL = 30 * time.Second

// EmailEnqueuer queues the invite email inside the transaction that mints or
// rotates the token. *queues.Enqueuer satisfies it.
type EmailEnqueuer interface {
	EnqueueTenantInviteEmailTx(ctx context.Context, tx *sql.Tx, inviteID string, send int, toEmail, inviteURL, expiresAt, message string) error
}

// Deps wires a Service. Emails may be nil (invites are stored, no mail).
type Deps struct {
	DB           *bun.DB
	Invites      repository.TenantInviteRepository
	Settings     repository.SignupSettingsRepository
	Accounts     repository.AccountRepository
	Tiers        repository.TenantTierRepository
	TierVersions repository.TenantTierVersionRepository
	Groups       repository.TenantGroupRepository
	Emails       EmailEnqueuer
	// AppBaseURL is the base of the emailed signup link.
	AppBaseURL string
	// Now defaults to time.Now.
	Now func() time.Time
}

// Service is the signup-access usecase. It is safe for concurrent use.
type Service struct {
	d Deps

	mu       sync.Mutex
	cached   models.SignupSettings
	cachedAt time.Time
}

// New builds a Service.
func New(d Deps) *Service {
	if d.Now == nil {
		d.Now = time.Now
	}
	return &Service{d: d}
}

func (s *Service) now() time.Time { return s.d.Now().UTC() }

// Settings returns the signup settings through a short in-process cache. A
// failed read keeps serving the last good value, or the seeded defaults (open)
// on a cold start, and reports the error.
func (s *Service) Settings(ctx context.Context) (models.SignupSettings, error) {
	s.mu.Lock()
	if !s.cachedAt.IsZero() && s.now().Sub(s.cachedAt) < settingsTTL {
		v := s.cached
		s.mu.Unlock()
		return v, nil
	}
	s.mu.Unlock()
	v, err := s.d.Settings.Get(ctx)
	if err != nil {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.cachedAt.IsZero() {
			return models.DefaultSignupSettings(), err
		}
		return s.cached, err
	}
	s.mu.Lock()
	s.cached, s.cachedAt = v, s.now()
	s.mu.Unlock()
	return v, nil
}

// UpdateSettingsInput is a whole-resource settings write.
type UpdateSettingsInput struct {
	Mode                         string
	MaxOwnedWorkspacesPerAccount int
	UpdatedBy                    string
}

// UpdateSettings validates and stores the settings, then refreshes this
// replica's cache. Other replicas pick the change up within settingsTTL.
func (s *Service) UpdateSettings(ctx context.Context, in UpdateSettingsInput) (models.SignupSettings, error) {
	in.Mode, in.UpdatedBy = strings.TrimSpace(in.Mode), strings.TrimSpace(in.UpdatedBy)
	switch {
	case in.Mode != models.SignupModeOpen && in.Mode != models.SignupModeInviteOnly:
		return models.SignupSettings{}, invalid("mode must be open or invite_only")
	case in.MaxOwnedWorkspacesPerAccount < 0:
		return models.SignupSettings{}, invalid("max_owned_workspaces_per_account must be 0 or more")
	case in.UpdatedBy == "":
		return models.SignupSettings{}, invalid("updated_by is required")
	}
	v, err := s.d.Settings.Update(ctx, models.SignupSettings{
		Mode: in.Mode, MaxOwnedWorkspacesPerAccount: in.MaxOwnedWorkspacesPerAccount,
		UpdatedAt: s.now(), UpdatedBy: in.UpdatedBy,
	})
	if err != nil {
		return v, err
	}
	s.mu.Lock()
	s.cached, s.cachedAt = v, s.now()
	s.mu.Unlock()
	return v, nil
}

// CheckWorkspaceCapTx refuses a new workspace for an account that already
// owns the invite-only maximum. It runs inside the create transaction: the
// count locks the account, so parallel creates cannot both pass. In open mode,
// or with a cap of 0, it allows everything.
func (s *Service) CheckWorkspaceCapTx(ctx context.Context, tx bun.IDB, accountID string) error {
	st, err := s.Settings(ctx)
	if err != nil {
		return err
	}
	if !st.InviteOnly() || st.MaxOwnedWorkspacesPerAccount <= 0 {
		return nil
	}
	n, err := s.d.Settings.CountOwnedWorkspacesTx(ctx, tx, accountID)
	if err != nil {
		return err
	}
	if n >= st.MaxOwnedWorkspacesPerAccount {
		workspaceLimitHits.Add(1)
		return ErrWorkspaceLimit
	}
	return nil
}

// Preview is what the signup page shows for an invite link: only the
// invited address and when the link stops working.
type Preview struct {
	Email     string    `json:"email"`
	ExpiresAt time.Time `json:"expires_at"`
}

// Preview resolves a usable invite token, or ErrInviteInvalid.
func (s *Service) Preview(ctx context.Context, token string) (Preview, error) {
	if strings.TrimSpace(token) == "" {
		return Preview{}, ErrInviteInvalid
	}
	inv, err := s.d.Invites.GetUsable(ctx, models.HashInvitationToken(token), s.now())
	if errors.Is(err, sql.ErrNoRows) {
		return Preview{}, ErrInviteInvalid
	}
	if err != nil {
		return Preview{}, err
	}
	return Preview{Email: inv.Email, ExpiresAt: inv.ExpiresAt.UTC()}, nil
}

func invalid(msg string) error { return fmt.Errorf("%w: %s", ErrInvalidArgument, msg) }

// validEmail normalises an address and checks it is a single plain address.
func validEmail(raw string) (string, error) {
	email := repository.NormalizeEmail(raw)
	addr, err := mail.ParseAddress(email)
	if email == "" || err != nil || addr.Address != email {
		return "", invalid("email is not a valid address")
	}
	return email, nil
}
