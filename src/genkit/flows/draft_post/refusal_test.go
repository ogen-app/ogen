package draft_post

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/firebase/genkit/go/ai"
	"github.com/firebase/genkit/go/genkit"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/flowkit"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/infra/vendors/llm"
)

// A refused stream must not fall back to a blocking call: the retry would be
// refused, and billed, again.
func TestGenerate_RefusalSkipsFallback(t *testing.T) {
	calls := 0
	g := genkit.Init(t.Context())
	genkit.DefineModel(g, "test/refuser", &ai.ModelOptions{Supports: &ai.ModelSupports{Multiturn: true}},
		func(context.Context, *ai.ModelRequest, ai.ModelStreamCallback) (*ai.ModelResponse, error) {
			calls++
			return &ai.ModelResponse{Message: ai.NewModelTextMessage(""), FinishReason: ai.FinishReasonUnknown}, nil
		})

	err := (&draftSink{}).generate(t.Context(), g, flowkit.Usage{},
		ai.WithModelName("test/refuser"),
		ai.WithPrompt("go"),
		ai.WithMiddleware(llm.NewProvider().CallMiddleware("draft_post", nil)),
	)
	if _, ok := errors.AsType[*AIError](err); !ok {
		t.Fatalf("err = %v, want *AIError", err)
	}
	if calls != 1 {
		t.Fatalf("model called %d times, want 1", calls)
	}
}

type savedPosts struct {
	repository.PostRepository
	rows []*models.Post
}

func (r *savedPosts) Create(_ context.Context, p *models.Post) error {
	r.rows = append(r.rows, p)
	return nil
}

// A refusal after a draft was saved keeps that draft in the result, with a
// warning, so the caller still links and reports it.
func TestGenerate_RefusalKeepsSavedDrafts(t *testing.T) {
	g := genkit.Init(t.Context())
	genkit.DefineModel(g, "test/half-refuser", &ai.ModelOptions{Supports: &ai.ModelSupports{Multiturn: true}},
		func(_ context.Context, _ *ai.ModelRequest, cb ai.ModelStreamCallback) (*ai.ModelResponse, error) {
			if cb != nil {
				if err := cb(t.Context(), &ai.ModelResponseChunk{Content: []*ai.Part{ai.NewTextPart(`[{"title":"T","content":"Body"},`)}}); err != nil {
					return nil, err
				}
			}
			return &ai.ModelResponse{Message: ai.NewModelTextMessage(""), FinishReason: ai.FinishReasonUnknown}, nil
		})

	posts := &savedPosts{}
	sink := &draftSink{
		campaign: &models.Campaign{ID: "c1", CreatedBy: "owner"},
		dates:    []string{"2026-12-01", "2026-12-02"},
		loc:      time.UTC,
		author:   "user-2",
		repos:    DraftPostRepos{Posts: posts},
	}
	err := sink.generate(t.Context(), g, flowkit.Usage{},
		ai.WithModelName("test/half-refuser"),
		ai.WithPrompt("go"),
		ai.WithMiddleware(llm.NewProvider().CallMiddleware("draft_post", nil)),
	)
	if err != nil {
		t.Fatalf("err = %v, want the saved draft kept", err)
	}
	if len(sink.out) != 1 || len(sink.warnings) != 1 || len(posts.rows) != 1 {
		t.Fatalf("out = %+v, warnings = %v, rows = %d", sink.out, sink.warnings, len(posts.rows))
	}
	if posts.rows[0].CreatedBy != "user-2" {
		t.Fatalf("created by %q, want the drafting user", posts.rows[0].CreatedBy)
	}
}
