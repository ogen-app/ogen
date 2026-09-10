package zernio

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/kernel/tenantctx"
)

// memStore is a tiny in-memory SettingsStore for tests. The cache and
// bootstrap tests use it directly so they don't need SQLite.
type memStore struct {
	mu       sync.Mutex
	data     map[string]string
	getCount map[string]int
}

func newMemStore() *memStore {
	return &memStore{data: make(map[string]string), getCount: make(map[string]int)}
}

func (m *memStore) Get(_ context.Context, key string) (string, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.getCount[key]++
	v, ok := m.data[key]
	return v, ok, nil
}

func (m *memStore) Set(_ context.Context, key, value string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data[key] = value
	return nil
}

func (m *memStore) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.data, key)
	return nil
}

func (m *memStore) gets(key string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.getCount[key]
}

func TestCachedSettingsStorePassesThroughNonZernioKeys(t *testing.T) {
	inner := newMemStore()
	_ = inner.Set(context.Background(), "other.key", "v")
	c := NewCachedSettingsStore(inner)

	for range 5 {
		v, ok, err := c.Get(context.Background(), "other.key")
		if err != nil || !ok || v != "v" {
			t.Fatalf("unexpected get: %v %v %v", v, ok, err)
		}
	}
	if got := inner.gets("other.key"); got != 5 {
		t.Errorf("inner gets for non-zernio key: got %d want 5 (no caching)", got)
	}
}

func TestCachedSettingsStoreCachesZernioKeys(t *testing.T) {
	inner := newMemStore()
	_ = inner.Set(context.Background(), SettingProfileID, "abc")
	c := NewCachedSettingsStoreTTL(inner, 50*time.Millisecond)

	for range 5 {
		v, ok, err := c.Get(context.Background(), SettingProfileID)
		if err != nil || !ok || v != "abc" {
			t.Fatalf("unexpected get: %v %v %v", v, ok, err)
		}
	}
	if got := inner.gets(SettingProfileID); got != 1 {
		t.Errorf("inner gets within TTL: got %d want 1", got)
	}

	time.Sleep(60 * time.Millisecond)
	_, _, _ = c.Get(context.Background(), SettingProfileID)
	if got := inner.gets(SettingProfileID); got != 2 {
		t.Errorf("inner gets after TTL: got %d want 2", got)
	}
}

func TestCachedSettingsStoreInvalidatesOnSet(t *testing.T) {
	inner := newMemStore()
	_ = inner.Set(context.Background(), SettingProfileID, "abc")
	c := NewCachedSettingsStore(inner)

	_, _, _ = c.Get(context.Background(), SettingProfileID) // primes cache

	// External-looking write through the cached store should drop the
	// cache entry; the next Get goes to the inner store.
	if err := c.Set(context.Background(), SettingProfileID, "xyz"); err != nil {
		t.Fatal(err)
	}

	v, _, _ := c.Get(context.Background(), SettingProfileID)
	if v != "xyz" {
		t.Errorf("after Set: got %q want xyz", v)
	}
	if got := inner.gets(SettingProfileID); got != 2 {
		t.Errorf("inner gets after invalidation: got %d want 2", got)
	}
}

func TestCachedSettingsStoreInvalidatesOnDelete(t *testing.T) {
	inner := newMemStore()
	_ = inner.Set(context.Background(), SettingProfileID, "abc")
	c := NewCachedSettingsStore(inner)

	_, _, _ = c.Get(context.Background(), SettingProfileID)
	if err := c.Delete(context.Background(), SettingProfileID); err != nil {
		t.Fatal(err)
	}

	_, ok, _ := c.Get(context.Background(), SettingProfileID)
	if ok {
		t.Errorf("after Delete: should report missing")
	}
}

// tenantMemStore returns per-tenant values, keyed by (tenant, key), so a cache
// bleed shows up as one tenant getting another's value.
type tenantMemStore struct {
	mu   sync.Mutex
	data map[string]string
	gets int
}

func (s *tenantMemStore) tkey(ctx context.Context, key string) string {
	tid, _ := tenantctx.From(ctx)
	return tid + "\x00" + key
}

func (s *tenantMemStore) Get(ctx context.Context, key string) (string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gets++
	v, ok := s.data[s.tkey(ctx, key)]
	return v, ok, nil
}

func (s *tenantMemStore) Set(ctx context.Context, key, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[s.tkey(ctx, key)] = value
	return nil
}

func (s *tenantMemStore) Delete(ctx context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data, s.tkey(ctx, key))
	return nil
}

func TestCachedSettingsStoreTenantIsolation(t *testing.T) {
	inner := &tenantMemStore{data: map[string]string{
		"tenant-a\x00" + SettingProfileID: "prof-a",
		"tenant-b\x00" + SettingProfileID: "prof-b",
	}}
	c := NewCachedSettingsStore(inner)
	ctxA := tenantctx.With(context.Background(), "tenant-a")
	ctxB := tenantctx.With(context.Background(), "tenant-b")

	if v, _, _ := c.Get(ctxA, SettingProfileID); v != "prof-a" {
		t.Fatalf("tenant A: got %q want prof-a", v)
	}
	// B must NOT be served A's cached value.
	if v, _, _ := c.Get(ctxB, SettingProfileID); v != "prof-b" {
		t.Fatalf("tenant B leaked A's cached profile: got %q want prof-b", v)
	}
	// A's second read is served from cache (no additional inner Get).
	before := inner.gets
	if v, _, _ := c.Get(ctxA, SettingProfileID); v != "prof-a" {
		t.Fatalf("tenant A reread: got %q", v)
	}
	if inner.gets != before {
		t.Fatalf("A's second read should hit the cache; inner gets %d -> %d", before, inner.gets)
	}

	// Prime B's cache, then A's Set must invalidate only A's entry.
	_, _, _ = c.Get(ctxB, SettingProfileID)
	if err := c.Set(ctxA, SettingProfileID, "prof-a2"); err != nil {
		t.Fatal(err)
	}
	bBefore := inner.gets
	if v, _, _ := c.Get(ctxB, SettingProfileID); v != "prof-b" {
		t.Fatalf("tenant B after A's set: got %q want prof-b", v)
	}
	if inner.gets != bBefore {
		t.Fatalf("A's Set must not invalidate B's cache (inner gets %d -> %d)", bBefore, inner.gets)
	}
	if v, _, _ := c.Get(ctxA, SettingProfileID); v != "prof-a2" {
		t.Fatalf("tenant A after own set: got %q want prof-a2", v)
	}
}
