package loginsecurity

import (
	"testing"

	"github.com/ogen-app/ogen/src/domain/models"
)

func (f *fixture) linkGoogle(t *testing.T) {
	t.Helper()
	if _, err := f.db.NewInsert().Model(&models.AccountIdentity{ID: "ai-1", AccountID: f.login.AccountID, Provider: models.IdentityProviderGoogle, Subject: "sub", Email: "jane@gmail.com"}).Exec(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestGoogleSignInAlertNamesGoogle(t *testing.T) {
	f := newFixture(t)
	f.linkGoogle(t)
	f.loginFrom(t, "")
	f.login.Method = models.LoginMethodGoogle
	f.loginFrom(t, "")

	if f.emails.count() != 1 || f.emails.sent[0].vars.LoginMethod != "Google" {
		t.Fatalf("alert vars: %+v", f.emails.sent)
	}
	raw := f.secureToken(t)
	p, err := f.svc.Preview(t.Context(), raw)
	if err != nil || p.Method != models.LoginMethodGoogle {
		t.Fatalf("preview: %+v err=%v", p, err)
	}

	res, err := f.svc.Secure(t.Context(), raw)
	if err != nil || !res.IdentityRemoved {
		t.Fatalf("secure: %+v err=%v", res, err)
	}
	if n := f.count(t, (*models.AccountIdentity)(nil), "account_id = ?", f.login.AccountID); n != 0 {
		t.Fatal("securing a Google sign-in disconnects Google")
	}
}

func TestPasswordSignInAlertKeepsGoogle(t *testing.T) {
	f := newFixture(t)
	f.linkGoogle(t)
	f.loginFrom(t, "")
	f.loginFrom(t, "")

	if f.emails.sent[0].vars.LoginMethod != "" {
		t.Fatalf("a password sign-in names no provider: %+v", f.emails.sent[0].vars)
	}
	raw := f.secureToken(t)
	if p, _ := f.svc.Preview(t.Context(), raw); p.Method != models.LoginMethodPassword {
		t.Fatalf("preview method %q", p.Method)
	}
	res, err := f.svc.Secure(t.Context(), raw)
	if err != nil || res.IdentityRemoved {
		t.Fatalf("secure: %+v err=%v", res, err)
	}
	if n := f.count(t, (*models.AccountIdentity)(nil), "account_id = ?", f.login.AccountID); n != 1 {
		t.Fatal("securing a password sign-in leaves Google connected")
	}
}
