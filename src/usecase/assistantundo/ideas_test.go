package assistantundo_test

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/usecase/assistantundo"
)

// ideaFixture stores a second user, user-2, and idea, and returns the idea
// repository and the service that undoes its verdicts.
func ideaFixture(t *testing.T, f *fixture, idea *models.Idea) (repository.IdeaRepository, *assistantundo.Service) {
	t.Helper()
	for _, m := range []any{
		&models.Account{ID: "acc-2", Email: "sam@example.com", Name: "Sam"},
		&models.User{ID: "user-2", AccountID: "acc-2", TenantID: models.DefaultTenantID, Name: "Sam", Email: "sam@example.com", Role: models.RoleMember},
	} {
		if _, err := f.db.NewInsert().Model(m).Exec(f.ctx); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	ideas := repository.NewIdeaRepository(f.db)
	if err := ideas.Create(f.ctx, idea); err != nil {
		t.Fatal(err)
	}
	return ideas, assistantundo.New(f.db, f.actions, f.campaigns).WithIdeas(ideas)
}

// logVerdict applies change to the stored idea and logs it, as
// setIdeaVerdict does.
func logVerdict(t *testing.T, f *fixture, ideas repository.IdeaRepository, change func(*models.Idea)) string {
	t.Helper()
	i := loadIdea(t, f, ideas)
	before := assistantundo.IdeaVerdict(i)
	change(i)
	setTriage(t, f, ideas, i)
	id, err := assistantundo.Record(f.ctx, f.actions, &models.AssistantAction{ScopeKind: models.AssistantScopeWorkspace,
		Kind: models.AssistantActionIdeaVerdict, EntityID: i.ID, Before: before, After: assistantundo.IdeaVerdict(i), CreatedBy: "user-1"})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func loadIdea(t *testing.T, f *fixture, ideas repository.IdeaRepository) *models.Idea {
	t.Helper()
	i, err := ideas.GetByID(f.ctx, "i1")
	if err != nil {
		t.Fatal(err)
	}
	return i
}

func setTriage(t *testing.T, f *fixture, ideas repository.IdeaRepository, i *models.Idea) {
	t.Helper()
	if _, err := ideas.Update(f.ctx, i, "verdict", "remind_at", "decided_at", "decided_by"); err != nil {
		t.Fatal(err)
	}
}

func TestUndo_IdeaVerdict(t *testing.T) {
	f := newFixture(t)
	ideas, svc := ideaFixture(t, f, &models.Idea{ID: "i1", Title: "Pricing myths"})
	decided := time.Now().UTC().Truncate(time.Microsecond)
	id := logVerdict(t, f, ideas, func(i *models.Idea) {
		i.Verdict, i.DecidedAt, i.DecidedBy = new(models.IdeaVerdictYes), &decided, new("user-1")
	})

	// Changed again since: refused, and not recorded.
	i := loadIdea(t, f, ideas)
	i.Verdict = new(models.IdeaVerdictNo)
	setTriage(t, f, ideas, i)
	if _, err := svc.Undo(f.ctx, id, "user-1"); !errors.Is(err, assistantundo.ErrChangedSince) || f.undone(t, id) {
		t.Fatalf("changed since: %v", err)
	}
	// The same verdict decided again by someone else is a change too.
	i.Verdict, i.DecidedBy = new(models.IdeaVerdictYes), new("user-2")
	setTriage(t, f, ideas, i)
	if _, err := svc.Undo(f.ctx, id, "user-1"); !errors.Is(err, assistantundo.ErrChangedSince) {
		t.Fatalf("decided again: %v", err)
	}
	i.DecidedBy = new("user-1")
	setTriage(t, f, ideas, i)
	if _, err := svc.Undo(f.ctx, id, "user-1"); err != nil {
		t.Fatal(err)
	}
	if i := loadIdea(t, f, ideas); i.Verdict != nil || i.DecidedAt != nil || i.DecidedBy != nil || !f.undone(t, id) {
		t.Fatalf("idea = %+v; want back in the inbox", i)
	}
}

// Undo puts back the verdict, the reminder and the decision it replaced, not
// just clears them.
func TestUndo_IdeaVerdictRestoresEarlierTriage(t *testing.T) {
	f := newFixture(t)
	remind := time.Now().UTC().Add(72 * time.Hour).Truncate(time.Microsecond)
	decided := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Microsecond)
	ideas, svc := ideaFixture(t, f, &models.Idea{
		ID: "i1", Title: "Pricing myths", Verdict: new(models.IdeaVerdictLater), RemindAt: &remind, DecidedAt: &decided, DecidedBy: new("user-1"),
	})
	now := time.Now().UTC().Truncate(time.Microsecond)
	id := logVerdict(t, f, ideas, func(i *models.Idea) {
		i.Verdict, i.RemindAt, i.DecidedAt, i.DecidedBy = new(models.IdeaVerdictYes), nil, &now, new("user-2")
	})

	if _, err := svc.Undo(f.ctx, id, "user-2"); err != nil {
		t.Fatal(err)
	}
	i := loadIdea(t, f, ideas)
	if i.Verdict == nil || *i.Verdict != models.IdeaVerdictLater || i.RemindAt == nil || !i.RemindAt.Equal(remind) {
		t.Fatalf("verdict = %v, remind_at = %v; want later until %v", i.Verdict, i.RemindAt, remind)
	}
	if i.DecidedAt == nil || !i.DecidedAt.Equal(decided) || i.DecidedBy == nil || *i.DecidedBy != "user-1" {
		t.Fatalf("decided = %v by %v; want the original decision", i.DecidedAt, i.DecidedBy)
	}
}

// An earlier decider who has left the workspace is restored as no one.
func TestUndo_IdeaVerdictWithADeletedDecider(t *testing.T) {
	f := newFixture(t)
	decided := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Microsecond)
	ideas, svc := ideaFixture(t, f, &models.Idea{
		ID: "i1", Title: "Pricing myths", Verdict: new(models.IdeaVerdictNo), DecidedAt: &decided, DecidedBy: new("user-2"),
	})
	now := time.Now().UTC().Truncate(time.Microsecond)
	id := logVerdict(t, f, ideas, func(i *models.Idea) {
		i.Verdict, i.DecidedAt, i.DecidedBy = new(models.IdeaVerdictYes), &now, new("user-1")
	})
	if _, err := f.db.NewDelete().Model((*models.User)(nil)).Where("id = ?", "user-2").Exec(f.ctx); err != nil {
		t.Fatal(err)
	}

	if _, err := svc.Undo(f.ctx, id, "user-1"); err != nil {
		t.Fatal(err)
	}
	i := loadIdea(t, f, ideas)
	if i.Verdict == nil || *i.Verdict != models.IdeaVerdictNo || i.DecidedAt == nil || !i.DecidedAt.Equal(decided) || i.DecidedBy != nil {
		t.Fatalf("idea = %+v; want no, decided then by no one", i)
	}
}

func TestUndo_CampaignAssets(t *testing.T) {
	f := newFixture(t)
	f.seedCampaign(t, func(c *models.Campaign) { c.AssetIDs, c.UseAssets = models.StringSlice{"a1"}, true })
	id := f.logChange(t, models.AssistantActionCampaignAssets, assistantundo.CampaignAssets, func(c *models.Campaign) {
		c.AssetIDs = models.StringSlice{"a1", "a2"}
	})

	if _, err := f.svc.Undo(f.ctx, id, "u1"); err != nil {
		t.Fatal(err)
	}
	if got := f.load(t); !slices.Equal(got.AssetIDs, models.StringSlice{"a1"}) || !got.UseAssets {
		t.Fatalf("assets = %v, use = %v", got.AssetIDs, got.UseAssets)
	}
}

// Undoing a first attach turns the campaign's assets off again, and a flag
// flipped since the change counts as an edit.
func TestUndo_CampaignAssetsUseAssets(t *testing.T) {
	f := newFixture(t)
	f.seedCampaign(t, nil)
	id := f.logChange(t, models.AssistantActionCampaignAssets, assistantundo.CampaignAssets, func(c *models.Campaign) {
		c.AssetIDs, c.UseAssets = models.StringSlice{"a1"}, true
	})

	c := f.load(t)
	c.UseAssets = false
	f.save(t, c)
	if _, err := f.svc.Undo(f.ctx, id, "u1"); !errors.Is(err, assistantundo.ErrChangedSince) {
		t.Fatalf("flag changed since: %v", err)
	}
	c.UseAssets = true
	f.save(t, c)
	if _, err := f.svc.Undo(f.ctx, id, "u1"); err != nil {
		t.Fatal(err)
	}
	if got := f.load(t); len(got.AssetIDs) != 0 || got.UseAssets {
		t.Fatalf("assets = %v, use = %v; want none, off", got.AssetIDs, got.UseAssets)
	}
}

// Undoing an edit puts back the title, note and plan it replaced, and is
// refused once the idea was edited again.
func TestUndo_IdeaEdit(t *testing.T) {
	f := newFixture(t)
	day, err := models.ParseCalendarDate("2026-12-02")
	if err != nil {
		t.Fatal(err)
	}
	ideas, svc := ideaFixture(t, f, &models.Idea{ID: "i1", Title: "Pricing myths", Note: "three of them",
		Plan: models.IdeaPlan{PlatformID: new("linkedin"), TargetDate: &day}})
	i := loadIdea(t, f, ideas)
	before := assistantundo.IdeaEdit(i)
	i.Title, i.Note, i.Plan = "Five pricing myths", "", models.IdeaPlan{PostType: new("carousel")}
	if _, err := ideas.Update(f.ctx, i, "title", "note", "plan_platform_id", "plan_post_type", "plan_phase_id", "plan_target_date"); err != nil {
		t.Fatal(err)
	}
	id, err := assistantundo.Record(f.ctx, f.actions, &models.AssistantAction{ScopeKind: models.AssistantScopeWorkspace,
		Kind: models.AssistantActionIdeaEdit, EntityID: i.ID, Before: before, After: assistantundo.IdeaEdit(i), CreatedBy: "user-1"})
	if err != nil {
		t.Fatal(err)
	}

	i.Note = "edited by hand since"
	if _, err := ideas.Update(f.ctx, i, "note"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Undo(f.ctx, id, "user-1"); !errors.Is(err, assistantundo.ErrChangedSince) || f.undone(t, id) {
		t.Fatalf("changed since: %v", err)
	}
	i.Note = ""
	if _, err := ideas.Update(f.ctx, i, "note"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Undo(f.ctx, id, "user-1"); err != nil {
		t.Fatal(err)
	}
	got := loadIdea(t, f, ideas)
	if got.Title != "Pricing myths" || got.Note != "three of them" || got.Plan.PostType != nil ||
		got.Plan.PlatformID == nil || *got.Plan.PlatformID != "linkedin" || got.Plan.TargetDate == nil || got.Plan.TargetDate.String() != "2026-12-02" {
		t.Fatalf("idea = %+v, plan %+v", got, got.Plan)
	}
}
