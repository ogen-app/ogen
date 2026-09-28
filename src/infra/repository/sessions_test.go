package repository_test

import (
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
)

func TestSessionRepositoryDeleteAllForAccount(t *testing.T) {
	db := openMigratedDB(t)
	repo := repository.NewSessionRepository(db)
	ctx := t.Context()

	exp := time.Now().UTC().Add(time.Hour)
	for _, s := range []models.Session{
		{ID: "s1", AccountID: "acc-a", UserID: "u1", TenantID: "t1", ExpiresAt: exp},
		{ID: "s2", AccountID: "acc-a", UserID: "u2", TenantID: "t2", ExpiresAt: exp},
		{ID: "s3", AccountID: "acc-a", UserID: "u1", TenantID: "t1", ExpiresAt: exp},
		{ID: "s4", AccountID: "acc-b", UserID: "u3", TenantID: "t1", ExpiresAt: exp},
	} {
		if err := repo.Create(ctx, &s); err != nil {
			t.Fatalf("seed %s: %v", s.ID, err)
		}
	}

	n, err := repo.DeleteAllForAccount(ctx, nil, "acc-a", "s1")
	if err != nil || n != 2 {
		t.Fatalf("keep s1: deleted %d err=%v, want 2", n, err)
	}
	if _, err := repo.GetByID(ctx, "s1"); err != nil {
		t.Fatalf("excepted session must survive: %v", err)
	}

	n, err = repo.DeleteAllForAccount(ctx, db, "acc-a", "")
	if err != nil || n != 1 {
		t.Fatalf("delete all: deleted %d err=%v, want 1", n, err)
	}
	if _, err := repo.GetByID(ctx, "s4"); err != nil {
		t.Fatalf("other account's session must survive: %v", err)
	}
}

func TestSessionRepositoryCreateForCredential(t *testing.T) {
	db := openMigratedDB(t)
	repo := repository.NewSessionRepository(db)
	ctx := t.Context()

	if _, err := db.NewInsert().Model(&models.Account{ID: "acc", Email: "a@x.io", PasswordHash: "hash-1", Name: "A"}).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	exp := time.Now().UTC().Add(time.Hour)

	ok, err := repo.CreateForCredential(ctx, &models.Session{ID: "s1", AccountID: "acc", UserID: "u", TenantID: "t", ExpiresAt: exp}, "hash-1")
	if err != nil || !ok {
		t.Fatalf("unchanged credential: created=%v err=%v", ok, err)
	}

	// The password changed after it was verified: no session.
	ok, err = repo.CreateForCredential(ctx, &models.Session{ID: "s2", AccountID: "acc", UserID: "u", TenantID: "t", ExpiresAt: exp}, "hash-0")
	if err != nil || ok {
		t.Fatalf("stale credential: created=%v err=%v", ok, err)
	}
	if _, err := repo.GetByID(ctx, "s2"); err == nil {
		t.Fatal("a stale credential must not leave a session")
	}

	ok, err = repo.CreateForCredential(ctx, &models.Session{ID: "s3", AccountID: "gone", UserID: "u", TenantID: "t", ExpiresAt: exp}, "hash-1")
	if err != nil || ok {
		t.Fatalf("missing account: created=%v err=%v", ok, err)
	}
}
