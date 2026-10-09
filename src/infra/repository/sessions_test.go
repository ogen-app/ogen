package repository_test

import (
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
)

// TestSessionRepositoryGetForAuth checks the one-query auth lookup resolves the
// same membership GetMembership would: the session default when no workspace
// is named, the named workspace when the account belongs to it, and none for a
// workspace the account isn't in or that isn't active.
func TestSessionRepositoryGetForAuth(t *testing.T) {
	db := openMigratedDB(t)
	repo := repository.NewSessionRepository(db)
	ctx := t.Context()

	ts := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for id, status := range map[string]string{
		"t-home": models.TenantStatusActive, "t-other": models.TenantStatusActive,
		"t-suspended": models.TenantStatusSuspended, "t-foreign": models.TenantStatusActive,
	} {
		tn := &models.Tenant{ID: id, Name: id, Slug: id, TierID: models.DefaultTierID, Status: status, CreatedAt: ts, UpdatedAt: ts}
		if _, err := db.NewInsert().Model(tn).Exec(ctx); err != nil {
			t.Fatalf("seed tenant %s: %v", id, err)
		}
	}
	for _, m := range []*models.User{
		{ID: "u-home", AccountID: "acc", TenantID: "t-home", Name: "H", Email: "e@x", CreatedAt: ts, UpdatedAt: ts},
		{ID: "u-other", AccountID: "acc", TenantID: "t-other", Name: "O", Email: "e@x", CreatedAt: ts, UpdatedAt: ts},
		{ID: "u-sus", AccountID: "acc", TenantID: "t-suspended", Name: "S", Email: "e@x", CreatedAt: ts, UpdatedAt: ts},
		{ID: "u-foreign", AccountID: "acc-2", TenantID: "t-foreign", Name: "F", Email: "f@x", CreatedAt: ts, UpdatedAt: ts},
	} {
		if _, err := db.NewInsert().Model(m).Exec(ctx); err != nil {
			t.Fatalf("seed membership %s: %v", m.ID, err)
		}
	}
	s := &models.Session{ID: "s1", AccountID: "acc", UserID: "u-home", TenantID: "t-home", ExpiresAt: ts.Add(time.Hour)}
	if err := repo.Create(ctx, s); err != nil {
		t.Fatalf("seed session: %v", err)
	}

	cases := []struct{ workspace, wantUser string }{
		{"", "u-home"},
		{"t-other", "u-other"},
		{"t-suspended", ""},
		{"t-foreign", ""},
	}
	for _, c := range cases {
		session, m, err := repo.GetForAuth(ctx, "s1", c.workspace)
		if err != nil {
			t.Fatalf("workspace %q: %v", c.workspace, err)
		}
		if session.ID != "s1" || session.AccountID != "acc" || session.TenantID != "t-home" {
			t.Fatalf("workspace %q: session = %+v", c.workspace, session)
		}
		switch {
		case c.wantUser == "" && m != nil:
			t.Errorf("workspace %q: membership = %+v, want none", c.workspace, m)
		case c.wantUser != "" && (m == nil || m.UserID != c.wantUser):
			t.Errorf("workspace %q: membership = %+v, want %s", c.workspace, m, c.wantUser)
		}
	}

	if _, _, err := repo.GetForAuth(ctx, "missing", ""); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("unknown session: err = %v, want sql.ErrNoRows", err)
	}
}

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
