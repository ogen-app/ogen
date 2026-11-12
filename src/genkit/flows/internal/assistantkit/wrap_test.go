package assistantkit

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/firebase/genkit/go/ai"
)

type invalidInput struct{ msg string }

func (e *invalidInput) Error() string { return e.msg }

func TestWrap(t *testing.T) {
	infra := errors.New("db down")
	tool := Wrap(func(_ context.Context, in string) (int, error) {
		switch in {
		case "ok":
			return 7, nil
		case "refuse":
			return 0, fmt.Errorf("check: %w", Refused("past date", "today", "tomorrow"))
		case "coded":
			return 0, RefusedCode("read_only", "it can't be edited")
		case "invalid":
			return 0, fmt.Errorf("engine: %w", &invalidInput{msg: "no platforms"})
		}
		return 0, infra
	}, As(func(e *invalidInput) string { return e.msg }))

	call := func(in string) (Outcome[int], error) {
		return tool(&ai.ToolContext{Context: t.Context()}, in)
	}

	if out, err := call("ok"); err != nil || !out.Succeeded() || *out.Result != 7 {
		t.Fatalf("ok: %+v, %v", out, err)
	}
	if out, err := call("refuse"); err != nil || out.Status != StatusRefused || out.Reason != "past date" ||
		!slices.Equal(out.Alternatives, []string{"today", "tomorrow"}) {
		t.Fatalf("refuse: %+v, %v", out, err)
	}
	if out, err := call("coded"); err != nil || out.Status != StatusRefused || out.Code != "read_only" {
		t.Fatalf("coded: %+v, %v", out, err)
	}
	if out, err := call("invalid"); err != nil || out.Status != StatusRefused || out.Reason != "no platforms" {
		t.Fatalf("matched: %+v, %v", out, err)
	}
	if _, err := call("boom"); !errors.Is(err, infra) {
		t.Fatalf("infra error = %v, want it returned", err)
	}
}

func TestOutputCode(t *testing.T) {
	cases := []struct {
		output any
		want   string
	}{
		{map[string]any{"status": "refused", "reason": "x", "code": "idea_read_only"}, "idea_read_only"},
		{map[string]any{"status": "refused", "reason": "x"}, ""},
		{map[string]any{"status": "ok", "code": "idea_read_only"}, ""},
		{"plain", ""},
	}
	for _, tc := range cases {
		if got := OutputCode(tc.output); got != tc.want {
			t.Errorf("OutputCode(%v) = %q, want %q", tc.output, got, tc.want)
		}
	}
}

func TestOutputOK(t *testing.T) {
	cases := []struct {
		output any
		want   bool
	}{
		{map[string]any{"status": "ok", "result": 1}, true},
		{map[string]any{"status": "proposed"}, true},
		{map[string]any{"status": "refused", "reason": "x"}, false},
		{map[string]any{"status": "conflict"}, false},
		{"plain", true},
		{nil, true},
	}
	for _, tc := range cases {
		if got := OutputOK(tc.output); got != tc.want {
			t.Errorf("OutputOK(%v) = %v, want %v", tc.output, got, tc.want)
		}
	}
}
