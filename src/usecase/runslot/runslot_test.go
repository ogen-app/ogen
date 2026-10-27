package runslot_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/ogen-app/ogen/src/usecase/runslot"
)

// memStore is a one-row runslot.Store.
type memStore struct {
	mu  sync.Mutex
	row runslot.Row
	// beatErr fails every heartbeat; finishErrs fails that many Finish
	// calls first.
	beatErr    error
	finishErrs int
}

func (m *memStore) Claim(_ context.Context, _, token string, staleBefore time.Time) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.row.Token != nil && m.row.DoneAt == nil && m.row.HeartbeatAt.After(staleBefore) {
		return false, nil
	}
	now := time.Now()
	m.row = runslot.Row{Token: &token, HeartbeatAt: &now}
	return true, nil
}

func (m *memStore) Heartbeat(_ context.Context, _, token string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.beatErr != nil {
		return false, m.beatErr
	}
	if m.row.Token != nil && *m.row.Token == token && m.row.DoneAt == nil {
		now := time.Now()
		m.row.HeartbeatAt = &now
		return true, nil
	}
	return false, nil
}

// steal hands the slot to another run, as a takeover after a stale
// heartbeat does.
func (m *memStore) steal(token string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	m.row = runslot.Row{Token: &token, HeartbeatAt: &now}
}

func (m *memStore) Finish(_ context.Context, _, token, errText, code string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.finishErrs > 0 {
		m.finishErrs--
		return errors.New("db unavailable")
	}
	if m.row.Token != nil && *m.row.Token == token {
		now := time.Now()
		m.row.DoneAt, m.row.Err, m.row.Code = &now, errText, code
	}
	return nil
}

func (m *memStore) Read(context.Context, string) (runslot.Row, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.row, nil
}

func TestStartRunsDetachedAndRecordsTheOutcome(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := &memStore{}
		release := make(chan struct{})
		code := func(error) string { return "boom_code" }
		ctx, cancel := context.WithCancel(t.Context())
		token, err := runslot.Start(ctx, store, "r1", time.Hour, code, func(context.Context) error {
			<-release
			return errors.New("boom")
		})
		if err != nil || token == "" {
			t.Fatalf("Start = %q, %v", token, err)
		}
		// The caller's context ending doesn't stop the run.
		cancel()

		// A heartbeat keeps the slot live past the stale window.
		time.Sleep(2 * runslot.Stale)
		synctest.Wait()
		slot, _ := runslot.Read(t.Context(), store, "r1")
		if !slot.Running || slot.Token != token {
			t.Fatalf("slot = %+v, want running under %s", slot, token)
		}
		if again, _ := runslot.Start(t.Context(), store, "r1", time.Hour, nil, func(context.Context) error { return nil }); again != "" {
			t.Fatal("a second Start claimed a live slot")
		}

		close(release)
		synctest.Wait()
		slot, _ = runslot.Read(t.Context(), store, "r1")
		if !slot.Done || slot.Err == nil || slot.Err.Error() != "boom" || slot.Code != "boom_code" {
			t.Fatalf("slot = %+v, want done with boom", slot)
		}
	})
}

func TestStartRecordsAPanic(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := &memStore{}
		if _, err := runslot.Start(t.Context(), store, "r1", time.Hour, nil, func(context.Context) error { panic("bad") }); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		if slot, _ := runslot.Read(t.Context(), store, "r1"); !slot.Done || slot.Err == nil {
			t.Fatalf("slot = %+v, want done with the panic", slot)
		}
	})
}

// TestStartStopsARunThatLostItsSlot checks a run another run took the slot
// from is cancelled at its next heartbeat, so it stops spending on the model.
func TestStartStopsARunThatLostItsSlot(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := &memStore{}
		cancelled := make(chan struct{})
		if _, err := runslot.Start(t.Context(), store, "r1", time.Hour, nil, func(ctx context.Context) error {
			<-ctx.Done()
			close(cancelled)
			return ctx.Err()
		}); err != nil {
			t.Fatal(err)
		}
		store.steal("other")
		time.Sleep(time.Minute)
		synctest.Wait()
		select {
		case <-cancelled:
		default:
			t.Fatal("the displaced run was not cancelled")
		}
		if slot, _ := runslot.Read(t.Context(), store, "r1"); slot.Token != "other" || slot.Done {
			t.Fatalf("slot = %+v, want the new run's, untouched", slot)
		}
	})
}

// TestStartFencesARunThatCannotConfirmItsSlot checks a run whose heartbeats
// keep failing stops itself before its slot goes stale, so a run that takes
// the slot after Stale never writes alongside it.
func TestStartFencesARunThatCannotConfirmItsSlot(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := &memStore{beatErr: errors.New("db unavailable")}
		stopped := make(chan time.Time, 1)
		start := time.Now()
		if _, err := runslot.Start(t.Context(), store, "r1", time.Hour, nil, func(ctx context.Context) error {
			<-ctx.Done()
			stopped <- time.Now()
			return ctx.Err()
		}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(runslot.Stale)
		synctest.Wait()
		select {
		case at := <-stopped:
			if at.Sub(start) >= runslot.Stale {
				t.Fatalf("stopped after %v, want before the slot goes stale (%v)", at.Sub(start), runslot.Stale)
			}
		default:
			t.Fatal("the run kept going without a confirmed heartbeat")
		}
	})
}

// TestStartRetriesRecordingTheOutcome checks a failed Finish is retried, so a
// run that succeeded isn't run again for want of its outcome.
func TestStartRetriesRecordingTheOutcome(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := &memStore{finishErrs: 2}
		if _, err := runslot.Start(t.Context(), store, "r1", time.Hour, nil, func(context.Context) error { return nil }); err != nil {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Second)
		synctest.Wait()
		if slot, _ := runslot.Read(t.Context(), store, "r1"); !slot.Done || slot.Err != nil {
			t.Fatalf("slot = %+v, want the outcome recorded on the third try", slot)
		}
	})
}
