package oauthlogin

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/ogen-app/ogen/src/domain/entitlements"
	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/usecase/inviteaccept"
)

// InviteJoiner joins an invitee to the invited workspace. Implemented by
// *inviteaccept.Service, which the password accept endpoint uses too.
type InviteJoiner interface {
	Pending(ctx context.Context, tokenHash string) (*models.Invitation, error)
	JoinExisting(ctx context.Context, inv *models.Invitation, account *models.Account) (*models.User, error)
	JoinNew(ctx context.Context, inv *models.Invitation, na inviteaccept.NewAccount) (*models.User, *models.Session, error)
}

// acceptInvite accepts a workspace invitation as the verified identity and
// signs it in to the invited workspace. The provider address must be the
// invited one. The identity's account is found as at sign-in: linked already,
// linked now by address (removing an unverified password), or created without
// a password. An account that is already a member just lands in the workspace.
func (s *Service) acceptInvite(ctx context.Context, st *models.OAuthState, c *Claims) (*Result, error) {
	inv, err := s.d.Invites.Pending(ctx, st.InviteHash)
	if errors.Is(err, inviteaccept.ErrInvalid) {
		return nil, &Error{Code: CodeInviteInvalid}
	}
	if err != nil {
		return nil, err
	}
	if !strings.EqualFold(strings.TrimSpace(inv.Email), c.Email) {
		return nil, &Error{Code: CodeInviteEmailMismatch}
	}

	identity, err := s.d.Identities.GetBySubject(ctx, s.d.Provider.Name(), c.Subject)
	if errors.Is(err, sql.ErrNoRows) {
		return s.acceptInviteUnlinked(ctx, inv, c)
	}
	if err != nil {
		return nil, err
	}
	account, err := s.d.Accounts.GetByID(ctx, identity.AccountID)
	if err != nil {
		return nil, err
	}
	return s.joinAndOpen(ctx, inv, account, identity.ID, &Result{})
}

// acceptInviteUnlinked handles an identity with no link yet: link it to the
// account its address matches, or create the account from the invitation.
func (s *Service) acceptInviteUnlinked(ctx context.Context, inv *models.Invitation, c *Claims) (*Result, error) {
	account, err := s.d.Accounts.GetByEmailFold(ctx, c.Email)
	if err == nil {
		l, err := s.linkExisting(ctx, account, c, false)
		if err != nil {
			return nil, err
		}
		return s.joinAndOpen(ctx, inv, account, l.identity.ID, &Result{Linked: true, PasswordCleared: l.hadPassword})
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}

	identity, err := s.newIdentity("", c)
	if err != nil {
		return nil, err
	}
	user, session, err := s.d.Invites.JoinNew(ctx, inv, inviteaccept.NewAccount{Name: displayName(c.Name, c.Email), Identity: identity})
	if err != nil {
		return nil, joinError(err)
	}
	account = &models.Account{ID: user.AccountID, Email: user.Email, Name: user.Name}
	return &Result{Session: session, Account: account, User: user, Joined: true}, nil
}

// joinAndOpen adds the membership (keeping an existing one) and opens a
// session on it, guarded on the identity still being linked.
func (s *Service) joinAndOpen(ctx context.Context, inv *models.Invitation, account *models.Account, identityID string, res *Result) (*Result, error) {
	user, err := s.d.Invites.JoinExisting(ctx, inv, account)
	switch {
	case errors.Is(err, inviteaccept.ErrAlreadyMember):
		if user, err = s.d.Users.GetMembership(ctx, account.ID, inv.TenantID); err != nil {
			return nil, err
		}
	case err != nil:
		return nil, joinError(err)
	default:
		res.Joined = true
	}
	session, err := s.newSession(account.ID, user)
	if err != nil {
		return nil, err
	}
	created, err := s.d.Sessions.CreateForIdentity(ctx, session, identityID)
	if err != nil {
		return nil, err
	}
	if !created {
		return nil, &Error{Code: CodeStateInvalid, Err: errors.New("identity unlinked during sign-in")}
	}
	res.Session, res.Account, res.User = session, account, user
	return res, nil
}

// joinError maps the accept core's refusals to redirect codes.
func joinError(err error) error {
	if _, ok := errors.AsType[*entitlements.QuotaExceededError](err); ok {
		return &Error{Code: CodeInviteSeatsFull, Err: err}
	}
	switch {
	case errors.Is(err, inviteaccept.ErrInvalid):
		return &Error{Code: CodeInviteInvalid}
	case errors.Is(err, inviteaccept.ErrAccountExists):
		// The address gained an account a moment ago; going through Google
		// again finds and links it.
		return &Error{Code: CodeStateInvalid, Err: err}
	}
	return err
}
