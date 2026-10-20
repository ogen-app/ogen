// Package oauthlogin signs people in through an external OpenID Connect
// provider (Google). It runs the server side of the authorization-code flow
// with PKCE, state and nonce: Start records a single-use state and returns the
// provider URL, and Callback spends that state, verifies the ID token and
// resolves the account the identity is linked to.
//
// The transport owns cookies, redirects, throttling and activity events; this
// package owns the flow's rules and the database writes.
package oauthlogin

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/uptrace/bun"
	"golang.org/x/oauth2"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/logging"
)

const component = "oauthlogin"

// sessionTTL matches a password login's session lifetime.
const sessionTTL = 7 * 24 * time.Hour

// Error codes the transport puts in the redirect after a failed sign-in. The UI
// maps each to a sentence.
const (
	CodeCancelled         = "google_cancelled"
	CodeStateInvalid      = "google_state_invalid"
	CodeEmailUnverified   = "google_email_unverified"
	CodeUnavailable       = "google_unavailable"
	CodeNotLinked         = "google_not_linked"
	CodeNoActiveWorkspace = "no_active_workspace"
	CodeThrottled         = "throttled"
)

// Error is a sign-in that ended without a session. Intent and ReturnTo come
// from the spent state when it could be read, so the transport can send a
// Settings flow back to Settings.
type Error struct {
	Code     string
	Intent   string
	ReturnTo string
	Err      error
}

func (e *Error) Error() string {
	if e.Err != nil {
		return e.Code + ": " + e.Err.Error()
	}
	return e.Code
}

func (e *Error) Unwrap() error { return e.Err }

// ErrInvalidIntent rejects a Start with an intent this service doesn't run.
var ErrInvalidIntent = errors.New("oauthlogin: unsupported intent")

// Deps are the Service's collaborators.
type Deps struct {
	DB         *bun.DB
	Provider   Provider
	OAuth      repository.OAuthRepository
	Identities repository.AccountIdentityRepository
	Accounts   repository.AccountRepository
	Users      repository.UserRepository
	Sessions   repository.SessionRepository
}

// Service runs provider sign-ins.
type Service struct {
	d Deps
	// now is injectable so tests can pin the clock. nil → time.Now().UTC().
	now func() time.Time
}

// New returns a Service.
func New(d Deps) *Service { return &Service{d: d} }

func (s *Service) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now().UTC()
}

// ProviderName is the provider key, e.g. "google".
func (s *Service) ProviderName() string { return s.d.Provider.Name() }

// StartInput is a request to begin a sign-in.
type StartInput struct {
	Intent   string
	ReturnTo string
}

// Started is what the transport needs to send the browser to the provider.
type Started struct {
	// AuthURL is the provider's sign-in page.
	AuthURL string
	// State is the raw state; the transport keeps it in a cookie scoped to the
	// callback so only the browser that started the flow can finish it.
	State string
}

// Start records a single-use state for a new sign-in and returns where to send
// the browser.
func (s *Service) Start(ctx context.Context, in StartInput) (*Started, error) {
	intent := in.Intent
	if intent == "" {
		intent = models.OAuthIntentSignin
	}
	if intent != models.OAuthIntentSignin {
		return nil, ErrInvalidIntent
	}
	state, stateHash, err := models.NewOAuthToken()
	if err != nil {
		return nil, err
	}
	nonce, _, err := models.NewOAuthToken()
	if err != nil {
		return nil, err
	}
	verifier := oauth2.GenerateVerifier()
	now := s.clock()
	row := &models.OAuthState{
		StateHash:    stateHash,
		Provider:     s.d.Provider.Name(),
		Intent:       intent,
		Nonce:        nonce,
		PKCEVerifier: verifier,
		ReturnTo:     SafeReturnTo(in.ReturnTo),
		ExpiresAt:    now.Add(models.OAuthStateTTL),
		CreatedAt:    now,
	}
	if err := s.d.OAuth.CreateState(ctx, row); err != nil {
		return nil, fmt.Errorf("oauthlogin: store state: %w", err)
	}
	return &Started{AuthURL: s.d.Provider.AuthCodeURL(state, nonce, verifier), State: state}, nil
}

// CallbackInput is the provider's redirect back, plus the state cookie the
// browser carried.
type CallbackInput struct {
	State         string
	CookieState   string
	Code          string
	ProviderError string
}

// Result is a sign-in that opened a session.
type Result struct {
	Intent   string
	ReturnTo string
	Session  *models.Session
	Account  *models.Account
	User     *models.User
}

// Callback finishes a sign-in. Every failure is an *Error carrying the code for
// the redirect.
func (s *Service) Callback(ctx context.Context, in CallbackInput) (*Result, error) {
	st, err := s.spendState(ctx, in)
	if err != nil {
		return nil, err
	}
	fail := func(code string, err error) error {
		return &Error{Code: code, Intent: st.Intent, ReturnTo: st.ReturnTo, Err: err}
	}
	if in.ProviderError != "" {
		return nil, fail(CodeCancelled, nil)
	}
	if in.Code == "" {
		return nil, fail(CodeStateInvalid, errors.New("no authorization code"))
	}

	claims, err := s.d.Provider.Exchange(ctx, in.Code, st.PKCEVerifier, st.Nonce)
	if errors.Is(err, ErrTokenInvalid) {
		return nil, fail(CodeStateInvalid, err)
	}
	if err != nil {
		slog.WarnContext(ctx, "provider code exchange failed",
			logging.AttrComponent, component, logging.AttrError, err)
		return nil, fail(CodeUnavailable, err)
	}
	if !claims.EmailVerified {
		return nil, fail(CodeEmailUnverified, nil)
	}

	res, err := s.signIn(ctx, claims)
	if err != nil {
		if e, ok := errors.AsType[*Error](err); ok {
			e.Intent, e.ReturnTo = st.Intent, st.ReturnTo
			return nil, e
		}
		return nil, err
	}
	res.Intent, res.ReturnTo = st.Intent, st.ReturnTo
	return res, nil
}

// spendState checks the browser's cookie against the redirect's state and spends
// the stored row, so a callback can't be replayed or completed in a browser that
// didn't start it.
func (s *Service) spendState(ctx context.Context, in CallbackInput) (*models.OAuthState, error) {
	if in.State == "" || in.CookieState != in.State || !models.ValidOAuthToken(in.State) {
		return nil, &Error{Code: CodeStateInvalid, Err: errors.New("state missing or not bound to this browser")}
	}
	st, err := s.d.OAuth.ConsumeState(ctx, models.HashOAuthToken(in.State), s.clock())
	if errors.Is(err, sql.ErrNoRows) {
		return nil, &Error{Code: CodeStateInvalid, Err: errors.New("state unknown, spent or expired")}
	}
	if err != nil {
		return nil, err
	}
	if st.Provider != s.d.Provider.Name() {
		return nil, &Error{Code: CodeStateInvalid, Err: errors.New("state issued for another provider")}
	}
	return st, nil
}

// signIn opens a session for the account a verified identity is linked to.
func (s *Service) signIn(ctx context.Context, c *Claims) (*Result, error) {
	identity, err := s.d.Identities.GetBySubject(ctx, s.d.Provider.Name(), c.Subject)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, &Error{Code: CodeNotLinked}
	}
	if err != nil {
		return nil, err
	}
	res, err := s.openSession(ctx, identity)
	if err != nil {
		return nil, err
	}
	if err := s.d.Identities.MarkUsed(ctx, identity.ID, c.Email, s.clock()); err != nil {
		slog.WarnContext(ctx, "mark identity used failed",
			logging.AttrComponent, component, logging.AttrError, err)
	}
	return res, nil
}

// openSession opens a session on the account's oldest live workspace, guarded on
// the identity still being linked.
func (s *Service) openSession(ctx context.Context, identity *models.AccountIdentity) (*Result, error) {
	account, err := s.d.Accounts.GetByID(ctx, identity.AccountID)
	if err != nil {
		return nil, fmt.Errorf("oauthlogin: load account: %w", err)
	}
	user, err := s.d.Users.GetByAccountID(ctx, account.ID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, &Error{Code: CodeNoActiveWorkspace}
	}
	if err != nil {
		return nil, err
	}
	token, err := models.NewSessionToken()
	if err != nil {
		return nil, err
	}
	now := s.clock()
	session := &models.Session{
		ID:        token,
		AccountID: account.ID,
		UserID:    user.ID,
		TenantID:  user.TenantID,
		ExpiresAt: now.Add(sessionTTL),
		CreatedAt: now,
	}
	created, err := s.d.Sessions.CreateForIdentity(ctx, session, identity.ID)
	if err != nil {
		return nil, err
	}
	if !created {
		// Disconnected between the lookup and the insert.
		return nil, &Error{Code: CodeStateInvalid, Err: errors.New("identity unlinked during sign-in")}
	}
	return &Result{Session: session, Account: account, User: user}, nil
}

// maxReturnToLen bounds the stored return path.
const maxReturnToLen = 512

// SafeReturnTo keeps a post-sign-in destination only when it is a path on this
// site, so the flow can't be used as an open redirect. Auth pages fall back to
// the home page: landing back on the login form after signing in is a dead end.
func SafeReturnTo(raw string) string {
	if raw == "" || len(raw) > maxReturnToLen || !strings.HasPrefix(raw, "/") ||
		strings.HasPrefix(raw, "//") || strings.ContainsAny(raw, "\\\r\n\t") {
		return "/"
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "" || u.Host != "" || u.User != nil {
		return "/"
	}
	if u.Path == "/auth" || strings.HasPrefix(u.Path, "/auth/") {
		return "/"
	}
	return raw
}
