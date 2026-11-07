package clone

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
)

// stubAttRepo is a minimal PostAttachmentRepository for copyAttachments
// tests — only ListByPostID is exercised.
type stubAttRepo struct{ atts []models.PostAttachment }

func (s stubAttRepo) ListByPostID(context.Context, string) ([]models.PostAttachment, error) {
	return s.atts, nil
}
func (s stubAttRepo) ListByPostIDs(context.Context, []string) ([]models.PostAttachment, error) {
	return nil, nil
}
func (s stubAttRepo) CoverKeysByPostIDs(context.Context, []string) (map[string]string, error) {
	return nil, nil
}
func (s stubAttRepo) ListS3KeysByPostID(context.Context, string) ([]string, error) { return nil, nil }
func (s stubAttRepo) GetByID(context.Context, string) (*models.PostAttachment, error) {
	return nil, nil
}
func (s stubAttRepo) CreateAtNextPosition(context.Context, *models.PostAttachment) error { return nil }
func (s stubAttRepo) CreateFromPendingUpload(context.Context, *models.PostAttachment) error {
	return nil
}
func (s stubAttRepo) Patch(context.Context, string, repository.AttachmentPatch) error    { return nil }
func (s stubAttRepo) SetGeneratedAltText(context.Context, string, string) error          { return nil }
func (s stubAttRepo) SetPublisherMedia(context.Context, string, string, time.Time) error { return nil }
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

// A clone keeps the source's grounding, verbatim or adapted for another
// platform; a hand-written source leaves it unset.
func TestBuildClone_KeepsGrounding(t *testing.T) {
	grounded := models.GroundingGrounded
	src := &models.Post{ID: "src", PlatformID: "pl1", Content: "body", GroundingMode: &grounded}
	other := target{platformID: "pl2", platform: &models.Platform{ID: "pl2", Name: "Threads"}}
	for name, tc := range map[string]struct {
		tgt     target
		content string
	}{
		"verbatim": {target{platformID: "pl1"}, "body"},
		"adapted":  {other, "adapted body"},
	} {
		t.Run(name, func(t *testing.T) {
			c := buildClone(src, tc.tgt, DefaultOptions("u", TriggerAPI), "new", tc.content, false, time.Time{})
			if c.GroundingMode == nil || *c.GroundingMode != grounded || c.GroundingMode == src.GroundingMode {
				t.Fatalf("grounding = %v, want its own copy of %s", c.GroundingMode, grounded)
			}
		})
	}
	src.GroundingMode = nil
	if c := buildClone(src, target{platformID: "pl1"}, DefaultOptions("u", TriggerAPI), "new", "body", false, time.Time{}); c.GroundingMode != nil {
		t.Fatalf("grounding = %v, want nil", *c.GroundingMode)
	}
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

func TestCloneStatus(t *testing.T) {
	cases := []struct {
		src     models.PostStatus
		content string
		want    models.PostStatus
	}{
		{models.PostStatusPlanned, "", models.PostStatusPlanned},
		{models.PostStatusPlanned, "body", models.PostStatusDraft},
		{models.PostStatusPublished, "body", models.PostStatusDraft},
		{models.PostStatusDraft, "", models.PostStatusDraft},
	}
	for _, tc := range cases {
		if got := cloneStatus(tc.src, tc.content); got != tc.want {
			t.Errorf("clone of %s with %q = %s, want %s", tc.src, tc.content, got, tc.want)
		}
	}
}
