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
