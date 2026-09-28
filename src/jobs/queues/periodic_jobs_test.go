package queues

import (
	"reflect"
	"testing"
	"time"
	"unsafe"

	"github.com/riverqueue/river"
)

// periodicRunOnStart reads the unexported river.PeriodicJob.opts so the test can
// assert the RunOnStart fix directly — River exposes no accessor for it. The
// reflect+unsafe dance clears the read-only flag so Interface() is allowed; the
// field name is stable for the pinned River version (see go.mod).
func periodicRunOnStart(t *testing.T, pj *river.PeriodicJob) bool {
	t.Helper()
	f := reflect.ValueOf(pj).Elem().FieldByName("opts")
	if !f.IsValid() {
		t.Fatal("river.PeriodicJob has no opts field — River internals changed")
	}
	f = reflect.NewAt(f.Type(), unsafe.Pointer(f.UnsafeAddr())).Elem()
	opts, _ := reflect.TypeAssert[*river.PeriodicJobOpts](f)
	return opts != nil && opts.RunOnStart
}

// TestPeriodicJobsRunOnStart pins the fix: every periodic sweep must run once on
// boot, so a long-interval job (analytics 30m, cleanup 1h) can't be starved by a
// frequently-restarting process that never survives a full interval.
func TestPeriodicJobsRunOnStart(t *testing.T) {
	cfg := PeriodicConfig{
		CleanupEvery:     time.Hour,
		ReconcileEvery:   5 * time.Minute,
		AnalyticsEvery:   30 * time.Minute,
		IncludeAnalytics: true,
	}
	jobs := cfg.PeriodicJobs()
	if len(jobs) != 3 {
		t.Fatalf("periodic jobs: got %d, want 3 (cleanup, reconcile, analytics)", len(jobs))
	}
	for i, pj := range jobs {
		if !periodicRunOnStart(t, pj) {
			t.Errorf("periodic job %d: RunOnStart = false, want true", i)
		}
	}
}

// TestPeriodicJobsAnalyticsGated keeps the analytics sweep behind IncludeAnalytics
// so a Zernio-less deployment schedules only cleanup + reconcile.
func TestPeriodicJobsAnalyticsGated(t *testing.T) {
	cfg := PeriodicConfig{CleanupEvery: time.Hour, ReconcileEvery: 5 * time.Minute}
	if got := len(cfg.PeriodicJobs()); got != 2 {
		t.Fatalf("periodic jobs without analytics: got %d, want 2", got)
	}
}

// TestPeriodicJobsConnectionExpiryNeedsInterval keeps the CON-219 health sweep
// from registering with a zero interval, which River doesn't validate and which
// would spin the periodic enqueuer (PeriodicInterval(0).Next(t) == t).
func TestPeriodicJobsConnectionExpiryNeedsInterval(t *testing.T) {
	base := PeriodicConfig{CleanupEvery: time.Hour, ReconcileEvery: 5 * time.Minute}

	zero := base
	zero.IncludeConnectionExpiry = true // HealthCheckEvery left 0
	if got := len(zero.PeriodicJobs()); got != 2 {
		t.Fatalf("enabled with zero interval should register no sweep: got %d, want 2", got)
	}

	set := base
	set.IncludeConnectionExpiry = true
	set.HealthCheckEvery = 6 * time.Hour
	if got := len(set.PeriodicJobs()); got != 3 {
		t.Fatalf("enabled with a positive interval should register the sweep: got %d, want 3", got)
	}
}
