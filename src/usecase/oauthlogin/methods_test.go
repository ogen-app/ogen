package oauthlogin_test

import (
	"errors"
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/usecase/oauthlogin"
	"github.com/ogen-app/ogen/src/usecase/oauthlogin/oauthtest"
)

// connect runs a Settings "Connect Google" flow started by startAccount and
// finished by a browser signed in as sessionAccount.
func (f *fixture) connect(t *testing.T, startAccount, sessionAccount string, who oauthtest.Identity) (*oauthlogin.Result, error) {
	t.Helper()
	started, err := f.svc.Start(t.Context(), oauthlogin.StartInput{Intent: models.OAuthIntentLink, ReturnTo: "/settings/account", AccountID: startAccount})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	code, state := f.issuer.Authorize(t, started.AuthURL, who)
	return f.svc.Callback(t.Context(), oauthlogin.CallbackInput{State: state, CookieState: state, Code: code, SessionAccountID: sessionAccount})
}

func TestConnectFromSettings(t *testing.T) {
	f := newFixture(t)
	f.seedAccount(t, "acc-1", "jane@acme.com", "")

	res, err := f.connect(t, "acc-1", "acc-1", jane)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if res.Kind != oauthlogin.ResultConnected || !res.Linked || res.Session != nil || res.ReturnTo != "/settings/account" {
		t.Fatalf("result: %+v", res)
	}
	if f.passwordIsNull(t, "acc-1") {
		t.Fatal("connecting while signed in keeps the password")
	}
	ids, err := repository.NewAccountIdentityRepository(f.db).ListForAccount(t.Context(), "acc-1")
	if err != nil || len(ids) != 1 || ids[0].Email != jane.Email {
		t.Fatalf("identity: %+v err=%v", ids, err)
	}

	// Connecting the same Google account again is a no-op.
	res, err = f.connect(t, "acc-1", "acc-1", jane)
	if err != nil || res.Linked {
		t.Fatalf("reconnect: %+v err=%v", res, err)
	}
}

func TestConnectRefusals(t *testing.T) {
	cases := []struct {
		name           string
		otherSubject   string // links acc-2 to this Google subject
		sessionAccount string
		want           string
	}{
		{
			name:           "signed out before the callback",
			sessionAccount: "",
			want:           oauthlogin.CodeStateInvalid,
		},
		{
			name:           "signed in as someone else",
			sessionAccount: "acc-2",
			want:           oauthlogin.CodeStateInvalid,
		},
		{
			name:           "google account already signs in to another account",
			otherSubject:   jane.Subject,
			sessionAccount: "acc-1",
			want:           oauthlogin.CodeLinkedElsewhere,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.seedAccount(t, "acc-1", "jane@acme.com", "")
			f.seedAccount(t, "acc-2", "other@acme.com", tc.otherSubject)
			_, err := f.connect(t, "acc-1", tc.sessionAccount, jane)
			if got := codeOf(t, err); got != tc.want {
				t.Fatalf("code %q, want %q", got, tc.want)
			}
		})
	}

	t.Run("account already connected to another google account", func(t *testing.T) {
		f := newFixture(t)
		f.seedAccount(t, "acc-1", "jane@acme.com", "first-google-sub")
		_, err := f.connect(t, "acc-1", "acc-1", jane)
		if got := codeOf(t, err); got != oauthlogin.CodeAlreadyConnected {
			t.Fatalf("code %q", got)
		}
	})

	t.Run("start without a session", func(t *testing.T) {
		f := newFixture(t)
		if _, err := f.svc.Start(t.Context(), oauthlogin.StartInput{Intent: models.OAuthIntentLink}); !errors.Is(err, oauthlogin.ErrNotSignedIn) {
			t.Fatalf("start: %v", err)
		}
	})
}

func TestMethodsListAndUnlink(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	f.seedAccount(t, "acc-1", "jane@acme.com", jane.Subject)
	methods := oauthlogin.NewMethods(f.db, repository.NewAccountIdentityRepository(f.db), repository.NewSessionRepository(f.db))
	accounts := repository.NewAccountRepository(f.db)

	acc, err := accounts.GetByID(ctx, "acc-1")
	if err != nil {
		t.Fatal(err)
	}
	got, err := methods.List(ctx, acc)
	if err != nil || !got.HasPassword || len(got.Identities) != 1 || got.Identities[0].Provider != models.IdentityProviderGoogle {
		t.Fatalf("list: %+v err=%v", got, err)
	}

	if err := methods.Unlink(ctx, "acc-1", "github", ""); !errors.Is(err, oauthlogin.ErrUnknownProvider) {
		t.Fatalf("unknown provider: %v", err)
	}

	// Without a password the identity is the only way in.
	if err := accounts.ClearPasswordTx(ctx, nil, "acc-1"); err != nil {
		t.Fatal(err)
	}
	if err := methods.Unlink(ctx, "acc-1", models.IdentityProviderGoogle, ""); !errors.Is(err, oauthlogin.ErrLastSignInMethod) {
		t.Fatalf("passwordless unlink: %v", err)
	}

	// With a password it goes, and only the caller's session survives.
	if err := accounts.UpdatePassword(ctx, "acc-1", "some-hash"); err != nil {
		t.Fatal(err)
	}
	exp := time.Now().UTC().Add(time.Hour)
	for _, id := range []string{"keep", "other"} {
		if _, err := f.db.NewInsert().Model(&models.Session{ID: id, AccountID: "acc-1", UserID: "u-acc-1", TenantID: models.DefaultTenantID, ExpiresAt: exp}).Exec(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if err := methods.Unlink(ctx, "acc-1", models.IdentityProviderGoogle, "keep"); err != nil {
		t.Fatalf("unlink: %v", err)
	}
	if n := f.count(t, (*models.Session)(nil), "account_id = ?", "acc-1"); n != 1 {
		t.Fatalf("sessions after unlink = %d, want the caller's only", n)
	}
	if err := methods.Unlink(ctx, "acc-1", models.IdentityProviderGoogle, "keep"); !errors.Is(err, oauthlogin.ErrNotLinked) {
		t.Fatalf("second unlink: %v", err)
	}
	got, err = methods.List(ctx, acc)
	if err != nil || got.Identities == nil || len(got.Identities) != 0 {
		t.Fatalf("list after unlink must be an empty array: %+v err=%v", got, err)
	}
}
