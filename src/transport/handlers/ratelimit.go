package handlers

import (
	"math"
	"strconv"
	"sync"
	"time"

	"github.com/gofiber/fiber/v2"
)

// maxRateLimiterBuckets is a hard cap on a keyed limiter's map so a spray of
// distinct keys (many addresses / spoofed IPs) can't grow it without bound. When
// a new key would exceed the cap, idle (fully-refilled) buckets are swept first;
// if none can be reclaimed the new key is simply not tracked (see touch).
const maxRateLimiterBuckets = 50_000

// keyedRateLimiter is an in-memory, per-instance token-bucket limiter keyed by
// an arbitrary string (an email address or a client IP). It backs the public,
// unauthenticated endpoints that are attractive to abuse: password-reset
// requests (CON-161, which send mail on demand), and login + signup (CON-162,
// where an unthrottled endpoint is a credential-stuffing / mass-signup vector).
//
// Per-instance is sufficient for the current single-replica deploy and matches
// the existing Zernio connect-link limiter; a multi-replica deploy would move
// this to a shared store (Redis INCR + TTL). The clock is injectable so tests
// can advance time deterministically.
type keyedRateLimiter struct {
	capacity   float64
	refillRate float64 // tokens per second
	ttl        time.Duration

	mu      sync.Mutex
	buckets map[string]*rlBucket
	now     func() time.Time
}

type rlBucket struct {
	tokens   float64
	lastSeen time.Time
}

// newKeyedRateLimiter builds a limiter allowing `burst` requests per `window`
// per key, refilling continuously. A key idle for longer than 2×window has
// fully refilled, so it can be dropped without changing behaviour.
func newKeyedRateLimiter(burst float64, window time.Duration) *keyedRateLimiter {
	return &keyedRateLimiter{
		capacity:   burst,
		refillRate: burst / window.Seconds(),
		ttl:        2 * window,
		buckets:    make(map[string]*rlBucket),
		now:        time.Now,
	}
}

// touch returns the refilled bucket for key, creating a full one when unseen, or
// nil when the map is at capacity and no idle bucket can be reclaimed for it.
// Callers must hold l.mu.
func (l *keyedRateLimiter) touch(key string, now time.Time) *rlBucket {
	b, ok := l.buckets[key]
	if !ok {
		// New key: keep maxRateLimiterBuckets a hard cap. Sweep idle buckets to make
		// room; if the map is still full (every bucket is active) don't grow it —
		// return nil so the caller leaves this key untracked. Per-key limiting does
		// nothing against an all-distinct-key flood anyway, so not tracking a new key
		// there costs no protection, and the buckets already tracking real activity
		// are preserved rather than evicted.
		if len(l.buckets) >= maxRateLimiterBuckets {
			l.sweep(now)
			if len(l.buckets) >= maxRateLimiterBuckets {
				return nil
			}
		}
		b = &rlBucket{tokens: l.capacity, lastSeen: now}
		l.buckets[key] = b
		return b
	}
	elapsed := now.Sub(b.lastSeen).Seconds()
	b.tokens += elapsed * l.refillRate
	if b.tokens > l.capacity {
		b.tokens = l.capacity
	}
	b.lastSeen = now
	return b
}

// allow consumes one token for key, creating the bucket if unseen. It returns
// (true, 0) when the request is permitted, or (false, retryAfter) when the key
// is exhausted — retryAfter is the wait until the next token, suitable for a
// Retry-After header. Used by the endpoints that charge every attempt
// (password-reset request, signup).
func (l *keyedRateLimiter) allow(key string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()

	b := l.touch(key, l.now())
	if b == nil {
		return true, 0 // limiter at capacity ⇒ leave untracked and fail open
	}
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	return false, l.waitFor(b)
}

// retryAfter peeks at key without consuming a token: it returns 0 when a token
// is available — or when the key is unseen, so a peek never allocates a bucket
// for the common (successful) path — and the wait until the next token when the
// key is exhausted. Login charges only on a failed attempt, so it
// peeks here to reject an already-exhausted key, then penalizes on failure.
func (l *keyedRateLimiter) retryAfter(key string) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()

	if _, ok := l.buckets[key]; !ok {
		return 0 // unseen ⇒ full ⇒ allowed; don't create a bucket on a peek
	}
	b := l.touch(key, l.now())
	if b.tokens >= 1 {
		return 0
	}
	return l.waitFor(b)
}

// penalize charges one token against key, creating the bucket if unseen. Tokens
// are clamped at 0 so a run of failures can't drive the wait unboundedly long —
// that bounds how long an attacker can keep a victim's key throttled (the wait
// tops out at window/burst). Meant to be called only after a failed attempt, so
// a success costs nothing (count failures only).
func (l *keyedRateLimiter) penalize(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	b := l.touch(key, l.now())
	if b == nil {
		return // limiter at capacity ⇒ nothing to charge
	}
	if b.tokens >= 1 {
		b.tokens--
	} else {
		b.tokens = 0
	}
}

// reset refunds key to a full bucket by dropping it (an absent key is treated as
// full). Login calls this on a successful attempt so a legitimate user's earlier
// fumbles don't carry over (a success resets the counter).
func (l *keyedRateLimiter) reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.buckets, key)
}

// waitFor is the time until b holds one whole token. Callers must hold l.mu.
func (l *keyedRateLimiter) waitFor(b *rlBucket) time.Duration {
	return time.Duration((1 - b.tokens) / l.refillRate * float64(time.Second))
}

// sweep drops buckets untouched for longer than ttl. Such a bucket has fully
// refilled, so removing it is equivalent to leaving a full one behind — it just
// bounds memory. Called under l.mu.
func (l *keyedRateLimiter) sweep(now time.Time) {
	for k, b := range l.buckets {
		if now.Sub(b.lastSeen) > l.ttl {
			delete(l.buckets, k)
		}
	}
}

// tooManyRequests writes a 429 with a rounded-up Retry-After (≥1s) and a
// user-facing message (the login/signup forms surface the response body
// verbatim), matching the Zernio connect-link limiter's contract.
func tooManyRequests(c *fiber.Ctx, retry time.Duration, message string) error {
	sec := max(int(math.Ceil(retry.Seconds())), 1)
	c.Set("Retry-After", strconv.Itoa(sec))
	return fiber.NewError(fiber.StatusTooManyRequests, message)
}
