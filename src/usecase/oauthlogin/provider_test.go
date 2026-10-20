package oauthlogin

import "testing"

func TestClaimsAuthoritative(t *testing.T) {
	cases := []struct {
		claims Claims
		want   bool
	}{
		{Claims{Email: "jane@gmail.com", EmailVerified: true}, true},
		{Claims{Email: "Jane@GMail.com", EmailVerified: true}, true},
		{Claims{Email: "jane@googlemail.com", EmailVerified: true}, true},
		{Claims{Email: "jane@gmail.com"}, false},
		{Claims{Email: "jane@acme.com", EmailVerified: true}, false},
		{Claims{Email: "jane@acme.com", EmailVerified: true, HostedDomain: "acme.com"}, true},
		{Claims{Email: "jane@acme.com", EmailVerified: true, HostedDomain: "evil.com"}, false},
		{Claims{Email: "jane@sub.acme.com", EmailVerified: true, HostedDomain: "acme.com"}, false},
		{Claims{Email: "not-an-address", EmailVerified: true}, false},
	}
	for _, tc := range cases {
		if got := tc.claims.Authoritative(); got != tc.want {
			t.Errorf("%+v: Authoritative() = %v, want %v", tc.claims, got, tc.want)
		}
	}
}
