package geoip

import (
	"os"
	"testing"
)

func TestDisabledLocator(t *testing.T) {
	for _, l := range []*Locator{nil, Open(""), Open("/nonexistent/city.mmdb")} {
		if l.Enabled() {
			t.Fatal("a locator without a database must be disabled")
		}
		if got := l.Lookup("8.8.8.8"); got != "" {
			t.Fatalf("disabled lookup = %q, want empty", got)
		}
		if err := l.Close(); err != nil {
			t.Fatalf("close disabled: %v", err)
		}
	}
}

func TestLabel(t *testing.T) {
	cases := [][3]string{
		{"Kyiv", "Ukraine", "Kyiv, Ukraine"},
		{"", "Ukraine", "Ukraine"},
		{"Kyiv", "", ""},
	}
	for _, c := range cases {
		if got := label(c[0], c[1]); got != c[2] {
			t.Errorf("label(%q, %q) = %q, want %q", c[0], c[1], got, c[2])
		}
	}
}

// TestLookupWithDatabase runs against a real City database when
// GEOIP_TEST_DB points at one (the file is too large to vendor).
func TestLookupWithDatabase(t *testing.T) {
	path := os.Getenv("GEOIP_TEST_DB")
	if path == "" {
		t.Skip("GEOIP_TEST_DB not set")
	}
	l := Open(path)
	if !l.Enabled() {
		t.Fatalf("could not open %s", path)
	}
	t.Cleanup(func() { _ = l.Close() })

	for _, ip := range []string{"10.0.0.1", "127.0.0.1", "192.168.1.1", "::1", "not-an-ip", "100.64.0.1"} {
		if got := l.Lookup(ip); got != "" {
			t.Errorf("Lookup(%q) = %q, want empty for a non-public address", ip, got)
		}
	}
	for _, ip := range []string{"8.8.8.8", "2001:4860:4860::8888", "::ffff:8.8.8.8"} {
		if got := l.Lookup(ip); got == "" {
			t.Errorf("Lookup(%q) is empty, want a location", ip)
		} else {
			t.Logf("%s -> %s", ip, got)
		}
	}
}
