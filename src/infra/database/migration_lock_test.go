package database_test

import (
	"context"
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/infra/database"
	"github.com/ogen-app/ogen/src/pgtest"
)

// TestWithMigrationLockSerializesCallers holds the lock in one caller and
// checks that a second caller only enters once the first has returned.
func TestWithMigrationLockSerializesCallers(t *testing.T) {
	ctx := t.Context()
	db := pgtest.MustDB()

	entered := make(chan struct{})
	release := make(chan struct{})
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- database.WithMigrationLock(ctx, db.DB, func(context.Context) error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered

	secondEntered := make(chan struct{})
	secondDone := make(chan error, 1)
	go func() {
		secondDone <- database.WithMigrationLock(ctx, db.DB, func(context.Context) error {
			close(secondEntered)
			return nil
		})
	}()

	select {
	case <-secondEntered:
		t.Fatal("second caller entered while the first held the lock")
	case <-time.After(300 * time.Millisecond):
	}

	close(release)
	if err := <-firstDone; err != nil {
		t.Fatalf("first caller: %v", err)
	}
	select {
	case <-secondEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("second caller never entered after the lock was released")
	}
	if err := <-secondDone; err != nil {
		t.Fatalf("second caller: %v", err)
	}
}

// TestWithMigrationLockSingleConnectionPool checks that a pool capped at one
// connection still migrates: the lock would hold its only connection.
func TestWithMigrationLockSingleConnectionPool(t *testing.T) {
	ctx := t.Context()
	db := pgtest.MustDB()
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)

	done := make(chan error, 1)
	go func() {
		done <- database.WithMigrationLock(ctx, db.DB, func(ctx context.Context) error {
			_, err := db.ExecContext(ctx, "SELECT 1")
			return err
		})
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("WithMigrationLock: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("WithMigrationLock deadlocked on a single-connection pool")
	}
}

// TestWithMigrationLockGivesUpWithContext checks that a caller waiting for the
// lock returns once its context ends instead of blocking boot forever.
func TestWithMigrationLockGivesUpWithContext(t *testing.T) {
	ctx := t.Context()
	db := pgtest.MustDB()

	entered := make(chan struct{})
	release := make(chan struct{})
	held := make(chan error, 1)
	go func() {
		held <- database.WithMigrationLock(ctx, db.DB, func(context.Context) error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered
	defer func() {
		close(release)
		if err := <-held; err != nil {
			t.Errorf("holder: %v", err)
		}
	}()

	waitCtx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	err := database.WithMigrationLock(waitCtx, db.DB, func(context.Context) error {
		t.Error("waiter entered while the lock was held")
		return nil
	})
	if err == nil {
		t.Fatal("WithMigrationLock returned nil while the lock was held, want a context error")
	}
}
