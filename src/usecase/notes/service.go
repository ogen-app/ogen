// Package notes implements the shared "per-post Note" operations (CON-188).
// It is the single source of truth for note writes used by the REST CRUD
// (/api/posts/:post_id/notes) and the Post Assistant's createNote tool, so the
// two entry points can never drift on validation or origin stamping. The
// content-plan flow writes its draft_thesis notes through the repository
// directly (trusted, no user input to validate).
package notes

import (
	"cmp"
	"context"
	"errors"
	"strings"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
)

// Length bounds guard the unbounded TEXT columns against abuse; they are
// generous relative to any real note.
const (
	MaxTitleLen = 200
	MaxBodyLen  = 50000
)

// Validation errors. Callers (the REST handler) map these to HTTP 400.
var (
	ErrEmptyBody    = errors.New("note body is required")
	ErrInvalidType  = errors.New("invalid note type")
	ErrTitleTooLong = errors.New("note title is too long")
	ErrBodyTooLong  = errors.New("note body is too long")
)

// ErrNotFound is returned by Update when no note matches (unknown id or
// cross-tenant). Handlers map it to 404.
var ErrNotFound = errors.New("note not found")

// IsValidation reports whether err is one of the note validation errors, so a
// handler can return 400 without enumerating each sentinel.
func IsValidation(err error) bool {
	return errors.Is(err, ErrEmptyBody) ||
		errors.Is(err, ErrInvalidType) ||
		errors.Is(err, ErrTitleTooLong) ||
		errors.Is(err, ErrBodyTooLong)
}

// Service persists notes with shared validation + origin stamping.
type Service struct {
	repo repository.PostNoteRepository
}

// New returns a note service over the given repository.
func New(repo repository.PostNoteRepository) *Service {
	return &Service{repo: repo}
}

// CreateInput is the data needed to create a note. Origin defaults to manual
// when empty. Title/Body are trimmed before validation.
type CreateInput struct {
	PostID    string
	Type      models.PostNoteType
	Title     string
	Body      string
	Origin    models.PostNoteOrigin
	CreatedBy string
}

// Create validates the input, stamps id/origin/timestamps, and persists the
// note. The parent-post existence check is left to the caller (the REST handler
// loads the post first; a mid-write post deletion trips the post_id FK).
func (s *Service) Create(ctx context.Context, in CreateInput) (*models.PostNote, error) {
	title := strings.TrimSpace(in.Title)
	body := strings.TrimSpace(in.Body)
	if err := validateFields(in.Type, title, body); err != nil {
		return nil, err
	}

	origin := in.Origin
	origin = cmp.Or(origin, models.PostNoteOriginManual)

	id, err := models.NewID()
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	note := &models.PostNote{
		ID:        id,
		PostID:    in.PostID,
		Type:      in.Type,
		Title:     title,
		Body:      body,
		Origin:    origin,
		CreatedBy: in.CreatedBy,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := s.repo.Create(ctx, note); err != nil {
		return nil, err
	}
	return note, nil
}

// UpdateInput carries the partial edits for a note. A nil field is left
// unchanged.
type UpdateInput struct {
	Title *string
	Body  *string
	Type  *models.PostNoteType
}

// Update applies the patch to the already-loaded note, validates the result,
// and persists it. Returns the updated note. existing must be a note the caller
// has already fetched and authorized (its post scope checked).
func (s *Service) Update(ctx context.Context, existing *models.PostNote, in UpdateInput) (*models.PostNote, error) {
	if in.Title != nil {
		existing.Title = strings.TrimSpace(*in.Title)
	}
	if in.Body != nil {
		existing.Body = strings.TrimSpace(*in.Body)
	}
	if in.Type != nil {
		existing.Type = *in.Type
	}
	if err := validateFields(existing.Type, existing.Title, existing.Body); err != nil {
		return nil, err
	}
	ok, err := s.repo.Update(ctx, existing)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrNotFound
	}
	return existing, nil
}

// List returns a post's notes, draft_thesis first then oldest-first (CON-188).
func (s *Service) List(ctx context.Context, postID string) ([]models.PostNote, error) {
	return s.repo.ListByPostID(ctx, postID)
}

// Get returns a single note by id.
func (s *Service) Get(ctx context.Context, id string) (*models.PostNote, error) {
	return s.repo.GetByID(ctx, id)
}

// Delete removes a note by id, reporting whether a row was deleted.
func (s *Service) Delete(ctx context.Context, id string) (bool, error) {
	return s.repo.Delete(ctx, id)
}

func validateFields(t models.PostNoteType, title, body string) error {
	if !t.Valid() {
		return ErrInvalidType
	}
	if body == "" {
		return ErrEmptyBody
	}
	if len([]rune(title)) > MaxTitleLen {
		return ErrTitleTooLong
	}
	if len([]rune(body)) > MaxBodyLen {
		return ErrBodyTooLong
	}
	return nil
}
