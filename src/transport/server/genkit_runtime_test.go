package server

import (
	"context"
	"errors"
	"testing"

	"github.com/ogen-app/ogen/src/genkit/flows/assistant"
)

func TestRebuildWithoutKeyDisablesTheAssistant(t *testing.T) {
	r := &genkitRuntime{assistantFn: func(context.Context, assistant.Request, assistant.OnEventFunc) (*assistant.Response, error) {
		return &assistant.Response{}, nil
	}}
	if err := r.rebuild(t.Context(), adminKeyStore{}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.RunAssistant(t.Context(), assistant.Request{}, nil); !errors.Is(err, ErrAnthropicUnavailable) {
		t.Fatalf("err = %v, want ErrAnthropicUnavailable once the key is removed", err)
	}
}
