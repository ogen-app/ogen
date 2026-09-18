package handlers

import (
	"net/http"
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
