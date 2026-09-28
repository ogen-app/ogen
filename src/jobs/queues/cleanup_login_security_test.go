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

	p := &CleanupLoginSecurityProcessor{
		Devices: repository.NewKnownDeviceRepository(db),
		Alerts:  repository.NewLoginAlertTokenRepository(db),
	}
	if err := p.Process(ctx, CleanupLoginSecurityTask{}); err != nil {
		t.Fatalf("process: %v", err)
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

func TestPeriodicJobsLoginSecurityCleanupNeedsInterval(t *testing.T) {
	base := PeriodicConfig{CleanupEvery: time.Hour, ReconcileEvery: 5 * time.Minute}
	if got := len(base.PeriodicJobs()); got != 2 {
		t.Fatalf("zero interval must not register the sweep: got %d jobs", got)
	}
	base.LoginSecurityCleanupEvery = 24 * time.Hour
	if got := len(base.PeriodicJobs()); got != 3 {
		t.Fatalf("positive interval must register the sweep: got %d jobs", got)
	}
}
