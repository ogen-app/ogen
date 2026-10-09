package handlers

import (
	"context"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// ttlCache memoises a slow read per key for a fixed time, and makes
// concurrent misses for one key share a single load. Only successful loads
// are kept. It holds at most maxEntries keys; expired keys are dropped when a
// new one would exceed that, and if all are live the cache starts over.
type ttlCache[V any] struct {
	ttl        time.Duration
	maxEntries int
	now        func() time.Time

	group singleflight.Group

	mu      sync.Mutex
	entries map[string]ttlEntry[V]
}

type ttlEntry[V any] struct {
	value   V
	expires time.Time
}

func newTTLCache[V any](ttl time.Duration, maxEntries int) *ttlCache[V] {
	return &ttlCache[V]{ttl: ttl, maxEntries: maxEntries, now: time.Now, entries: map[string]ttlEntry[V]{}}
}

// get returns the cached value for key, or runs load and caches its result.
// load runs detached from the caller's cancellation, so one client going away
// does not fail the shared load for the others waiting on it.
func (c *ttlCache[V]) get(ctx context.Context, key string, load func(context.Context) (V, error)) (V, error) {
	c.mu.Lock()
	e, ok := c.entries[key]
	c.mu.Unlock()
	if ok && c.now().Before(e.expires) {
		return e.value, nil
	}
	v, err, _ := c.group.Do(key, func() (any, error) {
		v, err := load(context.WithoutCancel(ctx))
		if err != nil {
			return v, err
		}
		c.put(key, v)
		return v, nil
	})
	out, _ := v.(V)
	return out, err
}

func (c *ttlCache[V]) put(key string, v V) {
	now := c.now()
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.entries[key]; !exists && len(c.entries) >= c.maxEntries {
		for k, e := range c.entries {
			if !now.Before(e.expires) {
				delete(c.entries, k)
			}
		}
		if len(c.entries) >= c.maxEntries {
			clear(c.entries)
		}
	}
	c.entries[key] = ttlEntry[V]{value: v, expires: now.Add(c.ttl)}
}
