package content_plan

import (
	"context"
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
)

// CON-114: the streaming persist path stops at the batch's requested count, so
// an over-producing model can't turn "generate exactly 1" into 3 persisted
// posts. expectedCount <= 0 is the uncapped fallback (model decides the count).
func TestWithinCount(t *testing.T) {
	cases := []struct {
		name                string
		persisted, expected int
		want                bool
	}{
		{"first post for a 1-post batch", 0, 1, true},
		{"second post exceeds a 1-post batch", 1, 1, false},
		{"under a 3-post batch", 2, 3, true},
		{"at a 3-post batch cap", 3, 3, false},
		{"uncapped (zero)", 5, 0, true},
		{"uncapped (negative)", 5, -1, true},
	}
	for _, c := range cases {
		if got := withinCount(c.persisted, c.expected); got != c.want {
			t.Errorf("%s: withinCount(%d, %d) = %v, want %v", c.name, c.persisted, c.expected, got, c.want)
		}
	}
}

// stubPostRepo embeds the interface so it satisfies the type; only Create is
// implemented (the rest panic if unexpectedly called).
type stubPostRepo struct {
	repository.PostRepository
	created *models.Post
}

func (s *stubPostRepo) Create(_ context.Context, p *models.Post) error {
	s.created = p
	return nil
}

type stubNoteRepo struct {
	repository.PostNoteRepository
	created *models.PostNote
}

func (s *stubNoteRepo) Create(_ context.Context, n *models.PostNote) error {
	s.created = n
	return nil
}

// CON-188: content-plan no longer writes the thesis into the post body — the
// post is created with an empty body and the thesis is captured as a
// draft_thesis note (origin content_plan, authored by the campaign owner).
func TestPersistOne_DraftThesisNote(t *testing.T) {
	postRepo := &stubPostRepo{}
	noteRepo := &stubNoteRepo{}
	campaign := &models.Campaign{ID: "camp1", CreatedBy: "user1"}
	dp := DraftPost{Title: "T", Body: "- point 1\n- point 2", PlatformID: "linkedin", ContentType: "article"}

	id, err := persistOne(t.Context(), &dp, campaign, campaign.StartDate, campaign.EndDate, postRepo, noteRepo, nil)
	if err != nil {
		t.Fatalf("persistOne: %v", err)
	}
	if id == "" {
		t.Fatal("expected a non-empty post id")
	}
	if postRepo.created == nil {
		t.Fatal("expected a post to be created")
	}
	if postRepo.created.Content != "" {
		t.Errorf("post body = %q, want empty (thesis goes to a note)", postRepo.created.Content)
	}
	if noteRepo.created == nil {
		t.Fatal("expected a draft_thesis note to be created")
	}
	n := noteRepo.created
	if n.Type != models.PostNoteTypeDraftThesis {
		t.Errorf("note type = %q, want draft_thesis", n.Type)
	}
	if n.Origin != models.PostNoteOriginContentPlan {
		t.Errorf("note origin = %q, want content_plan", n.Origin)
	}
	if n.Body != dp.Body {
		t.Errorf("note body = %q, want %q", n.Body, dp.Body)
	}
	if n.PostID != id {
		t.Errorf("note post_id = %q, want %q", n.PostID, id)
	}
	if n.CreatedBy != "user1" {
		t.Errorf("note created_by = %q, want user1", n.CreatedBy)
	}
}

// An empty thesis creates no note (but the post is still created).
func TestPersistOne_EmptyThesisNoNote(t *testing.T) {
	postRepo := &stubPostRepo{}
	noteRepo := &stubNoteRepo{}
	campaign := &models.Campaign{ID: "camp1", CreatedBy: "user1"}
	dp := DraftPost{Title: "T", Body: "   ", PlatformID: "linkedin", ContentType: "article"}

	if _, err := persistOne(t.Context(), &dp, campaign, campaign.StartDate, campaign.EndDate, postRepo, noteRepo, nil); err != nil {
		t.Fatalf("persistOne: %v", err)
	}
	if postRepo.created == nil {
		t.Fatal("expected a post to be created")
	}
	if noteRepo.created != nil {
		t.Errorf("expected no note for an empty thesis, got %+v", noteRepo.created)
	}
}

// CON-181: persistOne composes scheduled_at from the campaign's scheduling
// settings — the model's date placed at the publishing time in the campaign
// timezone — and reflects the (unchanged, enabled-day) date back onto dp.
func TestPersistOne_ComposesScheduledAt(t *testing.T) {
	postRepo := &stubPostRepo{}
	start := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC)
	campaign := &models.Campaign{
		ID: "camp1", CreatedBy: "user1",
		PublishingTime: "09:00",
		Timezone:       "", // UTC
		PublishingDays: models.StringSlice{"mon", "tue", "wed", "thu", "fri", "sat", "sun"},
		SpreadMinutes:  0,
		StartDate:      &start,
		EndDate:        &end,
	}
	dp := DraftPost{Title: "T", Body: "x", PlatformID: "linkedin", ContentType: "article", PublishDate: "2026-08-12"}

	if _, err := persistOne(t.Context(), &dp, campaign, campaign.StartDate, campaign.EndDate, postRepo, nil, nil); err != nil {
		t.Fatalf("persistOne: %v", err)
	}
	want := time.Date(2026, 8, 12, 9, 0, 0, 0, time.UTC)
	if postRepo.created.ScheduledAt == nil || !postRepo.created.ScheduledAt.Equal(want) {
		t.Errorf("scheduled_at = %v, want %v", postRepo.created.ScheduledAt, want)
	}
	if dp.PublishDate != "2026-08-12" {
		t.Errorf("dp.PublishDate = %q, want 2026-08-12 (reflected)", dp.PublishDate)
	}
}

// A nil note repo must not panic — the post is still created (note skipped).
func TestPersistOne_NilNoteRepo(t *testing.T) {
	postRepo := &stubPostRepo{}
	campaign := &models.Campaign{ID: "camp1", CreatedBy: "user1"}
	dp := DraftPost{Title: "T", Body: "- point 1", PlatformID: "linkedin", ContentType: "article"}

	if _, err := persistOne(t.Context(), &dp, campaign, campaign.StartDate, campaign.EndDate, postRepo, nil, nil); err != nil {
		t.Fatalf("persistOne: %v", err)
	}
	if postRepo.created == nil {
		t.Fatal("expected a post to be created even with a nil note repo")
	}
}

// CON-114: day-snapping is bounded by the window passed to persistOne (the
// targeting window), not the full campaign window — so a targeted run can't snap
// a post onto an enabled day outside the requested window.
func TestPersistOne_SnapsWithinWindow(t *testing.T) {
	postRepo := &stubPostRepo{}
	campStart := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	campEnd := time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC)
	date := time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC)

	// Enable only the weekday AFTER the model's date, so the date itself is on a
	// disabled day and the nearest enabled day is date+1 — inside the campaign
	// window but outside the single-day targeting window below.
	tok := map[time.Weekday]string{
		time.Sunday: "sun", time.Monday: "mon", time.Tuesday: "tue", time.Wednesday: "wed",
		time.Thursday: "thu", time.Friday: "fri", time.Saturday: "sat",
	}
	campaign := &models.Campaign{
		ID: "camp1", CreatedBy: "user1",
		PublishingTime: "09:00", Timezone: "",
		PublishingDays: models.StringSlice{tok[(date.Weekday()+1)%7]},
		SpreadMinutes:  0,
		StartDate:      &campStart, EndDate: &campEnd,
	}
	dp := DraftPost{Title: "T", Body: "x", PlatformID: "linkedin", ContentType: "article", PublishDate: "2026-08-12"}

	// Targeting window is the single (disabled) day: no enabled day inside it, so
	// the date is kept rather than snapped forward to date+1. With the full
	// campaign window it would snap to 2026-08-13.
	win := date
	if _, err := persistOne(t.Context(), &dp, campaign, &win, &win, postRepo, nil, nil); err != nil {
		t.Fatalf("persistOne: %v", err)
	}
	want := time.Date(2026, 8, 12, 9, 0, 0, 0, time.UTC)
	if postRepo.created.ScheduledAt == nil || !postRepo.created.ScheduledAt.Equal(want) {
		t.Errorf("scheduled_at = %v, want %v (kept in-window, not snapped to date+1)", postRepo.created.ScheduledAt, want)
	}
	if dp.PublishDate != "2026-08-12" {
		t.Errorf("dp.PublishDate = %q, want 2026-08-12 (not snapped out of window)", dp.PublishDate)
	}
}
