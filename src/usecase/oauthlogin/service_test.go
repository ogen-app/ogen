package oauthlogin_test

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"testing"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/pgtest"
	"github.com/ogen-app/ogen/src/usecase/inviteaccept"
	"github.com/ogen-app/ogen/src/usecase/oauthlogin"
	"github.com/ogen-app/ogen/src/usecase/oauthlogin/oauthtest"
	"github.com/ogen-app/ogen/src/usecase/tenant_actions/signup"
)

type linkedEmail struct {
	userID, identityID, googleEmail string
	passwordCleared                 bool
}

type fakeLinkEmails struct{ sent []linkedEmail }

func (f *fakeLinkEmails) EnqueueGoogleLinkedEmailTx(_ context.Context, _ *sql.Tx, userID, _, identityID, googleEmail string, passwordCleared bool) error {
	f.sent = append(f.sent, linkedEmail{userID, identityID, googleEmail, passwordCleared})
	return nil
}

type fixture struct {
	db     *bun.DB
	issuer *oauthtest.Issuer
	svc    *oauthlogin.Service
	emails *fakeLinkEmails
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	db := pgtest.MustDB()
	t.Cleanup(func() { _ = db.Close() })
	issuer := oauthtest.New(t)
	accounts := repository.NewAccountRepository(db)
	emails := &fakeLinkEmails{}
	svc := oauthlogin.New(oauthlogin.Deps{
		DB:         db,
		Provider:   issuer.Provider(t),
		OAuth:      repository.NewOAuthRepository(db),
		Identities: repository.NewAccountIdentityRepository(db),
		Accounts:   accounts,
		Users:      repository.NewUserRepository(db),
		Sessions:   repository.NewSessionRepository(db),
		Signup:     signup.New(db, accounts, repository.NewTenantRepository(db), nil),
		Emails:     emails,
		Invites: inviteaccept.New(db, repository.NewUserRepository(db), accounts,
			repository.NewInvitationRepository(db), repository.NewSessionRepository(db), nil),
	})
	return &fixture{db: db, issuer: issuer, svc: svc, emails: emails}
}

func (f *fixture) count(t *testing.T, model any, where string, args ...any) int {
	t.Helper()
	n, err := f.db.NewSelect().Model(model).Where(where, args...).Count(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func (f *fixture) passwordIsNull(t *testing.T, accountID string) bool {
	t.Helper()
	var isNull bool
	if err := f.db.NewSelect().Table("accounts").ColumnExpr("password_hash IS NULL").Where("id = ?", accountID).Scan(t.Context(), &isNull); err != nil {
		t.Fatal(err)
	}
	return isNull
}

// seedAccount creates an account with an owner membership of the default
// workspace, optionally linked to a Google subject.
func (f *fixture) seedAccount(t *testing.T, id, email, subject string) {
	t.Helper()
	ctx := t.Context()
	pw, err := models.HashPassword("old-password")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range []any{
		&models.Account{ID: id, Email: email, PasswordHash: pw, Name: "Jane"},
		&models.User{ID: "u-" + id, AccountID: id, TenantID: models.DefaultTenantID, Name: "Jane", Email: email, Role: models.RoleOwner},
	} {
		if _, err := f.db.NewInsert().Model(m).Exec(ctx); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	if subject != "" {
		if _, err := f.db.NewInsert().Model(&models.AccountIdentity{ID: "ai-" + id, AccountID: id, Provider: models.IdentityProviderGoogle, Subject: subject, Email: email}).Exec(ctx); err != nil {
			t.Fatalf("seed identity: %v", err)
		}
	}
}

// signIn runs a whole flow: start, the user signing in at the issuer as who,
// and the callback.
func (f *fixture) signIn(t *testing.T, start oauthlogin.StartInput, who oauthtest.Identity) (*oauthlogin.Result, error) {
	t.Helper()
	started, err := f.svc.Start(t.Context(), start)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	code, state := f.issuer.Authorize(t, started.AuthURL, who)
	if state != started.State {
		t.Fatalf("provider state %q != started state %q", state, started.State)
	}
	return f.svc.Callback(t.Context(), oauthlogin.CallbackInput{State: state, CookieState: started.State, Code: code})
}

func codeOf(t *testing.T, err error) string {
	t.Helper()
	e, ok := errors.AsType[*oauthlogin.Error](err)
	if !ok {
		t.Fatalf("want *oauthlogin.Error, got %v", err)
	}
	return e.Code
}

var jane = oauthtest.Identity{Subject: "google-sub-1", Email: "jane@gmail.com", EmailVerified: true, Name: "Jane"}

func TestSignInLinkedIdentity(t *testing.T) {
	f := newFixture(t)
	f.seedAccount(t, "acc-1", "jane@acme.com", jane.Subject)

	res, err := f.signIn(t, oauthlogin.StartInput{ReturnTo: "/campaigns?tab=2"}, jane)
	if err != nil {
		t.Fatalf("callback: %v", err)
	}
	if res.Account.ID != "acc-1" || res.User.ID != "u-acc-1" || res.Session.TenantID != models.DefaultTenantID {
		t.Fatalf("signed into the wrong place: %+v", res)
	}
	if res.ReturnTo != "/campaigns?tab=2" || res.Intent != models.OAuthIntentSignin {
		t.Fatalf("return path/intent: %q %q", res.ReturnTo, res.Intent)
	}
	s, err := repository.NewSessionRepository(f.db).GetByID(t.Context(), res.Session.ID)
	if err != nil || s.AccountID != "acc-1" {
		t.Fatalf("session not stored: %+v err=%v", s, err)
	}
	ids, err := repository.NewAccountIdentityRepository(f.db).ListForAccount(t.Context(), "acc-1")
	if err != nil || len(ids) != 1 || ids[0].LastUsedAt == nil || ids[0].Email != jane.Email {
		t.Fatalf("identity not marked used: %+v err=%v", ids, err)
	}
}

func TestCallbackRejectsBadState(t *testing.T) {
	f := newFixture(t)
	f.seedAccount(t, "acc-1", "jane@acme.com", jane.Subject)
	ctx := t.Context()

	started, err := f.svc.Start(ctx, oauthlogin.StartInput{})
	if err != nil {
		t.Fatal(err)
	}
	code, state := f.issuer.Authorize(t, started.AuthURL, jane)

	// A callback link opened in a browser that didn't start the flow.
	_, err = f.svc.Callback(ctx, oauthlogin.CallbackInput{State: state, Code: code})
	if got := codeOf(t, err); got != oauthlogin.CodeStateInvalid {
		t.Fatalf("no cookie: code %q", got)
	}
	// The real browser finishes once; a replay fails.
	if _, err := f.svc.Callback(ctx, oauthlogin.CallbackInput{State: state, CookieState: state, Code: code}); err != nil {
		t.Fatalf("first callback: %v", err)
	}
	_, err = f.svc.Callback(ctx, oauthlogin.CallbackInput{State: state, CookieState: state, Code: code})
	if got := codeOf(t, err); got != oauthlogin.CodeStateInvalid {
		t.Fatalf("replay: code %q", got)
	}
}

func TestCallbackOutcomes(t *testing.T) {
	cases := []struct {
		name   string
		who    oauthtest.Identity
		tamper func(map[string]any)
		cancel bool
		want   string
	}{
		{name: "user cancelled", who: jane, cancel: true, want: oauthlogin.CodeCancelled},
		{name: "unverified email", who: oauthtest.Identity{Subject: jane.Subject, Email: jane.Email}, want: oauthlogin.CodeEmailUnverified},
		{name: "token for another client", who: jane, tamper: func(c map[string]any) { c["aud"] = "someone-else" }, want: oauthlogin.CodeStateInvalid},
		{name: "token from another issuer", who: jane, tamper: func(c map[string]any) { c["iss"] = "https://evil.example" }, want: oauthlogin.CodeStateInvalid},
		{name: "replayed nonce", who: jane, tamper: func(c map[string]any) { c["nonce"] = "other" }, want: oauthlogin.CodeStateInvalid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.seedAccount(t, "acc-1", "jane@acme.com", jane.Subject)
			f.issuer.Tamper = tc.tamper
			ctx := t.Context()

			started, err := f.svc.Start(ctx, oauthlogin.StartInput{})
			if err != nil {
				t.Fatal(err)
			}
			code, state := f.issuer.Authorize(t, started.AuthURL, tc.who)
			in := oauthlogin.CallbackInput{State: state, CookieState: state, Code: code}
			if tc.cancel {
				in.Code, in.ProviderError = "", "access_denied"
			}
			_, err = f.svc.Callback(ctx, in)
			if got := codeOf(t, err); got != tc.want {
				t.Fatalf("code %q, want %q (err %v)", got, tc.want, err)
			}
		})
	}
}

func TestSignInWithoutLiveWorkspace(t *testing.T) {
	f := newFixture(t)
	f.seedAccount(t, "acc-1", "jane@acme.com", jane.Subject)
	if _, err := f.db.NewDelete().Model((*models.User)(nil)).Where("account_id = ?", "acc-1").Exec(t.Context()); err != nil {
		t.Fatal(err)
	}
	_, err := f.signIn(t, oauthlogin.StartInput{}, jane)
	if got := codeOf(t, err); got != oauthlogin.CodeNoActiveWorkspace {
		t.Fatalf("code %q", got)
	}
}

func TestStartBuildsProviderRequest(t *testing.T) {
	f := newFixture(t)
	started, err := f.svc.Start(t.Context(), oauthlogin.StartInput{})
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(started.AuthURL)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if q.Get("state") != started.State || q.Get("nonce") == "" || q.Get("code_challenge") == "" ||
		q.Get("scope") != "openid email profile" || q.Get("prompt") != "select_account" {
		t.Fatalf("auth url missing parameters: %s", started.AuthURL)
	}
	if _, err := f.svc.Start(t.Context(), oauthlogin.StartInput{Intent: "bogus"}); !errors.Is(err, oauthlogin.ErrInvalidIntent) {
		t.Fatalf("bogus intent: %v", err)
	}
}

func TestSafeReturnTo(t *testing.T) {
	cases := map[string]string{
		"":                      "/",
		"/campaigns":            "/campaigns",
		"/campaigns?x=1#top":    "/campaigns?x=1#top",
		"//evil.example/x":      "/",
		"/\\evil.example":       "/",
		"https://evil.example/": "/",
		"javascript:alert(1)":   "/",
		"campaigns":             "/",
		"/auth/login":           "/",
		"/auth":                 "/",
		"/authors":              "/authors",
		"/x\r\nSet-Cookie: a=b": "/",
	}
	for in, want := range cases {
		if got := oauthlogin.SafeReturnTo(in); got != want {
			t.Errorf("SafeReturnTo(%q) = %q, want %q", in, got, want)
		}
	}
}
