package oauthlogin_test

import (
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/usecase/oauthlogin"
	"github.com/ogen-app/ogen/src/usecase/oauthlogin/oauthtest"
)

const acmeTenant = "t-acme"

// seedInvite creates the Acme workspace with an owner and a pending invitation
// of email to it, returning the raw invitation token.
func (f *fixture) seedInvite(t *testing.T, email string) string {
	t.Helper()
	ctx := t.Context()
	token, hash, err := models.NewInvitationToken()
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range []any{
		&models.Tenant{ID: acmeTenant, Name: "Acme", Slug: "acme", TierID: models.DefaultTierID},
		&models.Account{ID: "acc-owner", Email: "owner@acme.com", PasswordHash: "x", Name: "Owner"},
		&models.User{ID: "u-owner", AccountID: "acc-owner", TenantID: acmeTenant, Name: "Owner", Email: "owner@acme.com", Role: models.RoleOwner},
		&models.Invitation{ID: "inv-1", TenantID: acmeTenant, Email: email, Role: models.RoleMember, TokenHash: hash, InvitedBy: "u-owner", ExpiresAt: time.Now().UTC().Add(time.Hour)},
	} {
		if _, err := f.db.NewInsert().Model(m).Exec(ctx); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	return token
}

func (f *fixture) acceptInvite(t *testing.T, token string, who oauthtest.Identity) (*oauthlogin.Result, error) {
	t.Helper()
	return f.signIn(t, oauthlogin.StartInput{Intent: models.OAuthIntentInvite, InviteToken: token, ReturnTo: "/campaigns"}, who)
}

func (f *fixture) inviteStatus(t *testing.T) string {
	t.Helper()
	var status string
	if err := f.db.NewSelect().Model((*models.Invitation)(nil)).Column("status").Where("id = ?", "inv-1").Scan(t.Context(), &status); err != nil {
		t.Fatal(err)
	}
	return status
}

func TestInviteCreatesGoogleOnlyAccount(t *testing.T) {
	f := newFixture(t)
	token := f.seedInvite(t, "Jane@Gmail.com")

	res, err := f.acceptInvite(t, token, jane)
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	if !res.Joined || res.Session.TenantID != acmeTenant || res.User.Role != models.RoleMember || res.ReturnTo != "/campaigns" {
		t.Fatalf("result: %+v", res)
	}
	if !f.passwordIsNull(t, res.User.AccountID) {
		t.Fatal("an account created through Google has no password")
	}
	if n := f.count(t, (*models.AccountIdentity)(nil), "account_id = ? AND subject = ?", res.User.AccountID, jane.Subject); n != 1 {
		t.Fatal("the identity is linked to the new account")
	}
	if got := f.inviteStatus(t); got != models.InvitationAccepted {
		t.Fatalf("invite status %q", got)
	}

	// The next plain sign-in lands in the joined workspace.
	again, err := f.signIn(t, oauthlogin.StartInput{}, jane)
	if err != nil || again.Session.TenantID != acmeTenant {
		t.Fatalf("sign-in after accept: %+v err=%v", again, err)
	}
}

func TestInviteJoinsExistingAccount(t *testing.T) {
	t.Run("already linked", func(t *testing.T) {
		f := newFixture(t)
		token := f.seedInvite(t, jane.Email)
		f.seedAccount(t, "acc-1", jane.Email, jane.Subject)

		res, err := f.acceptInvite(t, token, jane)
		if err != nil {
			t.Fatalf("accept: %v", err)
		}
		if !res.Joined || res.Linked || res.Account.ID != "acc-1" || res.Session.TenantID != acmeTenant {
			t.Fatalf("result: %+v", res)
		}
		if f.passwordIsNull(t, "acc-1") {
			t.Fatal("an already-linked account keeps its password")
		}
	})

	t.Run("linked by address", func(t *testing.T) {
		f := newFixture(t)
		token := f.seedInvite(t, jane.Email)
		f.seedAccount(t, "acc-1", jane.Email, "")

		res, err := f.acceptInvite(t, token, jane)
		if err != nil {
			t.Fatalf("accept: %v", err)
		}
		if !res.Joined || !res.Linked || !res.PasswordCleared || res.Session.TenantID != acmeTenant {
			t.Fatalf("result: %+v", res)
		}
		if !f.passwordIsNull(t, "acc-1") {
			t.Fatal("linking by address removes the unverified password")
		}
		if n := f.count(t, (*models.User)(nil), "account_id = ?", "acc-1"); n != 2 {
			t.Fatalf("memberships = %d, want the original plus Acme", n)
		}
	})

	t.Run("already a member", func(t *testing.T) {
		f := newFixture(t)
		token := f.seedInvite(t, jane.Email)
		f.seedAccount(t, "acc-1", jane.Email, jane.Subject)
		if _, err := f.db.NewInsert().Model(&models.User{ID: "u-acme-jane", AccountID: "acc-1", TenantID: acmeTenant, Name: "Jane", Email: jane.Email, Role: models.RoleMember}).Exec(t.Context()); err != nil {
			t.Fatal(err)
		}

		res, err := f.acceptInvite(t, token, jane)
		if err != nil {
			t.Fatalf("accept: %v", err)
		}
		if res.Joined || res.User.ID != "u-acme-jane" || res.Session.TenantID != acmeTenant {
			t.Fatalf("a member lands in the workspace without joining again: %+v", res)
		}
	})
}

func TestInviteRefusals(t *testing.T) {
	t.Run("another google address", func(t *testing.T) {
		f := newFixture(t)
		token := f.seedInvite(t, "someone.else@gmail.com")
		_, err := f.acceptInvite(t, token, jane)
		if got := codeOf(t, err); got != oauthlogin.CodeInviteEmailMismatch {
			t.Fatalf("code %q", got)
		}
		if got := f.inviteStatus(t); got != models.InvitationPending {
			t.Fatalf("a refused accept must leave the invite pending, got %q", got)
		}
	})

	t.Run("existing account on an address google isn't authoritative for", func(t *testing.T) {
		f := newFixture(t)
		who := oauthtest.Identity{Subject: "s", Email: "jane@acme.com", EmailVerified: true}
		token := f.seedInvite(t, who.Email)
		f.seedAccount(t, "acc-1", who.Email, "")
		_, err := f.acceptInvite(t, token, who)
		if got := codeOf(t, err); got != oauthlogin.CodeLinkRequiresPassword {
			t.Fatalf("code %q", got)
		}
		if f.passwordIsNull(t, "acc-1") || f.inviteStatus(t) != models.InvitationPending {
			t.Fatal("a refused link must leave the password and the invite alone")
		}
	})

	t.Run("spent invitation", func(t *testing.T) {
		f := newFixture(t)
		token := f.seedInvite(t, jane.Email)
		if _, err := f.db.NewUpdate().Model((*models.Invitation)(nil)).Set("status = ?", models.InvitationRevoked).Where("id = ?", "inv-1").Exec(t.Context()); err != nil {
			t.Fatal(err)
		}
		_, err := f.acceptInvite(t, token, jane)
		if got := codeOf(t, err); got != oauthlogin.CodeInviteInvalid {
			t.Fatalf("code %q", got)
		}
	})

	t.Run("start without a token", func(t *testing.T) {
		f := newFixture(t)
		if _, err := f.svc.Start(t.Context(), oauthlogin.StartInput{Intent: models.OAuthIntentInvite}); err == nil {
			t.Fatal("an invite flow needs its token")
		}
	})
}
