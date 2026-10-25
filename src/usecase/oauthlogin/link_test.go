package oauthlogin_test

import (
	"errors"
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/usecase/oauthlogin"
	"github.com/ogen-app/ogen/src/usecase/oauthlogin/oauthtest"
	"github.com/ogen-app/ogen/src/usecase/tenant_actions/signup"
)

func TestSignInLinksMatchingAccount(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	// Registered with a password under a differently-cased address.
	f.seedAccount(t, "acc-1", "Jane@Gmail.com", "")
	exp := time.Now().UTC().Add(time.Hour)
	if _, err := f.db.NewInsert().Model(&models.Session{ID: "old-session", AccountID: "acc-1", UserID: "u-acc-1", TenantID: models.DefaultTenantID, ExpiresAt: exp}).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.NewInsert().Model(&models.PasswordResetToken{ID: "rt", UserID: "u-acc-1", TenantID: models.DefaultTenantID, TokenHash: "h", ExpiresAt: exp}).Exec(ctx); err != nil {
		t.Fatal(err)
	}

	res, err := f.signIn(t, oauthlogin.StartInput{ReturnTo: "/campaigns"}, jane)
	if err != nil {
		t.Fatalf("callback: %v", err)
	}
	if res.Kind != oauthlogin.ResultSignedIn || !res.Linked || !res.PasswordCleared || res.Account.ID != "acc-1" {
		t.Fatalf("result: %+v", res)
	}
	if !f.passwordIsNull(t, "acc-1") {
		t.Fatal("linking must remove the never-verified password")
	}
	if n := f.count(t, (*models.Session)(nil), "account_id = ?", "acc-1"); n != 1 {
		t.Fatalf("sessions after link = %d, want only the new one", n)
	}
	if n := f.count(t, (*models.Session)(nil), "id = ?", res.Session.ID); n != 1 {
		t.Fatal("the new session must be stored")
	}
	if n := f.count(t, (*models.PasswordResetToken)(nil), "consumed_at IS NULL"); n != 0 {
		t.Fatal("pending reset links must be voided")
	}
	identity, err := repository.NewAccountIdentityRepository(f.db).GetBySubject(ctx, models.IdentityProviderGoogle, jane.Subject)
	if err != nil || identity.AccountID != "acc-1" {
		t.Fatalf("identity not linked: %+v err=%v", identity, err)
	}
	if len(f.emails.sent) != 1 || f.emails.sent[0] != (linkedEmail{"u-acc-1", identity.ID, jane.Email, true}) {
		t.Fatalf("linked notice: %+v", f.emails.sent)
	}

	// The next sign-in goes straight through the identity.
	res, err = f.signIn(t, oauthlogin.StartInput{}, jane)
	if err != nil || res.Linked {
		t.Fatalf("second sign-in: %+v err=%v", res, err)
	}
}

func TestSignInLinksOnlyWhereGoogleIsAuthoritative(t *testing.T) {
	cases := []struct {
		name string
		who  oauthtest.Identity
		link bool
	}{
		{"consumer account on another domain", oauthtest.Identity{Subject: "s", Email: "jane@acme.com", EmailVerified: true}, false},
		{"workspace account for another domain", oauthtest.Identity{Subject: "s", Email: "jane@acme.com", EmailVerified: true, HostedDomain: "other.com"}, false},
		{"workspace account for its own domain", oauthtest.Identity{Subject: "s", Email: "jane@acme.com", EmailVerified: true, HostedDomain: "Acme.com"}, true},
		{"googlemail address", oauthtest.Identity{Subject: "s", Email: "jane@googlemail.com", EmailVerified: true}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.seedAccount(t, "acc-1", tc.who.Email, "")

			res, err := f.signIn(t, oauthlogin.StartInput{}, tc.who)
			if tc.link {
				if err != nil || !res.Linked {
					t.Fatalf("want a link: %+v err=%v", res, err)
				}
				return
			}
			if got := codeOf(t, err); got != oauthlogin.CodeLinkRequiresPassword {
				t.Fatalf("code %q", got)
			}
			if f.passwordIsNull(t, "acc-1") {
				t.Fatal("a refused link must leave the password alone")
			}
			if n := f.count(t, (*models.AccountIdentity)(nil), "account_id = ?", "acc-1"); n != 0 {
				t.Fatal("a refused link must not link the identity")
			}
		})
	}
}

func TestUnauthoritativeAddressStillSignsUp(t *testing.T) {
	f := newFixture(t)
	res, err := f.signIn(t, oauthlogin.StartInput{}, oauthtest.Identity{Subject: "s", Email: "new@acme.com", EmailVerified: true})
	if err != nil || res.Kind != oauthlogin.ResultPendingSignup {
		t.Fatalf("no account to take over, so a newcomer can still sign up: %+v err=%v", res, err)
	}
}

func TestSignInRefusesSecondGoogleAccount(t *testing.T) {
	f := newFixture(t)
	f.seedAccount(t, "acc-1", jane.Email, "another-google-sub")
	_, err := f.signIn(t, oauthlogin.StartInput{}, jane)
	if got := codeOf(t, err); got != oauthlogin.CodeAlreadyConnected {
		t.Fatalf("code %q", got)
	}
	if f.passwordIsNull(t, "acc-1") {
		t.Fatal("a refused link must leave the password alone")
	}
}

func TestSignInLinksAccountWithoutWorkspace(t *testing.T) {
	f := newFixture(t)
	f.seedAccount(t, "acc-1", jane.Email, "")
	if _, err := f.db.NewDelete().Model((*models.User)(nil)).Where("account_id = ?", "acc-1").Exec(t.Context()); err != nil {
		t.Fatal(err)
	}
	_, err := f.signIn(t, oauthlogin.StartInput{}, jane)
	if got := codeOf(t, err); got != oauthlogin.CodeNoActiveWorkspace {
		t.Fatalf("code %q", got)
	}
	if n := f.count(t, (*models.AccountIdentity)(nil), "account_id = ?", "acc-1"); n != 1 {
		t.Fatal("the identity is linked even when there is no workspace to open")
	}
}

func TestGoogleSignup(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()

	res, err := f.signIn(t, oauthlogin.StartInput{ReturnTo: "/campaigns"}, jane)
	if err != nil {
		t.Fatalf("callback: %v", err)
	}
	if res.Kind != oauthlogin.ResultPendingSignup || res.PendingToken == "" || res.Session != nil {
		t.Fatalf("want a pending signup, got %+v", res)
	}
	if n := f.count(t, (*models.Account)(nil), "1 = 1"); n != 0 {
		t.Fatal("no account exists until the workspace is named")
	}

	preview, err := f.svc.PendingSignup(ctx, res.PendingToken)
	if err != nil || preview.Email != jane.Email || preview.Name != jane.Name {
		t.Fatalf("preview: %+v err=%v", preview, err)
	}

	done, err := f.svc.CompleteSignup(ctx, res.PendingToken, "Acme", "")
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if done.Tenant.Name != "Acme" || done.User.Role != models.RoleOwner || done.User.Email != jane.Email || done.ReturnTo != "/campaigns" {
		t.Fatalf("completed: %+v", done)
	}
	if !f.passwordIsNull(t, done.User.AccountID) {
		t.Fatal("a Google signup has no password")
	}
	if n := f.count(t, (*models.AccountIdentity)(nil), "account_id = ? AND subject = ?", done.User.AccountID, jane.Subject); n != 1 {
		t.Fatal("the identity is linked in the signup transaction")
	}

	if _, err := f.svc.CompleteSignup(ctx, res.PendingToken, "Acme again", ""); !errors.Is(err, oauthlogin.ErrPendingNotFound) {
		t.Fatalf("double submit: %v", err)
	}
	if _, err := f.svc.PendingSignup(ctx, res.PendingToken); !errors.Is(err, oauthlogin.ErrPendingNotFound) {
		t.Fatalf("preview after use: %v", err)
	}

	again, err := f.signIn(t, oauthlogin.StartInput{}, jane)
	if err != nil || again.Kind != oauthlogin.ResultSignedIn || again.Account.ID != done.User.AccountID {
		t.Fatalf("sign-in after signup: %+v err=%v", again, err)
	}
}

func TestGoogleSignupLosesRaceToPasswordSignup(t *testing.T) {
	f := newFixture(t)
	res, err := f.signIn(t, oauthlogin.StartInput{}, jane)
	if err != nil || res.Kind != oauthlogin.ResultPendingSignup {
		t.Fatalf("callback: %+v err=%v", res, err)
	}
	// Someone registers the address with a password while the form is open.
	f.seedAccount(t, "acc-1", "JANE@gmail.com", "")
	if _, err := f.svc.CompleteSignup(t.Context(), res.PendingToken, "Acme", ""); !errors.Is(err, signup.ErrEmailInUse) {
		t.Fatalf("complete: %v, want ErrEmailInUse", err)
	}
}

func TestPendingSignupRejectsGarbage(t *testing.T) {
	f := newFixture(t)
	for _, tok := range []string{"", "short", "not-a-real-token-but-the-right-length-xxxxx"} {
		if _, err := f.svc.PendingSignup(t.Context(), tok); !errors.Is(err, oauthlogin.ErrPendingNotFound) {
			t.Errorf("preview %q: %v", tok, err)
		}
		if _, err := f.svc.CompleteSignup(t.Context(), tok, "Acme", ""); !errors.Is(err, oauthlogin.ErrPendingNotFound) {
			t.Errorf("complete %q: %v", tok, err)
		}
	}
}
