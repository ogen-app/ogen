package assistantkit

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"

	"github.com/ogen-app/ogen/src/infra/vendors/llm"
)

func TestFriendlyError(t *testing.T) {
	cases := map[string]struct {
		err  error
		want string
	}{
		"nil":         {nil, ""},
		"cancelled":   {fmt.Errorf("generate: %w", context.Canceled), MsgCancelled},
		"timeout":     {fmt.Errorf("generate: %w", context.DeadlineExceeded), MsgTimeout},
		"refused":     {fmt.Errorf("round 2: %w", llm.ErrRefused), MsgDeclined},
		"sdk 529":     {fmt.Errorf("plugin: %w", &anthropic.Error{StatusCode: 529}), MsgBusy},
		"sdk 429":     {fmt.Errorf("plugin: %w", &anthropic.Error{StatusCode: 429}), MsgBusy},
		"flattened":   {errors.New(`POST "https://api.anthropic.com/v1/messages": 529 {"type":"overloaded_error"}`), MsgBusy},
		"tool failed": {errors.New(`tool "generatePosts" failed: LinkedIn does not support post type text-post`), MsgGeneric},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := FriendlyError(tc.err); got != tc.want {
				t.Fatalf("FriendlyError = %q, want %q", got, tc.want)
			}
		})
	}
}
