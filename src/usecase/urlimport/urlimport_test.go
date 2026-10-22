package urlimport_test

import (
	"errors"
	"testing"

	"github.com/ogen-app/ogen/src/usecase/urlimport"
)

func TestCanonical(t *testing.T) {
	for _, tt := range []struct {
		in, want, host string
		bad            bool
	}{
		{in: "HTTPS://Acme.COM/About/#team", want: "https://acme.com/About", host: "acme.com"},
		{in: "https://acme.com/", want: "https://acme.com/", host: "acme.com"},
		{in: " https://acme.com/pricing?plan=pro ", want: "https://acme.com/pricing?plan=pro", host: "acme.com"},
		{in: "ftp://acme.com", bad: true},
		{in: "https://", bad: true},
		{in: "::nope", bad: true},
	} {
		got, host, err := urlimport.Canonical(tt.in)
		if tt.bad {
			if _, ok := errors.AsType[*urlimport.InvalidURLError](err); !ok {
				t.Errorf("Canonical(%q) err = %v, want InvalidURLError", tt.in, err)
			}
			continue
		}
		if err != nil || got != tt.want || host != tt.host {
			t.Errorf("Canonical(%q) = %q, %q, %v; want %q, %q", tt.in, got, host, err, tt.want, tt.host)
		}
	}
}
