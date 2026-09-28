package queues

import (
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/email/templates"
	"github.com/ogen-app/ogen/src/infra/publishers/zernio"
)

//go:fix inline

func TestClassifyHealth(t *testing.T) {
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	const lead = 7

	cases := []struct {
		name string
		h    zernio.AccountHealth
		want string
	}{
		{"healthy far expiry", zernio.AccountHealth{Status: "healthy", TokenValid: true, TokenExpiresAt: new(now.AddDate(0, 0, 30))}, ""},
		{"healthy no expiry", zernio.AccountHealth{Status: "healthy", TokenValid: true}, ""},
		{"within lead window", zernio.AccountHealth{Status: "healthy", TokenExpiresAt: new(now.AddDate(0, 0, 3))}, templates.StageExpiringSoon},
		{"exactly at lead edge", zernio.AccountHealth{Status: "healthy", TokenExpiresAt: new(now.AddDate(0, 0, 7))}, templates.StageExpiringSoon},
		{"warning status", zernio.AccountHealth{Status: "warning", TokenExpiresAt: new(now.AddDate(0, 0, 30))}, templates.StageExpiringSoon},
		{"error status", zernio.AccountHealth{Status: "error"}, templates.StageActionRequired},
		{"needs reconnect", zernio.AccountHealth{Status: "healthy", NeedsReconnect: true}, templates.StageActionRequired},
		{"already expired", zernio.AccountHealth{Status: "healthy", TokenExpiresAt: new(now.AddDate(0, 0, -1))}, templates.StageActionRequired},
		{"expired trumps warning", zernio.AccountHealth{Status: "warning", TokenExpiresAt: new(now.AddDate(0, 0, -1))}, templates.StageActionRequired},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyHealth(tc.h, now, lead); got != tc.want {
				t.Errorf("classifyHealth = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestExpiryIdempotencyKey(t *testing.T) {
	exp := time.Date(2026, 9, 15, 8, 30, 0, 0, time.UTC)
	// Same account+stage+expiry+owner ⇒ stable key (notify-once across sweeps).
	a := expiryIdempotencyKey("acc1", templates.StageExpiringSoon, &exp, "owner1")
	b := expiryIdempotencyKey("acc1", templates.StageExpiringSoon, &exp, "owner1")
	if a != b {
		t.Fatalf("key not stable: %q vs %q", a, b)
	}
	if want := "conn_expiring:acc1:expiring_soon:2026-09-15:owner1"; a != want {
		t.Errorf("key = %q, want %q", a, want)
	}
	// A moved expiry ⇒ a fresh key, so a later re-expiry re-notifies.
	exp2 := exp.AddDate(0, 0, 60)
	if c := expiryIdempotencyKey("acc1", templates.StageExpiringSoon, &exp2, "owner1"); c == a {
		t.Errorf("moved expiry should change the key, both = %q", a)
	}
	// A different owner ⇒ its own key.
	if d := expiryIdempotencyKey("acc1", templates.StageExpiringSoon, &exp, "owner2"); d == a {
		t.Errorf("different owner should change the key")
	}
	// Nil expiry ⇒ "none" bucket, still stable.
	if e := expiryIdempotencyKey("acc1", templates.StageActionRequired, nil, "owner1"); e != "conn_expiring:acc1:action_required:none:owner1" {
		t.Errorf("nil-expiry key = %q", e)
	}
}

func TestPlatformLabel(t *testing.T) {
	cases := map[string]string{
		"linkedin": "LinkedIn",
		"twitter":  "X (Twitter)",
		"youtube":  "YouTube",
		"mastodon": "Mastodon", // unknown slug ⇒ capitalized
		"":         "social",
	}
	for slug, want := range cases {
		if got := platformLabel(slug); got != want {
			t.Errorf("platformLabel(%q) = %q, want %q", slug, got, want)
		}
	}
}

func TestAccountLabel(t *testing.T) {
	// Prefers the live health payload, falls back to the local mirror, then a default.
	if got := accountLabel(zernio.AccountHealth{DisplayName: "Acme Corp", Username: "acme"}, models.SocialAccount{}); got != "Acme Corp" {
		t.Errorf("display name preferred: got %q", got)
	}
	if got := accountLabel(zernio.AccountHealth{Username: "acme"}, models.SocialAccount{DisplayName: "ignored"}); got != "acme" {
		t.Errorf("username fallback: got %q", got)
	}
	if got := accountLabel(zernio.AccountHealth{}, models.SocialAccount{Username: "local-handle"}); got != "local-handle" {
		t.Errorf("local mirror fallback: got %q", got)
	}
	if got := accountLabel(zernio.AccountHealth{}, models.SocialAccount{}); got != "your account" {
		t.Errorf("default fallback: got %q", got)
	}
}

func TestHumanizeUntil(t *testing.T) {
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		in   time.Time
		want string
	}{
		{now.AddDate(0, 0, -1), "today"},
		{now.Add(2 * time.Hour), "in less than a day"},
		{now.Add(25 * time.Hour), "in 1 day"},
		{now.AddDate(0, 0, 6), "in 6 days"},
	}
	for _, tc := range cases {
		if got := humanizeUntil(tc.in, now); got != tc.want {
			t.Errorf("humanizeUntil(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
