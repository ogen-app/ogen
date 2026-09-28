package zernio

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"github.com/ogen-app/ogen/src/kernel/tenantctx"
)

// DefaultCacheTTL is the cache freshness window from the ticket.
const DefaultCacheTTL = 30 * time.Second

// CachedSettingsStore is a 30-second in-process cache around the
// host's SettingsStore. It only caches keys under SettingPrefix —
// non-Zernio keys pass through directly so this wrapper is safe to
// install as a drop-in replacement.
//
// Entries are namespaced by the tenant in the read/write context,
// so one tenant's zernio.* settings never satisfy another
// tenant's read.
//
// Writes (Set / Delete) on a Zernio-prefixed key invalidate just that
// tenant's entry, not the whole cache, so unrelated reads remain warm.
//
// The cache is intentionally process-local; with multiple Ogen
// replicas, a bootstrapper write on instance A would not invalidate
// the cache on instance B until the TTL expires. That's acceptable —
// profile metadata changes infrequently and a 30-second drift is
// well within tolerance for a UI cache.
type CachedSettingsStore struct {
	inner SettingsStore
	ttl   time.Duration

	mu    sync.Mutex
	cache map[string]cacheEntry
}

type cacheEntry struct {
	value   string
	found   bool
	expires time.Time
}

// NewCachedSettingsStore wraps inner with the default TTL.
func NewCachedSettingsStore(inner SettingsStore) *CachedSettingsStore {
	return NewCachedSettingsStoreTTL(inner, DefaultCacheTTL)
}

// NewCachedSettingsStoreTTL is the explicit-TTL variant for tests.
func NewCachedSettingsStoreTTL(inner SettingsStore, ttl time.Duration) *CachedSettingsStore {
	return &CachedSettingsStore{
		inner: inner,
		ttl:   ttl,
		cache: make(map[string]cacheEntry),
	}
}

// cacheKey namespaces a cache entry by the tenant in ctx so one tenant's
// zernio.* settings never satisfy another tenant's read. A read with
// no tenant (system context) uses an empty namespace; that's fine because the
// cache only holds Zernio-prefixed keys, which are read on the per-tenant or
// request path.
func cacheKey(ctx context.Context, key string) string {
	tid, _ := tenantctx.From(ctx)
	return tid + "\x00" + key
}

// Get reads from the cache when fresh; on miss falls through to the
// inner store and refills the cache. Non-Zernio keys are not cached.
func (c *CachedSettingsStore) Get(ctx context.Context, key string) (string, bool, error) {
	if !strings.HasPrefix(key, SettingPrefix) {
		return c.inner.Get(ctx, key)
	}
	ck := cacheKey(ctx, key)

	c.mu.Lock()
	if e, ok := c.cache[ck]; ok && time.Now().Before(e.expires) {
		c.mu.Unlock()
		return e.value, e.found, nil
	}
	c.mu.Unlock()

	value, found, err := c.inner.Get(ctx, key)
	if err != nil {
		return "", false, err
	}
	c.mu.Lock()
	c.cache[ck] = cacheEntry{value: value, found: found, expires: time.Now().Add(c.ttl)}
	c.mu.Unlock()
	return value, found, nil
}

func (c *CachedSettingsStore) Set(ctx context.Context, key, value string) error {
	err := c.inner.Set(ctx, key, value)
	if err == nil && strings.HasPrefix(key, SettingPrefix) {
		c.invalidateEntry(cacheKey(ctx, key))
	}
	return err
}

func (c *CachedSettingsStore) Delete(ctx context.Context, key string) error {
	err := c.inner.Delete(ctx, key)
	if err == nil && strings.HasPrefix(key, SettingPrefix) {
		c.invalidateEntry(cacheKey(ctx, key))
	}
	return err
}

// Invalidate drops the cached entry for key across all tenants. Exposed so
// callers outside this package (e.g. an admin operation that pokes settings
// directly) can drop stale entries without writing through the store.
func (c *CachedSettingsStore) Invalidate(key string) {
	suffix := "\x00" + key
	c.mu.Lock()
	for k := range c.cache {
		if strings.HasSuffix(k, suffix) {
			delete(c.cache, k)
		}
	}
	c.mu.Unlock()
}

func (c *CachedSettingsStore) invalidateEntry(ck string) {
	c.mu.Lock()
	delete(c.cache, ck)
	c.mu.Unlock()
}

// LoadProfileMeta reads the cached zernio.profile_meta JSON blob and
// decodes it into a Profile. Returns (nil, nil) on missing meta (i.e.
// bootstrap has not yet succeeded) so callers can branch on nil.
//
// Unknown fields survive the round trip via Profile.Raw, matching the
// ticket's forward-compatibility requirement.
func LoadProfileMeta(ctx context.Context, store SettingsStore) (*Profile, error) {
	raw, found, err := store.Get(ctx, SettingProfileMeta)
	if err != nil {
		return nil, err
	}
	if !found || raw == "" {
		return nil, nil
	}
	return decodeProfile(json.RawMessage(raw))
}
