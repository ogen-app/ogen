package repository_test

import (
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
)

func seedPluginPairing(t *testing.T, repo repository.PluginPairingRepository, id string, expires time.Time) (models.PluginPairingKeys, *models.PluginPairing) {
	t.Helper()
	keys, err := models.NewPluginPairingKeys()
	if err != nil {
		t.Fatal(err)
	}
	p := &models.PluginPairing{
		ID: id, Client: models.PluginClientFigma, ClientLabel: "Figma · Jane",
		ReadKeyHash: keys.ReadKeyHash, WriteKeyHash: keys.WriteKeyHash,
		Status: models.PluginPairingPending, CreatedIP: "203.0.113.7", ExpiresAt: expires,
	}
	if err := repo.Create(t.Context(), p); err != nil {
		t.Fatalf("seed pairing %s: %v", id, err)
	}
	return keys, p
}

func seedPluginToken(t *testing.T, db bun.IDB, repo repository.PluginTokenRepository, id, tenantID, userID string) {
	t.Helper()
	_, hash, err := models.NewPluginToken()
	if err != nil {
		t.Fatal(err)
	}
	tok := &models.PluginToken{
		ID: id, TenantID: tenantID, AccountID: "acc-" + userID, UserID: userID,
		Client: models.PluginClientFigma, Label: "Figma · " + id, TokenHash: hash,
	}
	if err := repo.Create(t.Context(), db, tok); err != nil {
		t.Fatalf("seed token %s: %v", id, err)
	}
}

func TestPluginPairingLifecycle(t *testing.T) {
	db := openMigratedDB(t)
	repo := repository.NewPluginPairingRepository(db)
	ctx := t.Context()
	now := time.Now().UTC()

	keys, p := seedPluginPairing(t, repo, "pair-1", now.Add(models.PluginPairingTTL))

	got, err := repo.GetLiveByWriteHash(ctx, keys.WriteKeyHash, now)
	if err != nil || got.ID != "pair-1" || got.Status != models.PluginPairingPending {
		t.Fatalf("by write hash: %+v err=%v", got, err)
	}
	if _, err := repo.GetLiveByReadHash(ctx, keys.ReadKeyHash, now); err != nil {
		t.Fatalf("by read hash: %v", err)
	}
	// Pending pairings aren't collectable.
	if _, err := repo.Collect(ctx, keys.ReadKeyHash, now); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("collect pending: err=%v, want ErrNoRows", err)
	}

	tenant, token := models.DefaultTenantID, "tok-1"
	p.TenantID, p.TokenID, p.SealedToken = &tenant, &token, []byte("sealed")
	if ok, err := repo.Approve(ctx, nil, p, now); err != nil || !ok {
		t.Fatalf("approve: ok=%v err=%v", ok, err)
	}
	if ok, err := repo.Approve(ctx, nil, p, now); err != nil || ok {
		t.Fatalf("second approve must not match: ok=%v err=%v", ok, err)
	}
	if ok, err := repo.Deny(ctx, p.ID, now); err != nil || ok {
		t.Fatalf("deny after approve must not match: ok=%v err=%v", ok, err)
	}

	collected, err := repo.Collect(ctx, keys.ReadKeyHash, now)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if string(collected.SealedToken) != "sealed" || collected.TokenID == nil || *collected.TokenID != "tok-1" {
		t.Fatalf("collected %+v", collected)
	}
	if _, err := repo.Collect(ctx, keys.ReadKeyHash, now); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("second collect: err=%v, want ErrNoRows", err)
	}
	if _, err := repo.GetLiveByReadHash(ctx, keys.ReadKeyHash, now); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("collected pairing must be gone: err=%v", err)
	}
}

func TestPluginPairingDenyAndExpiry(t *testing.T) {
	db := openMigratedDB(t)
	repo := repository.NewPluginPairingRepository(db)
	ctx := t.Context()
	now := time.Now().UTC()

	_, denied := seedPluginPairing(t, repo, "pair-deny", now.Add(time.Minute))
	if ok, err := repo.Deny(ctx, denied.ID, now); err != nil || !ok {
		t.Fatalf("deny: ok=%v err=%v", ok, err)
	}
	tenant, token := models.DefaultTenantID, "tok-x"
	denied.TenantID, denied.TokenID = &tenant, &token
	if ok, err := repo.Approve(ctx, nil, denied, now); err != nil || ok {
		t.Fatalf("approve after deny must not match: ok=%v err=%v", ok, err)
	}

	keys, expired := seedPluginPairing(t, repo, "pair-old", now.Add(-time.Second))
	if _, err := repo.GetLiveByWriteHash(ctx, keys.WriteKeyHash, now); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("expired pairing must read as gone: err=%v", err)
	}
	expired.TenantID, expired.TokenID = &tenant, &token
	if ok, err := repo.Approve(ctx, nil, expired, now); err != nil || ok {
		t.Fatalf("approve after expiry must not match: ok=%v err=%v", ok, err)
	}

	n, err := repo.DeleteExpired(ctx, now)
	if err != nil || n != 1 {
		t.Fatalf("sweep: n=%d err=%v, want 1", n, err)
	}
	var left int
	if left, err = db.NewSelect().Model((*models.PluginPairing)(nil)).Count(ctx); err != nil || left != 1 {
		t.Fatalf("rows left = %d err=%v, want only the live denied one", left, err)
	}
}

func TestPluginTokenRepository(t *testing.T) {
	db := openMigratedDB(t)
	repo := repository.NewPluginTokenRepository(db)
	ctx := t.Context()
	now := time.Now().UTC()

	for _, u := range []models.User{
		{ID: "u-jane", AccountID: "acc-u-jane", TenantID: "t-1", Name: "Jane", Email: "jane@x.io", Role: models.RoleMember},
		{ID: "u-bob", AccountID: "acc-u-bob", TenantID: "t-1", Name: "Bob", Email: "bob@x.io", Role: models.RoleOwner},
		{ID: "u-eve", AccountID: "acc-u-eve", TenantID: "t-2", Name: "Eve", Email: "eve@x.io", Role: models.RoleOwner},
	} {
		if _, err := db.NewInsert().Model(&u).Exec(ctx); err != nil {
			t.Fatalf("seed user %s: %v", u.ID, err)
		}
	}

	token, hash, err := models.NewPluginToken()
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.Create(ctx, nil, &models.PluginToken{
		ID: "tok-jane", TenantID: "t-1", AccountID: "acc-u-jane", UserID: "u-jane",
		Client: models.PluginClientFigma, Label: "Figma · Jane", TokenHash: hash,
	}); err != nil {
		t.Fatal(err)
	}
	seedPluginToken(t, db, repo, "tok-bob", "t-1", "u-bob")
	seedPluginToken(t, db, repo, "tok-eve", "t-2", "u-eve")

	got, err := repo.GetActiveByHash(ctx, models.HashPluginSecret(token))
	if err != nil || got.ID != "tok-jane" || got.TenantID != "t-1" {
		t.Fatalf("by hash: %+v err=%v", got, err)
	}

	all, err := repo.ListActive(ctx, "t-1", "")
	if err != nil || len(all) != 2 {
		t.Fatalf("list tenant: %d err=%v, want 2", len(all), err)
	}
	mine, err := repo.ListActive(ctx, "t-1", "u-jane")
	if err != nil || len(mine) != 1 || mine[0].UserName != "Jane" || mine[0].UserEmail != "jane@x.io" {
		t.Fatalf("list member: %+v err=%v", mine, err)
	}

	if _, err := repo.GetActive(ctx, "t-2", "tok-jane"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("another tenant's token must not resolve: err=%v", err)
	}
	if ok, err := repo.Revoke(ctx, "t-2", "tok-jane", now); err != nil || ok {
		t.Fatalf("cross-tenant revoke must not match: ok=%v err=%v", ok, err)
	}
	if ok, err := repo.Revoke(ctx, "t-1", "tok-jane", now); err != nil || !ok {
		t.Fatalf("revoke: ok=%v err=%v", ok, err)
	}
	if ok, err := repo.Revoke(ctx, "t-1", "tok-jane", now); err != nil || ok {
		t.Fatalf("second revoke must not match: ok=%v err=%v", ok, err)
	}
	if _, err := repo.GetActiveByHash(ctx, models.HashPluginSecret(token)); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("revoked token must not resolve: err=%v", err)
	}
	if all, _ = repo.ListActive(ctx, "t-1", ""); len(all) != 1 || all[0].ID != "tok-bob" {
		t.Fatalf("list after revoke: %+v", all)
	}
}

func TestPluginTokenTouchLastUsedIsThrottled(t *testing.T) {
	db := openMigratedDB(t)
	repo := repository.NewPluginTokenRepository(db)
	ctx := t.Context()
	seedPluginToken(t, db, repo, "tok-1", "t-1", "u-1")

	lastUsed := func() time.Time {
		t.Helper()
		tok, err := repo.GetActive(ctx, "t-1", "tok-1")
		if err != nil || tok.LastUsedAt == nil {
			t.Fatalf("read last_used_at: %+v err=%v", tok, err)
		}
		return tok.LastUsedAt.UTC()
	}

	t0 := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	if err := repo.TouchLastUsed(ctx, "tok-1", t0); err != nil {
		t.Fatal(err)
	}
	if got := lastUsed(); !got.Equal(t0) {
		t.Fatalf("first touch = %v, want %v", got, t0)
	}
	if err := repo.TouchLastUsed(ctx, "tok-1", t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if got := lastUsed(); !got.Equal(t0) {
		t.Fatalf("touch within the interval moved last_used_at to %v", got)
	}
	later := t0.Add(repository.PluginTokenTouchInterval)
	if err := repo.TouchLastUsed(ctx, "tok-1", later); err != nil {
		t.Fatal(err)
	}
	if got := lastUsed(); !got.Equal(later) {
		t.Fatalf("touch after the interval = %v, want %v", got, later)
	}
}
