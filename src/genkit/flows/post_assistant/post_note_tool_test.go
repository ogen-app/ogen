package post_assistant

import (
	"context"
	"strings"
	"testing"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/usecase/notes"
)

type captureNoteRepo struct {
	repository.PostNoteRepository
	created []*models.PostNote
}

func (r *captureNoteRepo) Create(_ context.Context, n *models.PostNote) error {
	r.created = append(r.created, n)
	return nil
}

func TestToolCreateNote(t *testing.T) {
	repo := &captureNoteRepo{}
	st := &requestState{postID: "post1", actor: "user1", noteSvc: notes.New(repo)}
	ctx := withRequestState(t.Context(), st)

	out, err := toolCreateNote(ctx, CreateNoteInput{Type: "image_prompt", Title: "Prompt", Body: "a photorealistic banana"})
	if err != nil {
		t.Fatalf("toolCreateNote: %v", err)
	}
	if out.ID == "" || out.Type != string(models.PostNoteTypeImagePrompt) {
		t.Errorf("unexpected output: %+v", out)
	}

	// draft_thesis is reserved for content-plan; the tool must downgrade it to
	// a free-form note rather than create a draft thesis.
	if _, err := toolCreateNote(ctx, CreateNoteInput{Type: "draft_thesis", Body: "should become note"}); err != nil {
		t.Fatalf("toolCreateNote (reserved type): %v", err)
	}

	if len(repo.created) != 2 {
		t.Fatalf("expected 2 notes persisted, got %d", len(repo.created))
	}
	first, second := repo.created[0], repo.created[1]
	if first.Type != models.PostNoteTypeImagePrompt {
		t.Errorf("first note type = %q, want image_prompt", first.Type)
	}
	if second.Type != models.PostNoteTypeNote {
		t.Errorf("reserved draft_thesis note type = %q, want note", second.Type)
	}
	for _, n := range repo.created {
		if n.Origin != models.PostNoteOriginAssistant {
			t.Errorf("origin = %q, want assistant", n.Origin)
		}
		if n.PostID != "post1" || n.CreatedBy != "user1" {
			t.Errorf("note not stamped with request state: %+v", n)
		}
	}
	if len(st.noteResults) != 2 {
		t.Errorf("noteResults = %d, want 2 (read by the runner to finalise the response)", len(st.noteResults))
	}
}

func TestToolCreateNote_Unavailable(t *testing.T) {
	st := &requestState{postID: "post1", actor: "user1"} // noteSvc nil
	ctx := withRequestState(t.Context(), st)
	if _, err := toolCreateNote(ctx, CreateNoteInput{Type: "note", Body: "x"}); err == nil {
		t.Fatal("expected an error when the note service is unavailable")
	}
}

type listNoteRepo struct {
	repository.PostNoteRepository
	notes []models.PostNote
}

func (r *listNoteRepo) ListByPostID(_ context.Context, _ string) ([]models.PostNote, error) {
	return r.notes, nil
}

// buildNoteSummaries must bound the aggregate note context (count cap) while
// keeping the repository order — the pinned draft_thesis stays first and is
// never the entry dropped when the limit is hit.
func TestBuildNoteSummaries_LimitAndOrdering(t *testing.T) {
	list := []models.PostNote{{Type: models.PostNoteTypeDraftThesis, Body: "the pinned thesis"}}
	for i := 0; i < maxNotesInContext+15; i++ {
		list = append(list, models.PostNote{Type: models.PostNoteTypeNote, Body: strings.Repeat("x", 10)})
	}
	repos := PostAssistantRepos{Notes: &listNoteRepo{notes: list}}

	out, err := buildNoteSummaries(t.Context(), &models.Post{ID: "p1"}, repos)
	if err != nil {
		t.Fatalf("buildNoteSummaries: %v", err)
	}
	if len(out) != maxNotesInContext {
		t.Errorf("len(out) = %d, want the count cap %d", len(out), maxNotesInContext)
	}
	if out[0].Type != string(models.PostNoteTypeDraftThesis) || out[0].Body != "the pinned thesis" {
		t.Errorf("draft_thesis must stay first, got %+v", out[0])
	}
}
