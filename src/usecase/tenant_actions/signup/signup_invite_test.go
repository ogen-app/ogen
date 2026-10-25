package signup_test

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/pgtest"
	"github.com/ogen-app/ogen/src/usecase/signupaccess"
	"github.com/ogen-app/ogen/src/usecase/tenant_actions/signup"
)

type tokenCapture struct {
	mu    sync.Mutex
	links []string
}

func (c *tokenCapture) EnqueueTenantInviteEmailTx(_ context.Context, _ *sql.Tx, _ string, _ int, _, link, _, _ string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.links = append(c.links, link)
	return nil
}

func (c *tokenCapture) lastToken(t *testing.T) string {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	u, err := url.Parse(c.links[len(c.links)-1])
	if err != nil {
		t.Fatal(err)
	}
	return u.Query().Get("invite")
}

type inviteFixture struct {
	db     *bun.DB
	access *signupaccess.Service
	signup *signup.Service
	links  *tokenCapture
}

func newInviteFixture(t *testing.T) *inviteFixture {
	t.Helper()
	db := pgtest.MustDB()
	t.Cleanup(func() { _ = db.Close() })
	f := &inviteFixture{db: db, links: &tokenCapture{}}
	invites := repository.NewTenantInviteRepository(db)
	versions := repository.NewTenantTierVersionRepository(db)
	f.access = signupaccess.New(signupaccess.Deps{
		DB: db, Invites: invites, Settings: repository.NewSignupSettingsRepository(db),
		Accounts: repository.NewAccountRepository(db), Tiers: repository.NewTenantTierRepository(db),
		TierVersions: versions, Groups: repository.NewTenantGroupRepository(db), Emails: f.links, AppBaseURL: "https://app",
	})
	f.signup = signup.New(db, repository.NewAccountRepository(db), repository.NewTenantRepository(db))
	f.signup.SetAccess(f.access, signup.InviteDeps{Invites: invites, TierVersions: versions, Assignments: repository.NewTenantTierAssignmentRepository(db)})
	// Billing is on: an ordinary signup is gated, an invited one never is.
	f.signup.SetBillingRequired(true)
	return f
}

func (f *inviteFixture) setMode(t *testing.T, mode string) {
	t.Helper()
	if _, err := f.access.UpdateSettings(t.Context(), signupaccess.UpdateSettingsInput{Mode: mode, MaxOwnedWorkspacesPerAccount: 2, UpdatedBy: "ops"}); err != nil {
		t.Fatal(err)
	}
}

// invite issues a tester invite on tier, in the given groups, and returns its token.
func (f *inviteFixture) invite(t *testing.T, email, tier string, groups ...string) (string, string) {
	t.Helper()
	ctx := t.Context()
	for _, g := range groups {
		if err := repository.NewTenantGroupRepository(f.db).Create(ctx, &models.TenantGroup{ID: g, Name: g, CreatedAt: time.Now(), UpdatedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	v, _, err := f.access.CreateInvite(ctx, signupaccess.CreateInviteInput{Email: email, TierID: tier, GroupIDs: groups, CreatedBy: "ops@getogen.com"})
	if err != nil {
		t.Fatal(err)
	}
	return v.ID, f.links.lastToken(t)
}

func input(email, token string) signup.Input {
	return signup.Input{TenantName: "Acme", UserName: "Jane", Email: email, Password: "correct horse battery", InviteToken: token}
}

func TestInviteRedemptionCreatesComplimentaryWorkspace(t *testing.T) {
	f := newInviteFixture(t)
	ctx := t.Context()
	f.setMode(t, models.SignupModeInviteOnly)
	inviteID, token := f.invite(t, "jane@acme.com", models.DefaultTierID, "alpha")

	res, err := f.signup.Create(ctx, input("Jane@Acme.com", token))
	if err != nil {
		t.Fatal(err)
	}
	if res.Invite == nil || res.Invite.ID != inviteID {
		t.Fatalf("result invite = %+v", res.Invite)
	}
	tenant, err := repository.NewTenantRepository(f.db).GetByID(ctx, res.Tenant.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !tenant.Complimentary || tenant.BillingRequired || tenant.InviteID == nil || *tenant.InviteID != inviteID || tenant.TierID != models.DefaultTierID {
		t.Fatalf("tenant = %+v", tenant)
	}
	if res.User.Email != "jane@acme.com" {
		t.Fatalf("account address = %q, want the invited one", res.User.Email)
	}
	a, err := repository.NewTenantTierAssignmentRepository(f.db).CoveringAt(ctx, tenant.ID, time.Now().Add(time.Second))
	if err != nil || a.Reason != models.AssignmentReasonComp || a.TierVersionID != "ttv-default-v1" {
		t.Fatalf("assignment = %+v %v", a, err)
	}
	groups, err := repository.NewTenantGroupRepository(f.db).ListByTenant(ctx, tenant.ID)
	if err != nil || len(groups) != 1 || groups[0].ID != "alpha" {
		t.Fatalf("groups = %+v %v", groups, err)
	}
	inv, err := f.access.GetInvite(ctx, inviteID)
	if err != nil || inv.Status != models.TenantInviteRedeemed || inv.RedeemedTenantID == nil || *inv.RedeemedTenantID != tenant.ID ||
		inv.RedeemedTenantName != "Acme" || !inv.RedeemedTenantComplimentary {
		t.Fatalf("invite after redemption = %+v %v", inv, err)
	}
	// The token is spent.
	if _, err := f.signup.Create(ctx, input("jane@acme.com", token)); !errors.Is(err, signup.ErrInviteInvalid) && !errors.Is(err, signup.ErrEmailInUse) {
		t.Fatalf("second redemption err = %v", err)
	}
}

func TestSignupGateAndInviteErrors(t *testing.T) {
	f := newInviteFixture(t)
	ctx := t.Context()

	// Open: self-registration works and is gated by billing.
	res, err := f.signup.Create(ctx, input("stranger@x.co", ""))
	if err != nil || !res.Tenant.BillingRequired || res.Tenant.Complimentary {
		t.Fatalf("open signup = %+v %v", res, err)
	}

	f.setMode(t, models.SignupModeInviteOnly)
	if _, err := f.signup.Create(ctx, input("other@x.co", "")); !errors.Is(err, signup.ErrInviteRequired) {
		t.Fatalf("closed signup err = %v", err)
	}
	if _, err := f.signup.Create(ctx, input("other@x.co", "not-a-token")); !errors.Is(err, signup.ErrInviteInvalid) {
		t.Fatalf("garbage token err = %v", err)
	}

	inviteID, token := f.invite(t, "li@forma.studio", models.DefaultTierID)
	if _, err := f.signup.Create(ctx, input("someone@else.io", token)); !errors.Is(err, signup.ErrInviteEmailMismatch) {
		t.Fatalf("mismatch err = %v", err)
	}
	// The mismatch rolled back: the invite is still usable.
	if _, err := f.access.Preview(ctx, token); err != nil {
		t.Fatalf("invite spent by a mismatch: %v", err)
	}
	// Invites work in open mode too.
	f.setMode(t, models.SignupModeOpen)
	res, err = f.signup.Create(ctx, input("li@forma.studio", token))
	if err != nil || !res.Tenant.Complimentary || res.Invite == nil || res.Invite.ID != inviteID {
		t.Fatalf("open-mode redemption = %+v %v", res, err)
	}
}

func TestInviteRedemptionRaceSpendsOnce(t *testing.T) {
	f := newInviteFixture(t)
	_, token := f.invite(t, "race@x.co", models.DefaultTierID)
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Go(func() {
			in := input("race@x.co", token)
			in.TenantName = "Race"
			_, errs[i] = f.signup.Create(t.Context(), in)
		})
	}
	wg.Wait()
	ok := 0
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, signup.ErrInviteInvalid), errors.Is(err, signup.ErrEmailInUse):
		default:
			t.Fatalf("unexpected err: %v", err)
		}
	}
	if ok != 1 {
		t.Fatalf("%d redemptions succeeded, want 1 (errs %v)", ok, errs)
	}
}
