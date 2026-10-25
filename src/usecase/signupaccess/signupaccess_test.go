package signupaccess_test

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/pgtest"
	"github.com/ogen-app/ogen/src/usecase/signupaccess"
)

// sentInvite is one queued invite email.
type sentInvite struct {
	inviteID string
	send     int
	to       string
	link     string
	message  string
}

type fakeEmails struct {
	mu   sync.Mutex
	sent []sentInvite
}

func (f *fakeEmails) EnqueueTenantInviteEmailTx(_ context.Context, _ *sql.Tx, inviteID string, send int, toEmail, inviteURL, _, message string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, sentInvite{inviteID, send, toEmail, inviteURL, message})
	return nil
}

func (f *fakeEmails) last(t *testing.T) sentInvite {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.sent) == 0 {
		t.Fatal("no invite email queued")
	}
	return f.sent[len(f.sent)-1]
}

// token is the raw invite token carried by a queued link.
func (s sentInvite) token(t *testing.T) string {
	t.Helper()
	u, err := url.Parse(s.link)
	if err != nil {
		t.Fatal(err)
	}
	return u.Query().Get("invite")
}

type fixture struct {
	db     *bun.DB
	svc    *signupaccess.Service
	emails *fakeEmails
	now    time.Time
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	db := pgtest.MustDB()
	t.Cleanup(func() { _ = db.Close() })
	f := &fixture{db: db, emails: &fakeEmails{}, now: time.Now().UTC().Truncate(time.Microsecond)}
	f.svc = signupaccess.New(signupaccess.Deps{
		DB: db, Invites: repository.NewTenantInviteRepository(db), Settings: repository.NewSignupSettingsRepository(db),
		Accounts: repository.NewAccountRepository(db), Tiers: repository.NewTenantTierRepository(db),
		TierVersions: repository.NewTenantTierVersionRepository(db), Groups: repository.NewTenantGroupRepository(db),
		Emails: f.emails, AppBaseURL: "https://app.example/", Now: func() time.Time { return f.now },
	})
	return f
}

func (f *fixture) group(t *testing.T, id string) {
	t.Helper()
	if err := repository.NewTenantGroupRepository(f.db).Create(t.Context(), &models.TenantGroup{ID: id, Name: id, CreatedAt: f.now, UpdatedAt: f.now}); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) invite(t *testing.T, email string) *models.TenantInviteView {
	t.Helper()
	v, _, err := f.svc.CreateInvite(t.Context(), signupaccess.CreateInviteInput{Email: email, TierID: models.DefaultTierID, CreatedBy: "ops@getogen.com"})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestCreateInviteValidatesAndQueuesEmail(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	f.group(t, "alpha")

	bad := []struct {
		name string
		in   signupaccess.CreateInviteInput
		want error
	}{
		{"bad email", signupaccess.CreateInviteInput{Email: "not an email", TierID: "default", CreatedBy: "ops"}, signupaccess.ErrInvalidArgument},
		{"no creator", signupaccess.CreateInviteInput{Email: "a@b.co", TierID: "default"}, signupaccess.ErrInvalidArgument},
		{"unknown tier", signupaccess.CreateInviteInput{Email: "a@b.co", TierID: "nope", CreatedBy: "ops"}, signupaccess.ErrInvalidArgument},
		{"draft-only tier", signupaccess.CreateInviteInput{Email: "a@b.co", TierID: "pro", CreatedBy: "ops"}, signupaccess.ErrTierNotActive},
		{"unknown group", signupaccess.CreateInviteInput{Email: "a@b.co", TierID: "default", GroupIDs: []string{"ghost"}, CreatedBy: "ops"}, signupaccess.ErrInvalidArgument},
		{"ttl out of range", signupaccess.CreateInviteInput{Email: "a@b.co", TierID: "default", ExpiresInHours: 721, CreatedBy: "ops"}, signupaccess.ErrInvalidArgument},
		{"long message", signupaccess.CreateInviteInput{Email: "a@b.co", TierID: "default", Message: strings.Repeat("x", 1001), CreatedBy: "ops"}, signupaccess.ErrInvalidArgument},
	}
	for _, tc := range bad {
		if _, _, err := f.svc.CreateInvite(ctx, tc.in); !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", tc.name, err, tc.want)
		}
	}

	if _, err := f.db.NewInsert().Model(&models.Account{ID: "acc-x", Email: "Taken@Example.com", Name: "x"}).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.svc.CreateInvite(ctx, signupaccess.CreateInviteInput{Email: "taken@example.com", TierID: "default", CreatedBy: "ops"}); !errors.Is(err, signupaccess.ErrAccountExists) {
		t.Fatalf("existing account err = %v", err)
	}

	// A hidden (non-purchasable) tier is a valid invite tier.
	v, replaced, err := f.svc.CreateInvite(ctx, signupaccess.CreateInviteInput{
		Email: " Jane@Acme.com ", TierID: models.DefaultTierID, GroupIDs: []string{"alpha", "alpha"},
		Message: "Break it.", Note: "met at a meetup", CreatedBy: "ops@getogen.com",
	})
	if err != nil || replaced {
		t.Fatalf("create = %v replaced=%v", err, replaced)
	}
	if v.Email != "jane@acme.com" || v.TierPurchasable || len(v.Groups) != 1 || v.Groups[0].ID != "alpha" ||
		v.DerivedStatus(f.now) != models.TenantInvitePending || v.SentCount != 1 || !v.ExpiresAt.Equal(f.now.Add(14*24*time.Hour)) {
		t.Fatalf("view = %+v", v)
	}
	mail := f.emails.last(t)
	if mail.to != "jane@acme.com" || mail.send != 1 || mail.message != "Break it." || !strings.HasPrefix(mail.link, "https://app.example/signup?invite=") {
		t.Fatalf("mail = %+v", mail)
	}
	p, err := f.svc.Preview(ctx, mail.token(t))
	if err != nil || p.Email != "jane@acme.com" {
		t.Fatalf("preview = %+v %v", p, err)
	}

	// Re-inviting the address replaces the pending invite: the old link dies.
	old := mail.token(t)
	v2, replaced, err := f.svc.CreateInvite(ctx, signupaccess.CreateInviteInput{Email: "jane@acme.com", TierID: "default", CreatedBy: "ops"})
	if err != nil || !replaced || v2.ID == v.ID {
		t.Fatalf("re-invite = %v replaced=%v", err, replaced)
	}
	if _, err := f.svc.Preview(ctx, old); !errors.Is(err, signupaccess.ErrInviteInvalid) {
		t.Fatalf("old link err = %v", err)
	}
	if first, _ := f.svc.GetInvite(ctx, v.ID); first.DerivedStatus(f.now) != models.TenantInviteRevoked {
		t.Fatalf("replaced invite status = %s", first.Status)
	}
}

func TestResendRotatesTokenAndRevokeKillsIt(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	v := f.invite(t, "sam@kite.co")
	first := f.emails.last(t).token(t)

	f.now = f.now.Add(15 * 24 * time.Hour) // the invite has expired
	if got, _ := f.svc.GetInvite(ctx, v.ID); got.DerivedStatus(f.now) != models.TenantInviteExpired {
		t.Fatalf("status = %s", got.DerivedStatus(f.now))
	}
	if _, err := f.svc.Preview(ctx, first); !errors.Is(err, signupaccess.ErrInviteInvalid) {
		t.Fatalf("expired preview err = %v", err)
	}
	r, err := f.svc.ResendInvite(ctx, v.ID, 48)
	if err != nil || r.SentCount != 2 || !r.ExpiresAt.Equal(f.now.Add(48*time.Hour)) {
		t.Fatalf("resend = %+v %v", r, err)
	}
	mail := f.emails.last(t)
	if mail.send != 2 || mail.token(t) == first {
		t.Fatalf("resend mail = %+v", mail)
	}
	if _, err := f.svc.Preview(ctx, mail.token(t)); err != nil {
		t.Fatalf("new link preview: %v", err)
	}

	if _, err := f.svc.RevokeInvite(ctx, v.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Preview(ctx, mail.token(t)); !errors.Is(err, signupaccess.ErrInviteInvalid) {
		t.Fatalf("revoked preview err = %v", err)
	}
	if _, err := f.svc.ResendInvite(ctx, v.ID, 0); !errors.Is(err, signupaccess.ErrNotPending) {
		t.Fatalf("resend revoked err = %v", err)
	}
	if _, err := f.svc.RevokeInvite(ctx, v.ID); !errors.Is(err, signupaccess.ErrNotPending) {
		t.Fatalf("revoke twice err = %v", err)
	}
	if _, err := f.svc.RevokeInvite(ctx, "missing"); !errors.Is(err, signupaccess.ErrNotFound) {
		t.Fatalf("revoke missing err = %v", err)
	}
}

func TestListInvitesFiltersAndPages(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	var ids []string
	for i, e := range []string{"a@x.co", "b@x.co", "c@y.co", "d@y.co"} {
		f.now = f.now.Add(time.Duration(i+1) * time.Minute)
		ids = append(ids, f.invite(t, e).ID)
	}
	if _, err := f.svc.RevokeInvite(ctx, ids[0]); err != nil {
		t.Fatal(err)
	}

	page, more, err := f.svc.ListInvites(ctx, signupaccess.ListInvitesInput{PageSize: 2})
	if err != nil || !more || len(page) != 2 || page[0].ID != ids[3] || page[1].ID != ids[2] {
		t.Fatalf("page 1 = %v more=%v err=%v", inviteIDs(page), more, err)
	}
	last := page[1]
	page, more, err = f.svc.ListInvites(ctx, signupaccess.ListInvitesInput{PageSize: 2, AfterCreatedAt: last.CreatedAt, AfterID: last.ID})
	if err != nil || more || len(page) != 2 || page[0].ID != ids[1] || page[1].ID != ids[0] {
		t.Fatalf("page 2 = %v more=%v err=%v", inviteIDs(page), more, err)
	}

	if page, _, _ = f.svc.ListInvites(ctx, signupaccess.ListInvitesInput{Status: models.TenantInviteRevoked}); len(page) != 1 || page[0].ID != ids[0] {
		t.Fatalf("revoked = %v", inviteIDs(page))
	}
	if page, _, _ = f.svc.ListInvites(ctx, signupaccess.ListInvitesInput{EmailQuery: "Y.CO"}); len(page) != 2 {
		t.Fatalf("email query = %v", inviteIDs(page))
	}
	f.now = f.now.Add(30 * 24 * time.Hour)
	if page, _, _ = f.svc.ListInvites(ctx, signupaccess.ListInvitesInput{Status: models.TenantInviteExpired}); len(page) != 3 {
		t.Fatalf("expired = %v", inviteIDs(page))
	}
	if page, _, _ = f.svc.ListInvites(ctx, signupaccess.ListInvitesInput{Status: models.TenantInvitePending}); len(page) != 0 {
		t.Fatalf("pending after expiry = %v", inviteIDs(page))
	}
	if _, _, err := f.svc.ListInvites(ctx, signupaccess.ListInvitesInput{Status: "bogus"}); !errors.Is(err, signupaccess.ErrInvalidArgument) {
		t.Fatalf("bad status err = %v", err)
	}
}

func inviteIDs(vs []models.TenantInviteView) []string {
	out := make([]string, len(vs))
	for i := range vs {
		out[i] = vs[i].ID
	}
	return out
}

func TestSettingsAndWorkspaceCap(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	st, err := f.svc.Settings(ctx)
	if err != nil || st.Mode != models.SignupModeOpen || st.MaxOwnedWorkspacesPerAccount != 2 {
		t.Fatalf("seeded settings = %+v %v", st, err)
	}
	if _, err := f.svc.UpdateSettings(ctx, signupaccess.UpdateSettingsInput{Mode: "closed", UpdatedBy: "ops"}); !errors.Is(err, signupaccess.ErrInvalidArgument) {
		t.Fatalf("bad mode err = %v", err)
	}
	if _, err := f.svc.UpdateSettings(ctx, signupaccess.UpdateSettingsInput{Mode: models.SignupModeInviteOnly, MaxOwnedWorkspacesPerAccount: -1, UpdatedBy: "ops"}); !errors.Is(err, signupaccess.ErrInvalidArgument) {
		t.Fatalf("negative cap err = %v", err)
	}
	if _, err := f.svc.UpdateSettings(ctx, signupaccess.UpdateSettingsInput{Mode: models.SignupModeInviteOnly, MaxOwnedWorkspacesPerAccount: 1}); !errors.Is(err, signupaccess.ErrInvalidArgument) {
		t.Fatalf("missing updated_by err = %v", err)
	}
	st, err = f.svc.UpdateSettings(ctx, signupaccess.UpdateSettingsInput{Mode: models.SignupModeInviteOnly, MaxOwnedWorkspacesPerAccount: 1, UpdatedBy: "ops@getogen.com"})
	if err != nil || !st.InviteOnly() || st.UpdatedBy != "ops@getogen.com" {
		t.Fatalf("update = %+v %v", st, err)
	}
	if st, _ = f.svc.Settings(ctx); !st.InviteOnly() {
		t.Fatal("cache not refreshed by the update")
	}

	// One owned workspace fills a cap of 1; a member seat doesn't count.
	seed := func(tenantID string, role models.Role) {
		t.Helper()
		if _, err := f.db.NewInsert().Model(&models.Tenant{ID: tenantID, Name: tenantID, Slug: tenantID, TierID: models.DefaultTierID, CreatedAt: f.now, UpdatedAt: f.now}).Exec(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := f.db.NewInsert().Model(&models.User{ID: "u-" + tenantID, AccountID: "acc-cap", TenantID: tenantID, Name: "cap", Email: "cap@example.com", Role: role}).Exec(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.db.NewInsert().Model(&models.Account{ID: "acc-cap", Email: "cap@example.com", Name: "cap"}).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	check := func() error {
		return f.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
			return f.svc.CheckWorkspaceCapTx(ctx, tx, "acc-cap")
		})
	}
	seed("t-member", models.RoleMember)
	if err := check(); err != nil {
		t.Fatalf("member seat counted: %v", err)
	}
	seed("t-owned", models.RoleOwner)
	if err := check(); !errors.Is(err, signupaccess.ErrWorkspaceLimit) {
		t.Fatalf("cap err = %v", err)
	}
	// Open mode lifts the cap.
	if _, err := f.svc.UpdateSettings(ctx, signupaccess.UpdateSettingsInput{Mode: models.SignupModeOpen, MaxOwnedWorkspacesPerAccount: 1, UpdatedBy: "ops"}); err != nil {
		t.Fatal(err)
	}
	if err := check(); err != nil {
		t.Fatalf("open mode capped: %v", err)
	}
}
