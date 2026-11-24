package server

import (
	"testing"

	"github.com/ogen-app/ogen/src/pgtest"
)

// TestAdvisoryLockExcludesHolders takes the lock, checks a second taker is
// refused while it is held, and that it can be taken again once released.
func TestAdvisoryLockExcludesHolders(t *testing.T) {
	ctx := t.Context()
	db := pgtest.MustDB()
	t.Cleanup(func() { _ = db.Close() })
	lock := advisoryLock(db, zernioSyncLockKey)

	release, ok := lock(ctx)
	if !ok {
		t.Fatal("first lock: not acquired")
	}
	if again, ok := lock(ctx); ok {
		again()
		t.Fatal("second lock acquired while the first is held")
	}
	release()

	release, ok = lock(ctx)
	if !ok {
		t.Fatal("lock after release: not acquired")
	}
	release()
}
