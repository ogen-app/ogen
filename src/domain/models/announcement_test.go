package models_test

import (
	"testing"

	"github.com/ogen-app/ogen/src/domain/models"
)

func TestAnnouncementStatusValid(t *testing.T) {
	valid := []models.AnnouncementStatus{
		models.AnnouncementStatusDraft,
		models.AnnouncementStatusPublished,
		models.AnnouncementStatusArchived,
	}
	for _, s := range valid {
		if !s.Valid() {
			t.Errorf("status %q should be valid", s)
		}
	}
	for _, s := range []models.AnnouncementStatus{"", "live", "deleted", "Published"} {
		if s.Valid() {
			t.Errorf("status %q should be invalid", s)
		}
	}
}
