package assistantundo_test

import (
	"errors"
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/usecase/assistantundo"
)

func undoPost(t *testing.T, f *fixture, posts repository.PostRepository, id string, at *time.Time, status models.PostStatus, postType string) {
	t.Helper()
	p := &models.Post{ID: id, CampaignID: "c1", Title: id, MediaURLs: models.StringSlice{}, UsedAssetIDs: models.StringSlice{},
		Status: status, PlatformPostType: postType, CTAType: models.CTATypeNone, CreatedBy: "user-1", ScheduledAt: at}
	if err := posts.Create(f.ctx, p); err != nil {
		t.Fatal(err)
	}
}

// Undoing an unschedule dates every post again, or none of them when one
// was dated since.
func TestUndo_PostSchedules(t *testing.T) {
	f := newFixture(t)
	f.seedCampaign(t, nil)
	posts := repository.NewPostRepository(f.db)
	svc := assistantundo.New(f.db, f.actions, f.campaigns).WithPosts(posts)
	at := time.Date(2027, 3, 2, 9, 0, 0, 0, time.UTC)
	undoPost(t, f, posts, "a", nil, models.PostStatusDraft, "")
	undoPost(t, f, posts, "b", nil, models.PostStatusDraft, "")
	log := func() string {
		before := []assistantundo.PostState{{ID: "a", ScheduledAt: &at}, {ID: "b", ScheduledAt: &at}}
		id, err := assistantundo.Record(f.ctx, f.actions, &models.AssistantAction{ScopeKind: models.AssistantScopeCampaign, ScopeID: "c1",
			Kind: models.AssistantActionPostSchedules, EntityID: "c1", Before: assistantundo.Posts(before),
			After: assistantundo.Posts([]assistantundo.PostState{{ID: "a"}, {ID: "b"}}), CreatedBy: "user-1"})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}

	other := at.Add(time.Hour)
	if _, err := posts.SetScheduleIf(f.ctx, nil, []repository.ScheduleChange{{PostID: "b", To: &other}}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Undo(f.ctx, log(), "user-1"); !errors.Is(err, assistantundo.ErrChangedSince) {
		t.Fatalf("undo over a later edit = %v", err)
	}
	if p, _ := posts.GetByID(f.ctx, "a"); p.ScheduledAt != nil {
		t.Fatalf("a was dated although the undo failed: %v", p.ScheduledAt)
	}

	if _, err := posts.SetScheduleIf(f.ctx, nil, []repository.ScheduleChange{{PostID: "b", From: &other}}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Undo(f.ctx, log(), "user-1"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"a", "b"} {
		if p, _ := posts.GetByID(f.ctx, id); p.ScheduledAt == nil || !p.ScheduledAt.Equal(at) {
			t.Fatalf("%s dated %v, want %v", id, p.ScheduledAt, at)
		}
	}
}

// Undoing a type change gives the post its type and status back.
func TestUndo_PostTypes(t *testing.T) {
	f := newFixture(t)
	f.seedCampaign(t, nil)
	posts := repository.NewPostRepository(f.db)
	svc := assistantundo.New(f.db, f.actions, f.campaigns).WithPosts(posts)
	undoPost(t, f, posts, "a", nil, models.PostStatusDraft, "text")
	id, err := assistantundo.Record(f.ctx, f.actions, &models.AssistantAction{ScopeKind: models.AssistantScopeCampaign, ScopeID: "c1",
		Kind: models.AssistantActionPostTypes, EntityID: "c1",
		Before: assistantundo.Posts([]assistantundo.PostState{{ID: "a", PostType: "video", Status: models.PostStatusReadyForPublish}}),
		After:  assistantundo.Posts([]assistantundo.PostState{{ID: "a", PostType: "text", Status: models.PostStatusDraft}}), CreatedBy: "user-1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Undo(f.ctx, id, "user-1"); err != nil {
		t.Fatal(err)
	}
	if p, _ := posts.GetByID(f.ctx, "a"); p.PlatformPostType != "video" || p.Status != models.PostStatusReadyForPublish {
		t.Fatalf("a = %s %s", p.PlatformPostType, p.Status)
	}
}
