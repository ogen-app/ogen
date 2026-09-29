package geoip

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFeed(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "feed.csv")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

const cloudflareFeed = `# Cloudflare egress
104.28.88.128/25,DZ,DZ-16,Algiers,
104.28.88.142/31,ES,ES-AN,Sevilla,
104.28.88.144/32,TG,TG-M,Lome,
2a09:bac0:1000::/45,PL,PL-14,Warsaw,
2a09:bac0:1000:10::/64,PL,PL-12,Krakow,
104.28.90.0/24,ZZ,,,
104.28.91.0/24,FR,,,
not-a-prefix,US,,Nowhere,
104.28.92.0/24
`

const appleFeed = `172.224.226.0/27,GB,GB-EN,London,
104.28.88.144/32,US,US-CA,Cupertino,
`

func TestGeofeedLookup(t *testing.T) {
	l := Open("", []string{writeFeed(t, cloudflareFeed), writeFeed(t, appleFeed)})
	if !l.Enabled() {
		t.Fatal("a locator with a geofeed must be enabled")
	}
	cases := map[string]string{
		// The longest prefix wins over the enclosing /25.
		"104.28.88.143": "Sevilla, Spain",
		"104.28.88.142": "Sevilla, Spain",
		"104.28.88.141": "Algiers, Algeria",
		// A prefix in both feeds keeps the first feed's location.
		"104.28.88.144":          "Lome, Togo",
		"172.224.226.9":          "London, United Kingdom",
		"::ffff:172.224.226.9":   "London, United Kingdom",
		"2a09:bac0:1000:10::1":   "Krakow, Poland",
		"2a09:bac0:1000:11::1":   "Warsaw, Poland",
		"2a09:bac0:1007:ffff::1": "Warsaw, Poland",
		"104.28.91.7":            "France",
		// Listed without a usable country: known relay, unknown location.
		"104.28.90.1": "",
		// Not in any feed and no City database behind it.
		"2a09:bac0:1008::1": "",
		"8.8.8.8":           "",
		"10.0.0.1":          "",
	}
	for ip, want := range cases {
		if got := l.Lookup(ip); got != want {
			t.Errorf("Lookup(%q) = %q, want %q", ip, got, want)
		}
	}
}

func TestGeofeedSkipsBadRows(t *testing.T) {
	b := newFeedBuilder()
	st, err := b.add(strings.NewReader(cloudflareFeed))
	if err != nil {
		t.Fatal(err)
	}
	if st.rows != 7 || st.skipped != 2 {
		t.Fatalf("stats = %+v, want 7 rows and 2 skipped", st)
	}
}

// TestGeofeedWithRealFeeds loads the published feeds when GEOIP_TEST_FEEDS
// lists them (comma-separated; too large to vendor).
func TestGeofeedWithRealFeeds(t *testing.T) {
	paths := os.Getenv("GEOIP_TEST_FEEDS")
	if paths == "" {
		t.Skip("GEOIP_TEST_FEEDS not set")
	}
	l := Open(os.Getenv("GEOIP_TEST_DB"), strings.Split(paths, ","))
	t.Cleanup(func() { _ = l.Close() })
	// A Cloudflare WARP / Private Relay egress address the Lite database
	// places in Algiers.
	if got, want := l.Lookup("104.28.88.143"), "Sevilla, Spain"; got != want {
		t.Errorf("Lookup(104.28.88.143) = %q, want %q", got, want)
	}
}
