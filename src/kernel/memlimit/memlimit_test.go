package memlimit

import (
	"errors"
	"testing"
)

func TestParse(t *testing.T) {
	cases := []struct {
		in      string
		want    int64
		noLimit bool
		bad     bool
	}{
		{in: "536870912\n", want: 512 << 20},
		{in: "max\n", noLimit: true},
		{in: "9223372036854771712\n", noLimit: true},
		{in: "0", noLimit: true},
		{in: "garbage", bad: true},
	}
	for _, c := range cases {
		got, err := parse([]byte(c.in))
		switch {
		case c.noLimit:
			if !errors.Is(err, errNoLimit) {
				t.Errorf("parse(%q) err = %v, want errNoLimit", c.in, err)
			}
		case c.bad:
			if err == nil || errors.Is(err, errNoLimit) {
				t.Errorf("parse(%q) err = %v, want a parse error", c.in, err)
			}
		case err != nil || got != c.want:
			t.Errorf("parse(%q) = (%d, %v), want (%d, nil)", c.in, got, err, c.want)
		}
	}
}

func TestApplyRespectsExplicitGOMEMLIMIT(t *testing.T) {
	t.Setenv("GOMEMLIMIT", "1GiB")
	if got := Apply(); got != 0 {
		t.Fatalf("Apply() = %d with GOMEMLIMIT set, want 0", got)
	}
}
