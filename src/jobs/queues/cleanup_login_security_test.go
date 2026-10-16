package queues

import (
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/pgtest"
)

func TestCleanupLoginSecurity(t *testing.T) {
	db := pgtest.MustDB()
	t.Cleanup(func() { _ = db.Close() })
	ctx := t.Context()
	now := time.Now().UTC()

	if _, err := db.NewInsert().Model(&models.Account{ID: "acc", Email: "a@x.io", PasswordHash: "x", Name: "A"}).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	for _, d := range []models.KnownDevice{
		{ID: "d-live", AccountID: "acc", DeviceHash: "h1", FirstSeenAt: now, LastSeenAt: now.Add(-399 * 24 * time.Hour)},
		{ID: "d-gone", AccountID: "acc", DeviceHash: "h2", FirstSeenAt: now, LastSeenAt: now.Add(-401 * 24 * time.Hour)},
	} {
		if _, err := db.NewInsert().Model(&d).Exec(ctx); err != nil {
			t.Fatal(err)
		}
	}
	for _, tok := range []models.LoginAlertToken{
		{ID: "t-live", AccountID: "acc", UserID: "u", TenantID: "t", TokenHash: "a", LoginAt: now, ExpiresAt: now, CreatedAt: now.Add(-29 * 24 * time.Hour)},
		{ID: "t-gone", AccountID: "acc", UserID: "u", TenantID: "t", TokenHash: "b", LoginAt: now, ExpiresAt: now, CreatedAt: now.Add(-31 * 24 * time.Hour)},
	} {
		if _, err := db.NewInsert().Model(&tok).Exec(ctx); err != nil {
			t.Fatal(err)
		}
	}

	for _, s := range []models.OAuthState{
		{StateHash: "s-live", Provider: "google", Intent: models.OAuthIntentSignin, Nonce: "n", PKCEVerifier: "v", ReturnTo: "/", ExpiresAt: now.Add(time.Minute)},
		{StateHash: "s-gone", Provider: "google", Intent: models.OAuthIntentSignin, Nonce: "n", PKCEVerifier: "v", ReturnTo: "/", ExpiresAt: now.Add(-time.Minute)},
	} {
		if _, err := db.NewInsert().Model(&s).Exec(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.NewInsert().Model(&models.OAuthPendingSignup{TokenHash: "p-gone", Provider: "google", Subject: "sub", Email: "a@x.io", ReturnTo: "/", ExpiresAt: now.Add(-time.Minute)}).Exec(ctx); err != nil {
		t.Fatal(err)
	}

	p := &CleanupLoginSecurityProcessor{
		Devices: repository.NewKnownDeviceRepository(db),
		Alerts:  repository.NewLoginAlertTokenRepository(db),
		OAuth:   repository.NewOAuthRepository(db),
	}
	if err := p.Process(ctx, CleanupLoginSecurityTask{}); err != nil {
		t.Fatalf("process: %v", err)
	}

	var states []string
	if err := db.NewSelect().Model((*models.OAuthState)(nil)).Column("state_hash").Scan(ctx, &states); err != nil {
		t.Fatal(err)
	}
	pending, err := db.NewSelect().Model((*models.OAuthPendingSignup)(nil)).Count(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != 1 || states[0] != "s-live" || pending != 0 {
		t.Fatalf("after sweep: states %v, pending %d; want only the live state", states, pending)
	}

	var devices, tokens []string
	if err := db.NewSelect().Model((*models.KnownDevice)(nil)).Column("id").Scan(ctx, &devices); err != nil {
		t.Fatal(err)
	}
	if err := db.NewSelect().Model((*models.LoginAlertToken)(nil)).Column("id").Scan(ctx, &tokens); err != nil {
		t.Fatal(err)
	}
	if len(devices) != 1 || devices[0] != "d-live" || len(tokens) != 1 || tokens[0] != "t-live" {
		t.Fatalf("after sweep: devices %v, tokens %v; want only the live rows", devices, tokens)
	}

	if err := (&CleanupLoginSecurityProcessor{}).Process(ctx, CleanupLoginSecurityTask{}); err != nil {
		t.Fatalf("unwired sweep must be a no-op: %v", err)
	}
}
