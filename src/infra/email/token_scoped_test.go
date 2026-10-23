package email

import (
	"errors"
	"testing"
	"time"
)

// A scoped token verifies only for its own scope, and never as a marketing
// unsubscribe token (nor a marketing token as a scoped one).
func TestScopedTokenIsBoundToScope(t *testing.T) {
	const secret = "s3cr3t-key"
	now := time.Unix(1_700_000_000, 0)
	tok := SignScoped(secret, ScopeWeeklyReport, "user-1", now)

	got, err := VerifyScoped(secret, ScopeWeeklyReport, tok, now)
	if err != nil || got != "user-1" {
		t.Fatalf("verify: %q, %v", got, err)
	}
	if _, err := VerifyScoped(secret, "other_scope", tok, now); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("other scope: got %v, want ErrInvalidToken", err)
	}
	if _, err := VerifyUnsubscribe(secret, tok, now); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("as marketing token: got %v, want ErrInvalidToken", err)
	}
	marketing := SignUnsubscribe(secret, "u@e.com", now)
	if _, err := VerifyScoped(secret, ScopeWeeklyReport, marketing, now); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("marketing as scoped: got %v, want ErrInvalidToken", err)
	}
	if _, err := VerifyScoped(secret, ScopeWeeklyReport, tok, now.Add(UnsubscribeTokenMaxAge+time.Hour)); !errors.Is(err, ErrExpiredToken) {
		t.Fatalf("expired: got %v, want ErrExpiredToken", err)
	}
}
