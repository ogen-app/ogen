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
	"github.com/ogen-app/ogen/src/usecase/oauthlogin"
	"github.com/ogen-app/ogen/src/usecase/signupaccess"
	"github.com/ogen-app/ogen/src/usecase/tenant_actions/signup"
)

type testerLinks struct{ last string }

func (l *testerLinks) EnqueueTenantInviteEmailTx(_ context.Context, _ *sql.Tx, _ string, _ int, _, link, _, _ string) error {
	l.last = link
	return nil
}

// TestGoogleSignupRedeemsTesterInvite: while signup is invite-only a Google
// newcomer needs a tester invite; the gate refuses before the pending signup is
// spent, so the same pending signup then completes with the invite.
func TestGoogleSignupRedeemsTesterInvite(t *testing.T) {
	var access *signupaccess.Service
	links := &testerLinks{}
	f := newFixture(t, func(db *bun.DB, s *signup.Service) {
		invites := repository.NewTenantInviteRepository(db)
		versions := repository.NewTenantTierVersionRepository(db)
		access = signupaccess.New(signupaccess.Deps{
			DB: db, Invites: invites, Settings: repository.NewSignupSettingsRepository(db),
			Accounts: repository.NewAccountRepository(db), Tiers: repository.NewTenantTierRepository(db),
			TierVersions: versions, Groups: repository.NewTenantGroupRepository(db), Emails: links, AppBaseURL: "https://app",
		})
		s.SetAccess(access, signup.InviteDeps{Invites: invites, TierVersions: versions, Assignments: repository.NewTenantTierAssignmentRepository(db)})
	})
	ctx := t.Context()
	if _, err := access.UpdateSettings(ctx, signupaccess.UpdateSettingsInput{Mode: models.SignupModeInviteOnly, MaxOwnedWorkspacesPerAccount: 2, UpdatedBy: "ops"}); err != nil {
		t.Fatal(err)
	}

	res, err := f.signIn(t, oauthlogin.StartInput{ReturnTo: "/"}, jane)
	if err != nil || res.Kind != oauthlogin.ResultPendingSignup {
		t.Fatalf("callback = %+v %v", res, err)
	}
	if _, err := f.svc.CompleteSignup(ctx, res.PendingToken, "Acme", ""); !errors.Is(err, signup.ErrInviteRequired) {
		t.Fatalf("closed signup err = %v", err)
	}

	if _, _, err := access.CreateInvite(ctx, signupaccess.CreateInviteInput{Email: "someone-else@example.com", TierID: models.DefaultTierID, CreatedBy: "ops"}); err != nil {
		t.Fatal(err)
	}
	wrong := inviteToken(t, links.last)
	if _, err := f.svc.CompleteSignup(ctx, res.PendingToken, "Acme", wrong); !errors.Is(err, signup.ErrInviteEmailMismatch) {
		t.Fatalf("mismatch err = %v", err)
	}

	// The mismatch spent the pending signup (it is single-use), so sign in again.
	res, err = f.signIn(t, oauthlogin.StartInput{ReturnTo: "/"}, jane)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := access.CreateInvite(ctx, signupaccess.CreateInviteInput{Email: jane.Email, TierID: models.DefaultTierID, CreatedBy: "ops"}); err != nil {
		t.Fatal(err)
	}
	done, err := f.svc.CompleteSignup(ctx, res.PendingToken, "Acme", inviteToken(t, links.last))
	if err != nil {
		t.Fatalf("complete with invite: %v", err)
	}
	if done.Invite == nil || !done.Tenant.Complimentary || done.Tenant.BillingRequired {
		t.Fatalf("completed = %+v", done.Result)
	}
}

func inviteToken(t *testing.T, link string) string {
	t.Helper()
	u, err := url.Parse(link)
	if err != nil {
		t.Fatal(err)
	}
	return u.Query().Get("invite")
}
