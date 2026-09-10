package notes

import (
	"context"
	"errors"
	"testing"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
)

// stubRepo embeds the interface so it satisfies the type; only Create is
// implemented for these validation tests.
type stubRepo struct {
	repository.PostNoteRepository
	created *models.PostNote
}

func (s *stubRepo) Create(_ context.Context, n *models.PostNote) error {
	s.created = n
	return nil
}

func TestCreate_ValidDefaultsOriginAndTrims(t *testing.T) {
	repo := &stubRepo{}
	svc := New(repo)

	note, err := svc.Create(t.Context(), CreateInput{
		PostID:    "post1",
		Type:      models.PostNoteTypeNote,
		Title:     "  Title  ",
		Body:      "  body  ",
		CreatedBy: "user1",
		// Origin intentionally empty → should default to manual.
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if note.ID == "" {
		t.Error("expected a non-empty id")
	}
	if note.Origin != models.PostNoteOriginManual {
		t.Errorf("origin = %q, want manual", note.Origin)
	}
	if note.Title != "Title" || note.Body != "body" {
		t.Errorf("title/body not trimmed: %q / %q", note.Title, note.Body)
	}
	if note.CreatedAt.IsZero() || note.UpdatedAt.IsZero() {
		t.Error("expected timestamps to be stamped")
	}
	if repo.created == nil {
		t.Error("expected the note to be persisted")
	}
}

func TestCreate_Validation(t *testing.T) {
	svc := New(&stubRepo{})

	if _, err := svc.Create(t.Context(), CreateInput{PostID: "p", Type: "bogus", Body: "x"}); !errors.Is(err, ErrInvalidType) {
		t.Errorf("invalid type: got %v, want ErrInvalidType", err)
	}
	if _, err := svc.Create(t.Context(), CreateInput{PostID: "p", Type: models.PostNoteTypeNote, Body: "   "}); !errors.Is(err, ErrEmptyBody) {
		t.Errorf("empty body: got %v, want ErrEmptyBody", err)
	}

	if !IsValidation(ErrInvalidType) || !IsValidation(ErrEmptyBody) {
		t.Error("IsValidation should recognize the validation sentinels")
	}
	if IsValidation(errors.New("other")) {
		t.Error("IsValidation should not match unrelated errors")
	}
}

func TestUpdate_AppliesPatchAndValidates(t *testing.T) {
	repo := &stubRepo{}
	// Make Update report a matched row.
	svc := New(&updatingStubRepo{stubRepo: repo})

	existing := &models.PostNote{ID: "n1", PostID: "p1", Type: models.PostNoteTypeNote, Body: "old"}
	newBody := "new body"
	newType := models.PostNoteTypeImagePrompt
	updated, err := svc.Update(t.Context(), existing, UpdateInput{Body: &newBody, Type: &newType})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if updated.Body != "new body" || updated.Type != models.PostNoteTypeImagePrompt {
		t.Errorf("patch not applied: %+v", updated)
	}

	// Patching to an empty body is rejected.
	empty := "   "
	if _, err := svc.Update(t.Context(), existing, UpdateInput{Body: &empty}); !errors.Is(err, ErrEmptyBody) {
		t.Errorf("empty body patch: got %v, want ErrEmptyBody", err)
	}
}

// updatingStubRepo reports a matched row on Update so Service.Update succeeds.
type updatingStubRepo struct {
	*stubRepo
}

func (s *updatingStubRepo) Update(_ context.Context, n *models.PostNote) (bool, error) {
	return true, nil
}
