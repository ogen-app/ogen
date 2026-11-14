package viewresolve

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/domain/viewcontext"
)

// findingMessages is how far back the campaign's thread is read for the
// review a finding came from.
const findingMessages = 200

// finding is the post finding v.From names, as the latest review in the
// campaign's thread that has it gave it. A finding that thread doesn't
// have, or one about another post, is a *viewcontext.InvalidError.
func (r *Resolver) finding(ctx context.Context, v *viewcontext.ViewContext) (*viewcontext.Finding, error) {
	from := v.From
	if r.threads == nil {
		return nil, &viewcontext.InvalidError{Field: "from", Reason: "findings can't be read here"}
	}
	msgs, err := r.threads.ListRecent(ctx, models.AssistantScopeCampaign, from.CampaignID, findingMessages)
	if err != nil {
		return nil, fmt.Errorf("read the campaign's thread: %w", err)
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role != "model" {
			continue
		}
		f := findIn(msgs[i].Content, from.Key)
		if f == nil {
			continue
		}
		if f.PostID != v.Scope.ID {
			return nil, &viewcontext.InvalidError{Field: "from.key", Reason: "the finding is about another post"}
		}
		return f, nil
	}
	return nil, &viewcontext.InvalidError{Field: "from.key", Reason: "no review in the campaign's thread has this finding"}
}

// findIn is the post finding keyed key in a model turn's stored reply: in
// any review's findings or dismissed findings, in the turn's results or
// the result of an answer. nil when it has none.
func findIn(content, key string) *viewcontext.Finding {
	var reply any
	if json.Unmarshal([]byte(content), &reply) != nil {
		return nil
	}
	var found *viewcontext.Finding
	var walk func(any)
	walk = func(n any) {
		switch n := n.(type) {
		case map[string]any:
			for _, list := range []string{"findings", "dismissedFindings"} {
				items, _ := n[list].([]any)
				for _, it := range items {
					if f := postFinding(it, key); f != nil && found == nil {
						found = f
					}
				}
			}
			for _, child := range n {
				walk(child)
			}
		case []any:
			for _, child := range n {
				walk(child)
			}
		}
	}
	walk(reply)
	return found
}

// postFinding is item as a finding when it is a post finding keyed key.
func postFinding(item any, key string) *viewcontext.Finding {
	m, _ := item.(map[string]any)
	if m["key"] != key || m["kind"] != "post" {
		return nil
	}
	str := func(k string) string { s, _ := m[k].(string); return s }
	return &viewcontext.Finding{Key: key, PostID: str("postId"), Title: str("title"), Severity: str("severity"),
		Issue: str("issue"), Quote: str("quote"), Suggestion: str("suggestion")}
}
