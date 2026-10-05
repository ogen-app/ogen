package models

import (
	"strings"
	"testing"
	"time"
)

func TestNewPluginToken(t *testing.T) {
	token, hash, err := NewPluginToken()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(token, PluginTokenPrefix) {
		t.Fatalf("token %q lacks the %q prefix", token, PluginTokenPrefix)
	}
	// 32 bytes base64url without padding is 43 characters.
	if got := len(token) - len(PluginTokenPrefix); got != 43 {
		t.Fatalf("secret length = %d, want 43", got)
	}
	if hash != HashPluginSecret(token) {
		t.Fatal("returned hash doesn't match HashPluginSecret(token)")
	}
	if !LooksLikePluginToken(token) {
		t.Fatal("a minted token must look like a plugin token")
	}
	other, _, err := NewPluginToken()
	if err != nil {
		t.Fatal(err)
	}
	if other == token {
		t.Fatal("two minted tokens are equal")
	}
}

func TestLooksLikePluginToken(t *testing.T) {
	for _, s := range []string{"", "ogp_", "abc", "Bearer ogp_x", "OGP_abc"} {
		if LooksLikePluginToken(s) {
			t.Errorf("%q should not look like a plugin token", s)
		}
	}
}

func TestNewPluginPairingKeys(t *testing.T) {
	k, err := NewPluginPairingKeys()
	if err != nil {
		t.Fatal(err)
	}
	if k.ReadKey == k.WriteKey {
		t.Fatal("read and write keys must be independent")
	}
	if k.ReadKeyHash != HashPluginSecret(k.ReadKey) || k.WriteKeyHash != HashPluginSecret(k.WriteKey) {
		t.Fatal("key hashes don't match their keys")
	}
}

func TestPluginPairingExpired(t *testing.T) {
	exp := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	p := &PluginPairing{ExpiresAt: exp}
	if p.Expired(exp.Add(-time.Nanosecond)) {
		t.Error("not expired just before expires_at")
	}
	if !p.Expired(exp) {
		t.Error("expired exactly at expires_at")
	}
}
