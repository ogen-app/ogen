package post_assistant

import (
	"fmt"
	"testing"
	"time"
)

func TestContextCacheIsBounded(t *testing.T) {
	contextCacheMu.Lock()
	clear(contextCache)
	clear(contextCacheGen)
	clear(contextCacheInflight)
	contextCacheMu.Unlock()
	t.Cleanup(func() {
		contextCacheMu.Lock()
		clear(contextCache)
		contextCacheMu.Unlock()
	})

	now := time.Now()
	contextCacheMu.Lock()
	for i := range contextCacheMax {
		expires := now.Add(time.Minute)
		if i%2 == 0 {
			expires = now.Add(-time.Minute)
		}
		contextCache[fmt.Sprint("post-", i)] = &contextCacheEntry{expiresAt: expires}
	}
	pruneContextCacheLocked(now)
	live := len(contextCache)
	contextCacheMu.Unlock()
	if live != contextCacheMax/2 {
		t.Fatalf("after pruning a full cache: %d entries, want the %d live ones", live, contextCacheMax/2)
	}

	contextCacheMu.Lock()
	for i := range contextCacheMax {
		contextCache[fmt.Sprint("live-", i)] = &contextCacheEntry{expiresAt: now.Add(time.Minute)}
	}
	pruneContextCacheLocked(now)
	left := len(contextCache)
	contextCacheMu.Unlock()
	if left != 0 {
		t.Fatalf("a cache full of live entries must start over, has %d", left)
	}
}

func TestInvalidateWithoutAssemblyKeepsNoGeneration(t *testing.T) {
	invalidateContextCache("post-idle")
	contextCacheMu.Lock()
	_, kept := contextCacheGen["post-idle"]
	contextCacheMu.Unlock()
	if kept {
		t.Fatal("invalidating a post with no assembly in flight must not leave a generation entry")
	}
}
