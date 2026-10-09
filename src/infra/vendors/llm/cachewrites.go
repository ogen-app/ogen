package llm

import (
	"context"
	"sync/atomic"
)

// UsageCacheCreation is the ai.GenerationUsage.Custom key carrying a call's
// prompt-cache write tokens. The genkit Anthropic plugin drops
// cache_creation_input_tokens, so CallMiddleware fills it from a
// CacheWriteProbe and the meter prices it as KindCacheCreation.
const UsageCacheCreation = "cache_creation_input_tokens"

// CacheWriteProbe collects the cache_creation_input_tokens Anthropic reports
// for one model call. The HTTP transport that sees the raw response observes
// it; the call's middleware reads it back.
type CacheWriteProbe struct{ tokens atomic.Int64 }

type cacheWriteProbeKey struct{}

// WithCacheWriteProbe returns ctx carrying a fresh probe.
func WithCacheWriteProbe(ctx context.Context) (context.Context, *CacheWriteProbe) {
	p := &CacheWriteProbe{}
	return context.WithValue(ctx, cacheWriteProbeKey{}, p), p
}

// CacheWriteProbeFrom returns the probe ctx carries, or nil.
func CacheWriteProbeFrom(ctx context.Context) *CacheWriteProbe {
	p, _ := ctx.Value(cacheWriteProbeKey{}).(*CacheWriteProbe)
	return p
}

// Observe records a reported cache-write count. A streamed response repeats
// the running total, and a retried request reports again, so the largest
// value is kept rather than a sum.
func (p *CacheWriteProbe) Observe(tokens int64) {
	for {
		cur := p.tokens.Load()
		if tokens <= cur || p.tokens.CompareAndSwap(cur, tokens) {
			return
		}
	}
}

// Tokens returns the largest count observed.
func (p *CacheWriteProbe) Tokens() int64 { return p.tokens.Load() }
