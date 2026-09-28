package loginsecurity

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
	"github.com/ogen-app/ogen/src/jobs/queues"
	"github.com/ogen-app/ogen/src/pgtest"
)

type fakeGeo struct{}

func (fakeGeo) Lookup(ip string) string {
	if ip == "203.0.113.42" {
		return "Kyiv, Ukraine"
	}
	return ""
}

type sentAlert struct {
	userID, tenantID, tokenID string
	vars                      queues.NewDeviceLoginVars
}

type fakeEmails struct {
	mu   sync.Mutex
	sent []sentAlert
}

func (f *fakeEmails) EnqueueNewDeviceLoginEmailTx(_ context.Context, _ *sql.Tx, userID, tenantID, tokenID string, v queues.NewDeviceLoginVars) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, sentAlert{userID, tenantID, tokenID, v})
	return nil
}

func (f *fakeEmails) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sent)
}

type fixture struct {
	db     *bun.DB
	svc    *Service
	emails *fakeEmails
	now    time.Time
	login  Login
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	db := pgtest.MustDB()
	t.Cleanup(func() { _ = db.Close() })

	f := &fixture{db: db, emails: &fakeEmails{}, now: time.Date(2026, 9, 28, 14, 3, 0, 0, time.UTC)}
	f.svc = New(Deps{
		DB:         db,
		Devices:    repository.NewKnownDeviceRepository(db),
		Alerts:     repository.NewLoginAlertTokenRepository(db),
		Sessions:   repository.NewSessionRepository(db),
		Users:      repository.NewUserRepository(db),
		Accounts:   repository.NewAccountRepository(db),
		Geo:        fakeGeo{},
		Emails:     f.emails,
		AppBaseURL: "https://app.example/",
	})
	f.svc.now = func() time.Time { return f.now }

	ctx := t.Context()
	pw, err := models.HashPassword("old-password")
	if err != nil {
		t.Fatal(err)
	}
	acc := &models.Account{ID: "acc-1", Email: "jane@acme.com", PasswordHash: pw, Name: "Jane"}
	if _, err := db.NewInsert().Model(acc).Exec(ctx); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	u := &models.User{ID: "user-1", AccountID: acc.ID, TenantID: models.DefaultTenantID, Name: "Jane", Email: acc.Email, Role: models.RoleOwner}
	if _, err := db.NewInsert().Model(u).Exec(ctx); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	f.login = Login{
		AccountID: acc.ID, UserID: u.ID, TenantID: u.TenantID, Email: acc.Email,
		IP: "203.0.113.42", UserAgent: "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/129.0.0.0 Safari/537.36",
	}
	return f
}

func (f *fixture) count(t *testing.T, model any, where string, args ...any) int {
	t.Helper()
	n, err := f.db.NewSelect().Model(model).Where(where, args...).Count(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func (f *fixture) devices(t *testing.T) int {
	return f.count(t, (*models.KnownDevice)(nil), "account_id = ?", f.login.AccountID)
}

// loginFrom observes a login carrying the given device cookie.
func (f *fixture) loginFrom(t *testing.T, cookie string) string {
	t.Helper()
	in := f.login
	in.DeviceToken = cookie
	tok := f.svc.Observe(t.Context(), in)
	if !models.ValidDeviceToken(tok) {
		t.Fatalf("Observe returned an unusable device token %q", tok)
	}
	return tok
}

// secureToken extracts the raw alert token from the last email's link.
func (f *fixture) secureToken(t *testing.T) string {
	t.Helper()
	f.emails.mu.Lock()
	defer f.emails.mu.Unlock()
	if len(f.emails.sent) == 0 {
		t.Fatal("no alert was sent")
	}
	u, err := url.Parse(f.emails.sent[len(f.emails.sent)-1].vars.SecureURL)
	if err != nil {
		t.Fatal(err)
	}
	return u.Query().Get("token")
}

func TestObserveFirstDeviceIsSilent(t *testing.T) {
	f := newFixture(t)
	f.loginFrom(t, "")
	if f.devices(t) != 1 || f.emails.count() != 0 {
		t.Fatalf("first device: %d devices, %d emails; want 1 and 0", f.devices(t), f.emails.count())
	}
}

func TestObserveKnownDeviceIsSilent(t *testing.T) {
	f := newFixture(t)
	cookie := f.loginFrom(t, "")
	f.now = f.now.Add(time.Hour)
	if again := f.loginFrom(t, cookie); again != cookie {
		t.Fatalf("a known device keeps its token, got %q", again)
	}
	if f.devices(t) != 1 || f.emails.count() != 0 {
		t.Fatalf("known device: %d devices, %d emails; want 1 and 0", f.devices(t), f.emails.count())
	}
	d := new(models.KnownDevice)
	if err := f.db.NewSelect().Model(d).Where("account_id = ?", f.login.AccountID).Scan(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !d.LastSeenAt.Equal(f.now) {
		t.Fatalf("last_seen_at = %v, want %v", d.LastSeenAt, f.now)
	}
}

func TestObserveNewDeviceSendsAlert(t *testing.T) {
	f := newFixture(t)
	f.loginFrom(t, "")
	f.loginFrom(t, "not-a-real-cookie") // malformed counts as no cookie

	if f.devices(t) != 2 || f.emails.count() != 1 {
		t.Fatalf("new device: %d devices, %d emails; want 2 and 1", f.devices(t), f.emails.count())
	}
	got := f.emails.sent[0]
	if got.userID != "user-1" || got.tenantID != models.DefaultTenantID {
		t.Fatalf("alert addressed to %s/%s", got.userID, got.tenantID)
	}
	want := queues.NewDeviceLoginVars{
		LoginTime:   "28 Sep 2026, 14:03 UTC",
		DeviceLabel: "Chrome on macOS",
		IPAddress:   "203.0.113.42",
		Location:    "Kyiv, Ukraine",
		MaskedEmail: "j***@acme.com",
	}
	if !strings.HasPrefix(got.vars.SecureURL, "https://app.example/auth/secure-account?token=") {
		t.Fatalf("secure url = %q", got.vars.SecureURL)
	}
	got.vars.SecureURL = ""
	if got.vars != want {
		t.Fatalf("vars = %+v, want %+v", got.vars, want)
	}
	if n := f.count(t, (*models.LoginAlertToken)(nil), "id = ?", got.tokenID); n != 1 {
		t.Fatalf("alert token row for %s: %d", got.tokenID, n)
	}
}

func TestObserveCapsAlertsPerHour(t *testing.T) {
	f := newFixture(t)
	f.loginFrom(t, "")
	for range alertsPerHour + 2 {
		f.loginFrom(t, "")
	}
	if f.emails.count() != alertsPerHour {
		t.Fatalf("sent %d alerts, want the cap of %d", f.emails.count(), alertsPerHour)
	}
	if f.devices(t) != alertsPerHour+3 {
		t.Fatalf("suppressed logins must still enrol: %d devices", f.devices(t))
	}
	f.now = f.now.Add(alertWindow + time.Minute)
	f.loginFrom(t, "")
	if f.emails.count() != alertsPerHour+1 {
		t.Fatal("the cap must reset after the window")
	}
}

// The retention sweep can remove every device of a dormant account; that must
// not turn its next unfamiliar login back into a silent "first device".
func TestObserveAlertsAfterRetentionEmptiedDevices(t *testing.T) {
	f := newFixture(t)
	f.loginFrom(t, "")
	if _, err := f.db.NewDelete().Model((*models.KnownDevice)(nil)).Where("account_id = ?", f.login.AccountID).Exec(t.Context()); err != nil {
		t.Fatal(err)
	}
	f.loginFrom(t, "")
	if f.emails.count() != 1 {
		t.Fatalf("login after the sweep emptied the devices sent %d alerts, want 1", f.emails.count())
	}
}

func TestEnrollIsSilentAndMakesTheDeviceKnown(t *testing.T) {
	f := newFixture(t)
	tok := f.svc.Enroll(t.Context(), f.login)
	if !models.ValidDeviceToken(tok) {
		t.Fatalf("Enroll token %q", tok)
	}
	f.loginFrom(t, tok)
	if f.devices(t) != 1 || f.emails.count() != 0 {
		t.Fatalf("login after enrol: %d devices, %d emails; want 1 and 0", f.devices(t), f.emails.count())
	}
}

func TestPreviewAndSecure(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	f.loginFrom(t, "")
	f.loginFrom(t, "")
	f.loginFrom(t, "") // a second pending alert, voided by securing
	raw := f.secureToken(t)

	for _, id := range []string{"sess-a", "sess-b"} {
		s := &models.Session{ID: id, AccountID: "acc-1", UserID: "user-1", TenantID: models.DefaultTenantID, ExpiresAt: f.now.Add(time.Hour)}
		if _, err := f.db.NewInsert().Model(s).Exec(ctx); err != nil {
			t.Fatal(err)
		}
	}

	p, err := f.svc.Preview(ctx, raw)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if p.Status != models.LoginAlertPending || p.Device != "Chrome on macOS" || p.IP != "203.0.113.42" ||
		p.Location != "Kyiv, Ukraine" || p.Email != "j***@acme.com" || !p.LoginAt.Equal(f.now) {
		t.Fatalf("preview = %+v", p)
	}
	if _, err := f.svc.Preview(ctx, raw); err != nil {
		t.Fatal("preview must not spend the token")
	}

	res, err := f.svc.Secure(ctx, raw)
	if err != nil {
		t.Fatalf("secure: %v", err)
	}
	if res.SessionsRevoked != 2 || f.count(t, (*models.Session)(nil), "account_id = ?", "acc-1") != 0 {
		t.Fatalf("sessions revoked = %d", res.SessionsRevoked)
	}
	if f.devices(t) != 0 {
		t.Fatal("securing must forget every device")
	}
	acc := new(models.Account)
	if err := f.db.NewSelect().Model(acc).Where("a.id = ?", "acc-1").Scan(ctx); err != nil {
		t.Fatal(err)
	}
	if ok, _ := models.VerifyPassword("old-password", acc.PasswordHash); ok {
		t.Fatal("securing must stop the old password from working")
	}
	if n := f.count(t, (*models.LoginAlertToken)(nil), "account_id = ? AND consumed_at IS NULL", "acc-1"); n != 0 {
		t.Fatalf("%d alert tokens still pending", n)
	}

	u, err := url.Parse(res.ResetURL)
	if err != nil || !strings.HasPrefix(res.ResetURL, "https://app.example/auth/reset?token=") {
		t.Fatalf("reset url = %q", res.ResetURL)
	}
	reset := new(models.PasswordResetToken)
	if err := f.db.NewSelect().Model(reset).Where("token_hash = ?", models.HashResetToken(u.Query().Get("token"))).Scan(ctx); err != nil {
		t.Fatalf("reset token not stored: %v", err)
	}
	if reset.UserID != "user-1" || reset.ConsumedAt != nil {
		t.Fatalf("reset token = %+v", reset)
	}

	if p, _ := f.svc.Preview(ctx, raw); p.Status != models.LoginAlertUsed {
		t.Fatalf("status after secure = %q", p.Status)
	}
	if _, err := f.svc.Secure(ctx, raw); !errors.Is(err, ErrUsed) {
		t.Fatalf("second secure = %v, want ErrUsed", err)
	}

	// After securing, the device table is empty, so the next login enrols silently.
	before := f.emails.count()
	f.loginFrom(t, "")
	if f.emails.count() != before {
		t.Fatal("the first login after securing must not alert")
	}
}

func TestSecureRejectsUnusableTokens(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	f.loginFrom(t, "")
	f.loginFrom(t, "")
	raw := f.secureToken(t)

	for _, bad := range []string{"", "short", strings.Repeat("A", 43)} {
		if _, err := f.svc.Secure(ctx, bad); !errors.Is(err, ErrNotFound) {
			t.Fatalf("Secure(%q) = %v, want ErrNotFound", bad, err)
		}
		if _, err := f.svc.Preview(ctx, bad); !errors.Is(err, ErrNotFound) {
			t.Fatalf("Preview(%q) = %v, want ErrNotFound", bad, err)
		}
	}

	f.now = f.now.Add(models.LoginAlertTokenTTL)
	if p, _ := f.svc.Preview(ctx, raw); p.Status != models.LoginAlertExpired {
		t.Fatalf("status at expiry = %q", p.Status)
	}
	if _, err := f.svc.Secure(ctx, raw); !errors.Is(err, ErrExpired) {
		t.Fatalf("secure after expiry = %v, want ErrExpired", err)
	}
}

func TestSecureConcurrentSubmitsSpendOnce(t *testing.T) {
	f := newFixture(t)
	f.loginFrom(t, "")
	f.loginFrom(t, "")
	raw := f.secureToken(t)

	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() {
			_, err := f.svc.Secure(t.Context(), raw)
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	var ok, used int
	for err := range errs {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, ErrUsed):
			used++
		default:
			t.Fatalf("secure: %v", err)
		}
	}
	if ok != 1 || used != 1 {
		t.Fatalf("concurrent secure: %d ok, %d used; want 1 and 1", ok, used)
	}
	if n := f.count(t, (*models.PasswordResetToken)(nil), "user_id = ?", "user-1"); n != 1 {
		t.Fatalf("%d reset tokens minted, want 1", n)
	}
}

func TestMaskEmail(t *testing.T) {
	for in, want := range map[string]string{
		"jane@acme.com": "j***@acme.com",
		"ö@x.io":        "ö***@x.io",
		"no-at-sign":    "***",
		"@acme.com":     "***",
	} {
		if got := MaskEmail(in); got != want {
			t.Errorf("MaskEmail(%q) = %q, want %q", in, got, want)
		}
	}
}
