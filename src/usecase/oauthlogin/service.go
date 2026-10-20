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
	"github.com/ogen-app/ogen/src/usecase/tenant_actions/signup"
)

const component = "oauthlogin"

// sessionTTL matches a password login's session lifetime.
const sessionTTL = 7 * 24 * time.Hour

// Error codes the transport puts in the redirect after a failed sign-in. The UI
// maps each to a sentence.
const (
	CodeCancelled           = "google_cancelled"
	CodeStateInvalid        = "google_state_invalid"
	CodeEmailUnverified     = "google_email_unverified"
	CodeUnavailable         = "google_unavailable"
	CodeAlreadyConnected    = "google_already_connected"
	CodeLinkedElsewhere     = "google_linked_elsewhere"
	CodeInviteInvalid       = "invite_invalid"
	CodeInviteEmailMismatch = "invite_email_mismatch"
	CodeInviteSeatsFull     = "invite_seats_full"
	CodeNoActiveWorkspace   = "no_active_workspace"
	CodeThrottled           = "throttled"
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

// SignupCreator creates a workspace with its owning account. Implemented by
// *signup.Service.
type SignupCreator interface {
	Create(ctx context.Context, in signup.Input) (*signup.Result, error)
}

// LinkEnqueuer queues the "Google sign-in is now connected" notice inside the
// linking transaction. Implemented by *queues.Enqueuer.
type LinkEnqueuer interface {
	EnqueueGoogleLinkedEmailTx(ctx context.Context, tx *sql.Tx, userID, tenantID, identityID, googleEmail string, passwordCleared bool) error
}

// Deps are the Service's collaborators. Emails may be nil (no notice mail).
type Deps struct {
	DB         *bun.DB
	Provider   Provider
	OAuth      repository.OAuthRepository
	Identities repository.AccountIdentityRepository
	Accounts   repository.AccountRepository
	Users      repository.UserRepository
	Sessions   repository.SessionRepository
	Signup     SignupCreator
	Emails     LinkEnqueuer
	Invites    InviteJoiner
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

// StartInput is a request to begin a sign-in. AccountID is the signed-in
// account connecting the provider from Settings (intent link); InviteToken is
// the invitation being accepted (intent invite).
type StartInput struct {
	Intent      string
	ReturnTo    string
	AccountID   string
	InviteToken string
}

// ErrNotSignedIn rejects connecting a provider without a session.
var ErrNotSignedIn = errors.New("oauthlogin: sign in to connect an account")

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
	row := &models.OAuthState{Intent: intent, ReturnTo: SafeReturnTo(in.ReturnTo)}
	switch intent {
	case models.OAuthIntentSignin:
	case models.OAuthIntentLink:
		if in.AccountID == "" {
			return nil, ErrNotSignedIn
		}
		row.AccountID = in.AccountID
	case models.OAuthIntentInvite:
		if in.InviteToken == "" || s.d.Invites == nil {
			return nil, ErrInvalidIntent
		}
		row.InviteHash = models.HashInvitationToken(in.InviteToken)
	default:
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
	row.StateHash = stateHash
	row.Provider = s.d.Provider.Name()
	row.Nonce = nonce
	row.PKCEVerifier = verifier
	row.ExpiresAt = now.Add(models.OAuthStateTTL)
	row.CreatedAt = now
	if err := s.d.OAuth.CreateState(ctx, row); err != nil {
		return nil, fmt.Errorf("oauthlogin: store state: %w", err)
	}
	return &Started{AuthURL: s.d.Provider.AuthCodeURL(state, nonce, verifier), State: state}, nil
}

// CallbackInput is the provider's redirect back, plus what the browser carried:
// the state cookie and, when signed in, its session's account.
type CallbackInput struct {
	State            string
	CookieState      string
	Code             string
	ProviderError    string
	SessionAccountID string
}

// ResultKind says how a successful callback ended.
type ResultKind int

const (
	// ResultSignedIn opened a session.
	ResultSignedIn ResultKind = iota
	// ResultPendingSignup found no account: the newcomer names a workspace
	// next, with PendingToken.
	ResultPendingSignup
	// ResultConnected linked the identity to the signed-in account from
	// Settings; no new session.
	ResultConnected
)

// Result is a callback that succeeded.
type Result struct {
	Kind     ResultKind
	Intent   string
	ReturnTo string

	// Set for ResultSignedIn.
	Session *models.Session
	Account *models.Account
	User    *models.User
	// Linked is set when this sign-in linked the identity to an existing
	// account; PasswordCleared when linking removed the account's password.
	Linked          bool
	PasswordCleared bool
	// Joined is set when an invitation added the account to its workspace.
	Joined bool

	// Set for ResultPendingSignup.
	PendingToken string
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

	var res *Result
	switch st.Intent {
	case models.OAuthIntentLink:
		res, err = s.connect(ctx, st, in.SessionAccountID, claims)
	case models.OAuthIntentInvite:
		res, err = s.acceptInvite(ctx, st, claims)
	default:
		res, err = s.signIn(ctx, st, claims)
	}
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

// signIn opens a session for the account a verified identity is linked to. An
// unlinked identity whose address matches an account is linked to it; with no
// such account the newcomer is handed on to name a workspace.
func (s *Service) signIn(ctx context.Context, st *models.OAuthState, c *Claims) (*Result, error) {
	identity, err := s.d.Identities.GetBySubject(ctx, s.d.Provider.Name(), c.Subject)
	if errors.Is(err, sql.ErrNoRows) {
		account, err := s.d.Accounts.GetByEmailFold(ctx, c.Email)
		if errors.Is(err, sql.ErrNoRows) {
			return s.pendingSignup(ctx, st, c)
		}
		if err != nil {
			return nil, err
		}
		return s.signInLinking(ctx, account, c)
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
	session, err := s.newSession(account.ID, user)
	if err != nil {
		return nil, err
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
