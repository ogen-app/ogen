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

func (m *memStore) Heartbeat(_ context.Context, _, token string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.row.Token != nil && *m.row.Token == token && m.row.DoneAt == nil {
		now := time.Now()
		m.row.HeartbeatAt = &now
	}
	return nil
}

func (m *memStore) Finish(_ context.Context, _, token, errText, code string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
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
