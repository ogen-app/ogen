package handlers

import (
	"encoding/json"
	"net/http"
	"net/url"
	"testing"
)

// TestConnectErrorCodeForStatus pins the mapping from a Zernio select-step HTTP
// status to the SPA connect_error code. The point of the split (CON-217
// follow-up) is that a permission/expiry failure must NOT surface as the
// transient "upstream" ("try again in a moment") copy, which sends the user into
// a futile retry loop; only genuinely transient statuses keep it.
func TestConnectErrorCodeForStatus(t *testing.T) {
	cases := []struct {
		name   string
		status int
		want   string
	}{
		// Transient — a plain retry can succeed, so "try again in a moment" holds.
		{"internal error", http.StatusInternalServerError, "upstream"},
		{"bad gateway", http.StatusBadGateway, "upstream"},
		{"service unavailable", http.StatusServiceUnavailable, "upstream"},
		{"gateway timeout", http.StatusGatewayTimeout, "upstream"},
		{"request timeout", http.StatusRequestTimeout, "upstream"},
		{"too many requests", http.StatusTooManyRequests, "upstream"},

		// Stale connect token — the link expired, reconnect required.
		{"unauthorized", http.StatusUnauthorized, "expired"},

		// No grantable page/profile — Page access not granted or none postable.
		{"forbidden", http.StatusForbidden, "permission"},
		{"bad request", http.StatusBadRequest, "permission"},

		// Unclassified 4xx falls back to the retry bucket rather than mislabeling
		// the cause as a permission problem.
		{"not found", http.StatusNotFound, "upstream"},
		{"conflict", http.StatusConflict, "upstream"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := connectErrorCodeForStatus(tc.status); got != tc.want {
				t.Errorf("connectErrorCodeForStatus(%d) = %q, want %q", tc.status, got, tc.want)
			}
		})
	}
}

// TestRawUserProfile guards the fix for the connect regression where Zernio's
// select-page POST failed with "Invalid input: expected object, received
// undefined": the userProfile callback param arrives (double-)URL-encoded, and
// the old code dropped it whenever a single decode left it non-JSON. It must now
// decode until the object parses, and forward the fully-decoded original.
func TestRawUserProfile(t *testing.T) {
	const obj = `{"id":"123","username":"mybrand","displayName":"My Brand Page"}`
	once := url.QueryEscape(obj)   // arrives single-encoded
	twice := url.QueryEscape(once) // arrives double-encoded

	cases := []struct {
		name      string
		in        string
		recovered bool
	}{
		{"plain json", obj, true},
		{"single-encoded", once, true},
		{"double-encoded", twice, true},
		{"empty", "", false},
		{"whitespace", "   ", false},
		{"not json", "hello world", false},
		{"encoded not json", url.QueryEscape("hello world"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := rawUserProfile(tc.in)
			if (len(got) > 0) != tc.recovered {
				t.Fatalf("rawUserProfile(%q) recovered=%v (got %q), want recovered=%v",
					tc.in, len(got) > 0, string(got), tc.recovered)
			}
			if !tc.recovered {
				return
			}
			if !json.Valid(got) {
				t.Fatalf("rawUserProfile(%q) returned invalid JSON: %q", tc.in, string(got))
			}
			// Must be fully decoded back to the original object, not a
			// half-decoded string still carrying %XX escapes.
			var m map[string]any
			if err := json.Unmarshal(got, &m); err != nil {
				t.Fatalf("unmarshal recovered: %v", err)
			}
			if m["displayName"] != "My Brand Page" {
				t.Errorf("displayName = %v, want %q (decoding incomplete)", m["displayName"], "My Brand Page")
			}
		})
	}
}
