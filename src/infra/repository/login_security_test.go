package repository_test

import (
	"database/sql"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
)

func TestKnownDeviceRepository(t *testing.T) {
	db := openMigratedDB(t)
	repo := repository.NewKnownDeviceRepository(db)
	ctx := t.Context()
	now := time.Now().UTC().Truncate(time.Microsecond)

	if ok, err := repo.Touch(ctx, "acc", "h1", "203.0.113.1", "ua", "Chrome on macOS", now); err != nil || ok {
		t.Fatalf("touch of an unknown device = %v, %v; want false", ok, err)
	}

	d := &models.KnownDevice{ID: "d1", AccountID: "acc", DeviceHash: "h1", LastIP: "203.0.113.1", FirstSeenAt: now, LastSeenAt: now}
	if err := repo.Upsert(ctx, nil, d); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	// A second insert of the same (account, hash) under a new id refreshes the
	// existing row and reports its id.
	dup := &models.KnownDevice{ID: "d2", AccountID: "acc", DeviceHash: "h1", LastIP: "198.51.100.7", FirstSeenAt: now, LastSeenAt: now}
	if err := repo.Upsert(ctx, nil, dup); err != nil {
		t.Fatalf("upsert duplicate: %v", err)
	}
	if dup.ID != "d1" {
		t.Fatalf("duplicate upsert id = %q, want d1", dup.ID)
	}
	if err := repo.Upsert(ctx, nil, &models.KnownDevice{ID: "d3", AccountID: "other", DeviceHash: "h1", FirstSeenAt: now, LastSeenAt: now}); err != nil {
		t.Fatalf("same hash, other account: %v", err)
	}

	if n, err := repo.CountForAccount(ctx, nil, "acc"); err != nil || n != 1 {
		t.Fatalf("count = %d, %v; want 1", n, err)
	}
	if ok, err := repo.Touch(ctx, "acc", "h1", "203.0.113.1", "ua", "Chrome on macOS", now.Add(time.Minute)); err != nil || !ok {
		t.Fatalf("touch of a known device = %v, %v; want true", ok, err)
	}

	if n, err := repo.DeleteUnseenSince(ctx, now.Add(30*time.Second)); err != nil || n != 1 {
		t.Fatalf("stale sweep deleted %d, %v; want only the untouched device", n, err)
	}
	if n, err := repo.DeleteForAccount(ctx, nil, "acc"); err != nil || n != 1 {
		t.Fatalf("delete for account = %d, %v; want 1", n, err)
	}
}

func TestLoginAlertTokenRepository(t *testing.T) {
	db := openMigratedDB(t)
	repo := repository.NewLoginAlertTokenRepository(db)
	ctx := t.Context()
	now := time.Now().UTC()

	seed := func(id, hash string, created time.Time) {
		t.Helper()
		tok := &models.LoginAlertToken{
			ID: id, AccountID: "acc", UserID: "u", TenantID: "t", TokenHash: hash,
			LoginAt: created, ExpiresAt: created.Add(models.LoginAlertTokenTTL), CreatedAt: created,
		}
		if err := repo.Create(ctx, nil, tok); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}
	seed("old", "h-old", now.Add(-48*time.Hour))
	seed("a", "h-a", now.Add(-10*time.Minute))
	seed("b", "h-b", now.Add(-5*time.Minute))

	if n, err := repo.CountSince(ctx, nil, "acc", now.Add(-time.Hour)); err != nil || n != 2 {
		t.Fatalf("count since = %d, %v; want 2", n, err)
	}

	if _, err := repo.Consume(ctx, nil, "h-old", now); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("expired token must not be consumable, got %v", err)
	}

	// Two concurrent consumes of one token: exactly one wins.
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for range 2 {
		wg.Go(func() {
			_, err := repo.Consume(ctx, nil, "h-a", now)
			results <- err
		})
	}
	wg.Wait()
	close(results)
	var won, lost int
	for err := range results {
		switch {
		case err == nil:
			won++
		case errors.Is(err, sql.ErrNoRows):
			lost++
		default:
			t.Fatalf("consume: %v", err)
		}
	}
	if won != 1 || lost != 1 {
		t.Fatalf("concurrent consume: %d won, %d lost; want 1 and 1", won, lost)
	}

	got, err := repo.GetByHash(ctx, "h-a")
	if err != nil || got.Status(now) != models.LoginAlertUsed {
		t.Fatalf("consumed token status = %v, %v; want used", got, err)
	}

	if n, err := repo.VoidPending(ctx, nil, "acc", now); err != nil || n != 2 {
		t.Fatalf("void pending = %d, %v; want b and the expired one", n, err)
	}
	if n, err := repo.DeleteCreatedBefore(ctx, now.Add(-24*time.Hour)); err != nil || n != 1 {
		t.Fatalf("retention sweep = %d, %v; want 1", n, err)
	}
}
