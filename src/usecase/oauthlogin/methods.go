package oauthlogin

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
)

// connect links a verified identity to the signed-in account that started the
// flow from Settings. The person proved both the session and the provider
// account, so the provider address may differ from the account's and the
// password stays.
func (s *Service) connect(ctx context.Context, st *models.OAuthState, sessionAccountID string, c *Claims) (*Result, error) {
	// The state names the account that started the flow; the browser must still
	// be signed in as it.
	if st.AccountID == "" || sessionAccountID != st.AccountID {
		return nil, &Error{Code: CodeStateInvalid, Err: errors.New("not signed in as the account that started connecting")}
	}
	existing, err := s.d.Identities.GetBySubject(ctx, s.d.Provider.Name(), c.Subject)
	switch {
	case err == nil && existing.AccountID == st.AccountID:
		return &Result{Kind: ResultConnected}, nil
	case err == nil:
		return nil, &Error{Code: CodeLinkedElsewhere}
	case !errors.Is(err, sql.ErrNoRows):
		return nil, err
	}

	identity, err := s.newIdentity(st.AccountID, c)
	if err != nil {
		return nil, err
	}
	err = s.d.Identities.Create(ctx, nil, identity)
	if isUniqueViolation(err) {
		// The account already has a Google account connected, or this identity
		// was linked elsewhere a moment ago.
		return nil, &Error{Code: CodeAlreadyConnected, Err: err}
	}
	if err != nil {
		return nil, fmt.Errorf("oauthlogin: connect identity: %w", err)
	}
	return &Result{Kind: ResultConnected, Linked: true}, nil
}

// Errors for removing a sign-in method.
var (
	ErrNotLinked        = errors.New("oauthlogin: provider not connected")
	ErrLastSignInMethod = errors.New("oauthlogin: can't remove the last way to sign in")
	ErrUnknownProvider  = errors.New("oauthlogin: unknown provider")
)

var knownIdentityProvider = map[string]bool{models.IdentityProviderGoogle: true}

// Methods reads and edits how an account can sign in. It works whether or not
// a provider is configured, so an account can still disconnect Google after the
// operator turns Google sign-in off.
type Methods struct {
	db         *bun.DB
	identities repository.AccountIdentityRepository
	sessions   repository.SessionRepository
}

// NewMethods returns a Methods.
func NewMethods(db *bun.DB, identities repository.AccountIdentityRepository, sessions repository.SessionRepository) *Methods {
	return &Methods{db: db, identities: identities, sessions: sessions}
}

// AuthMethods is how an account can sign in.
type AuthMethods struct {
	HasPassword bool                     `json:"has_password"`
	Identities  []models.AccountIdentity `json:"identities"`
}

// List reports the account's sign-in methods.
func (m *Methods) List(ctx context.Context, account *models.Account) (*AuthMethods, error) {
	ids, err := m.identities.ListForAccount(ctx, account.ID)
	if err != nil {
		return nil, err
	}
	if ids == nil {
		ids = []models.AccountIdentity{}
	}
	return &AuthMethods{HasPassword: account.HasPassword(), Identities: ids}, nil
}

// Unlink disconnects a provider and signs the account's other sessions out.
// It refuses when the account has no password, which would leave it with no
// way to sign in. The check and the delete run under the account row lock, so
// they can't race a password reset or a second unlink.
func (m *Methods) Unlink(ctx context.Context, accountID, provider, keepSessionID string) error {
	if !knownIdentityProvider[provider] {
		return ErrUnknownProvider
	}
	return m.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		var hasPassword bool
		if err := tx.NewSelect().Model((*models.Account)(nil)).ColumnExpr("password_hash IS NOT NULL").
			Where("id = ?", accountID).For("UPDATE").Scan(ctx, &hasPassword); err != nil {
			return err
		}
		if !hasPassword {
			if err := m.otherIdentityLeft(ctx, accountID, provider); err != nil {
				return err
			}
		}
		removed, err := m.identities.DeleteForAccount(ctx, tx, accountID, provider)
		if err != nil {
			return err
		}
		if !removed {
			return ErrNotLinked
		}
		_, err = m.sessions.DeleteAllForAccount(ctx, tx, accountID, keepSessionID)
		return err
	})
}

// otherIdentityLeft checks that a passwordless account keeps a way to sign in
// after provider is removed.
func (m *Methods) otherIdentityLeft(ctx context.Context, accountID, provider string) error {
	linked, err := m.identities.ListForAccount(ctx, accountID)
	if err != nil {
		return err
	}
	has, others := false, 0
	for _, i := range linked {
		if i.Provider == provider {
			has = true
		} else {
			others++
		}
	}
	if !has {
		return ErrNotLinked
	}
	if others == 0 {
		return ErrLastSignInMethod
	}
	return nil
}
