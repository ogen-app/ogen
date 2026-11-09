package post_assistant

import (
	"errors"
	"testing"

	"github.com/ogen-app/ogen/src/genkit/flows/internal/assistantkit"
)

// The reviser works on the post it names, not a thread's: a post gone since
// is refused.
func TestReviser_PostGone(t *testing.T) {
	revise, err := NewReviser(nil, PostAssistantFlowConfig{}, PostAssistantRepos{Posts: &savedPosts{}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = revise(t.Context(), "gone", "fix it", "")
	if _, ok := errors.AsType[*assistantkit.Refusal](err); !ok {
		t.Fatalf("err = %v, want a refusal", err)
	}
}
