package repository_test

import (
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
)

func TestAccountIdentityRepositoryLifecycle(t *testing.T) {
	db := openMigratedDB(t)
	repo := repository.NewAccountIdentityRepository(db)
	ctx := t.Context()

	id := &models.AccountIdentity{ID: "ai-1", AccountID: "acc", Provider: models.IdentityProviderGoogle, Subject: "sub-1", Email: "a@gmail.com"}
	if err := repo.Create(ctx, nil, id); err != nil {
		t.Fatalf("create: %v", err)
	}
	dup := &models.AccountIdentity{ID: "ai-2", AccountID: "other", Provider: models.IdentityProviderGoogle, Subject: "sub-1", Email: "b@gmail.com"}
	if err := repo.Create(ctx, nil, dup); err == nil {
		t.Fatal("a subject must link to one account only")
	}

	got, err := repo.GetBySubject(ctx, models.IdentityProviderGoogle, "sub-1")
	if err != nil || got.AccountID != "acc" {
		t.Fatalf("get by subject: %+v err=%v", got, err)
	}
	if _, err := repo.GetBySubject(ctx, models.IdentityProviderGoogle, "nope"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("unknown subject: err=%v, want sql.ErrNoRows", err)
	}

	at := time.Now().UTC().Truncate(time.Microsecond)
	if err := repo.MarkUsed(ctx, "ai-1", "renamed@gmail.com", at); err != nil {
		t.Fatalf("mark used: %v", err)
	}
	list, err := repo.ListForAccount(ctx, "acc")
	if err != nil || len(list) != 1 || list[0].Email != "renamed@gmail.com" || list[0].LastUsedAt == nil || !list[0].LastUsedAt.Equal(at) {
		t.Fatalf("list after use: %+v err=%v", list, err)
	}

	removed, err := repo.DeleteForAccount(ctx, nil, "acc", models.IdentityProviderGoogle)
	if err != nil || !removed {
		t.Fatalf("delete: removed=%v err=%v", removed, err)
	}
	removed, err = repo.DeleteForAccount(ctx, nil, "acc", models.IdentityProviderGoogle)
	if err != nil || removed {
		t.Fatalf("second delete: removed=%v err=%v, want nothing removed", removed, err)
	}
}

func TestSessionRepositoryCreateForIdentity(t *testing.T) {
	db := openMigratedDB(t)
	sessions := repository.NewSessionRepository(db)
	identities := repository.NewAccountIdentityRepository(db)
	ctx := t.Context()
	exp := time.Now().UTC().Add(time.Hour)

	if err := identities.Create(ctx, nil, &models.AccountIdentity{ID: "ai", AccountID: "acc", Provider: models.IdentityProviderGoogle, Subject: "s", Email: "a@x.io"}); err != nil {
		t.Fatal(err)
	}
	ok, err := sessions.CreateForIdentity(ctx, &models.Session{ID: "s1", AccountID: "acc", UserID: "u", TenantID: "t", ExpiresAt: exp}, "ai")
	if err != nil || !ok {
		t.Fatalf("linked identity: created=%v err=%v", ok, err)
	}

	// The identity belongs to another account: no session.
	ok, err = sessions.CreateForIdentity(ctx, &models.Session{ID: "s2", AccountID: "intruder", UserID: "u", TenantID: "t", ExpiresAt: exp}, "ai")
	if err != nil || ok {
		t.Fatalf("foreign identity: created=%v err=%v", ok, err)
	}

	// Disconnected between verification and session creation: no session.
	if _, err := identities.DeleteForAccount(ctx, nil, "acc", models.IdentityProviderGoogle); err != nil {
		t.Fatal(err)
	}
	ok, err = sessions.CreateForIdentity(ctx, &models.Session{ID: "s3", AccountID: "acc", UserID: "u", TenantID: "t", ExpiresAt: exp}, "ai")
	if err != nil || ok {
		t.Fatalf("unlinked identity: created=%v err=%v", ok, err)
	}
	if _, err := sessions.GetByID(ctx, "s3"); err == nil {
		t.Fatal("an unlinked identity must not leave a session")
	}
}

func TestAccountRepositoryPasswordlessAccounts(t *testing.T) {
	db := openMigratedDB(t)
	accounts := repository.NewAccountRepository(db)
	sessions := repository.NewSessionRepository(db)
	ctx := t.Context()

	if err := accounts.Create(ctx, &models.Account{ID: "acc", Email: "Jane@Acme.com", PasswordHash: "hash-1", Name: "Jane"}); err != nil {
		t.Fatal(err)
	}
	got, err := accounts.GetByEmailFold(ctx, "jane@acme.COM")
	if err != nil || got.ID != "acc" {
		t.Fatalf("case-insensitive lookup: %+v err=%v", got, err)
	}
	if _, err := accounts.GetByEmail(ctx, "jane@acme.com"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("exact lookup stays case-sensitive: err=%v", err)
	}

	if err := accounts.ClearPasswordTx(ctx, nil, "acc"); err != nil {
		t.Fatalf("clear password: %v", err)
	}
	got, err = accounts.GetByID(ctx, "acc")
	if err != nil || got.HasPassword() {
		t.Fatalf("after clear: has password=%v err=%v", got.HasPassword(), err)
	}
	var isNull bool
	if err := db.NewSelect().Table("accounts").ColumnExpr("password_hash IS NULL").Where("id = ?", "acc").Scan(ctx, &isNull); err != nil || !isNull {
		t.Fatalf("password_hash must be stored as NULL: null=%v err=%v", isNull, err)
	}

	// A passwordless account offers no hash to match, even an empty one.
	ok, err := sessions.CreateForCredential(ctx, &models.Session{ID: "s", AccountID: "acc", UserID: "u", TenantID: "t", ExpiresAt: time.Now().Add(time.Hour)}, "hash-1")
	if err != nil || ok {
		t.Fatalf("stale credential on passwordless account: created=%v err=%v", ok, err)
	}

	// A passwordless account is inserted with NULL, not "".
	if err := accounts.Create(ctx, &models.Account{ID: "acc-2", Email: "g@x.io", Name: "G"}); err != nil {
		t.Fatalf("create passwordless: %v", err)
	}
	if err := db.NewSelect().Table("accounts").ColumnExpr("password_hash IS NULL").Where("id = ?", "acc-2").Scan(ctx, &isNull); err != nil || !isNull {
		t.Fatalf("new passwordless account must store NULL: null=%v err=%v", isNull, err)
	}
}

func TestOAuthRepositorySingleUse(t *testing.T) {
	db := openMigratedDB(t)
	repo := repository.NewOAuthRepository(db)
	ctx := t.Context()
	now := time.Now().UTC()

	if err := repo.CreateState(ctx, &models.OAuthState{StateHash: "h", Provider: "google", Intent: models.OAuthIntentLink, Nonce: "n", PKCEVerifier: "v", ReturnTo: "/settings", AccountID: "acc", ExpiresAt: now.Add(models.OAuthStateTTL)}); err != nil {
		t.Fatal(err)
	}
	st, err := repo.ConsumeState(ctx, "h", now)
	if err != nil || st.AccountID != "acc" || st.ReturnTo != "/settings" || st.InviteHash != "" {
		t.Fatalf("consume: %+v err=%v", st, err)
	}
	if _, err := repo.ConsumeState(ctx, "h", now); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("replayed state: err=%v, want sql.ErrNoRows", err)
	}
	if err := repo.CreateState(ctx, &models.OAuthState{StateHash: "old", Provider: "google", Intent: models.OAuthIntentSignin, Nonce: "n", PKCEVerifier: "v", ReturnTo: "/", ExpiresAt: now.Add(-time.Second)}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.ConsumeState(ctx, "old", now); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("expired state: err=%v, want sql.ErrNoRows", err)
	}

	if err := repo.CreatePendingSignup(ctx, &models.OAuthPendingSignup{TokenHash: "p", Provider: "google", Subject: "sub", Email: "a@x.io", Name: "A", ReturnTo: "/", ExpiresAt: now.Add(models.OAuthPendingSignupTTL)}); err != nil {
		t.Fatal(err)
	}
	if p, err := repo.GetPendingSignup(ctx, "p", now); err != nil || p.Subject != "sub" {
		t.Fatalf("preview: %+v err=%v", p, err)
	}
	if _, err := repo.ConsumePendingSignup(ctx, nil, "p", now); err != nil {
		t.Fatalf("consume pending: %v", err)
	}
	if _, err := repo.ConsumePendingSignup(ctx, nil, "p", now); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("double submit: err=%v, want sql.ErrNoRows", err)
	}
	if _, err := repo.GetPendingSignup(ctx, "p", now); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("preview after use: err=%v, want sql.ErrNoRows", err)
	}
}
