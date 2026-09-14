package clone

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
)

// stubAttRepo is a minimal PostAttachmentRepository for copyAttachments
// tests — only ListByPostID is exercised.
type stubAttRepo struct{ atts []models.PostAttachment }

func (s stubAttRepo) ListByPostID(context.Context, string) ([]models.PostAttachment, error) {
	return s.atts, nil
}
func (s stubAttRepo) ListS3KeysByPostID(context.Context, string) ([]string, error) { return nil, nil }
func (s stubAttRepo) GetByID(context.Context, string) (*models.PostAttachment, error) {
	return nil, nil
}
func (s stubAttRepo) CreateAtNextPosition(context.Context, *models.PostAttachment) error { return nil }
func (s stubAttRepo) UpdatePosition(context.Context, string, int) error                  { return nil }
func (s stubAttRepo) UpdateAltText(context.Context, string, string) error                { return nil }
func (s stubAttRepo) UpdateSegmentIndex(context.Context, string, *int) error             { return nil }
func (s stubAttRepo) ReorderPositions(context.Context, string, []string) error           { return nil }
func (s stubAttRepo) Delete(context.Context, string) (bool, error)                       { return false, nil }
func (s stubAttRepo) SumSizeBytesInTenant(context.Context) (int64, error)                { return 0, nil }

func TestCopyAttachments_NoStorage(t *testing.T) {
	ctx := t.Context()

	t.Run("attachments with keys but no storage → error", func(t *testing.T) {
		s := &Service{
			store:       nil,
			attachments: stubAttRepo{atts: []models.PostAttachment{{ID: "a", S3Key: "post-attachments/x/a.png"}}},
		}
		_, _, err := s.copyAttachments(ctx, "src", "new", DefaultOptions("u", TriggerAPI), time.Time{}, false)
		if !errors.Is(err, ErrStorageUnavailable) {
			t.Fatalf("want ErrStorageUnavailable, got %v", err)
		}
	})

	t.Run("no attachments → no error", func(t *testing.T) {
		s := &Service{store: nil, attachments: stubAttRepo{}}
		atts, _, err := s.copyAttachments(ctx, "src", "new", DefaultOptions("u", TriggerAPI), time.Time{}, false)
		if err != nil || len(atts) != 0 {
			t.Fatalf("want no error and no atts, got atts=%d err=%v", len(atts), err)
		}
	})

	t.Run("CopyMedia=false skips entirely", func(t *testing.T) {
		opts := DefaultOptions("u", TriggerAPI)
		opts.CopyMedia = false
		s := &Service{store: nil, attachments: stubAttRepo{atts: []models.PostAttachment{{ID: "a", S3Key: "k"}}}}
		if _, _, err := s.copyAttachments(ctx, "src", "new", opts, time.Time{}, false); err != nil {
			t.Fatalf("CopyMedia=false should skip, got %v", err)
		}
	})
}

func TestDefaultPostType(t *testing.T) {
	tests := []struct {
		name  string
		types models.PostTypeMap
		want  string
	}{
		{"prefers text-post", models.PostTypeMap{"video": "V", "text-post": "T", "image-post": "I"}, "text-post"},
		{"first slug when no text-post", models.PostTypeMap{"video": "V", "image-post": "I"}, "image-post"},
		{"empty map", models.PostTypeMap{}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := defaultPostType(tt.types); got != tt.want {
				t.Fatalf("defaultPostType() = %q, want %q", got, tt.want)
			}
		})
	}
}
