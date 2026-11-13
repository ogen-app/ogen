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

// Undoing a tag change gives every asset its tags back, all or none, unless
// one of them was retagged since.
func TestUndo_AssetTags(t *testing.T) {
	f := newFixture(t)
	tags := repository.NewTagRepository(f.db)
	assets := repository.NewAssetRepository(f.db, tags, repository.NewAssetFileRepository(f.db))
	svc := assistantundo.New(f.db, f.actions, f.campaigns).WithAssets(assets)
	tag := func(name string) string {
		tg, err := tags.FindOrCreateByName(f.ctx, name, "", "user-1")
		if err != nil {
			t.Fatal(err)
		}
		return tg.ID
	}
	old, launch, extra := tag("old"), tag("launch"), tag("extra")
	now := time.Now().UTC()
	for _, a := range []*models.Asset{
		{ID: "a1", Title: "Deck", TagIDs: models.StringSlice{old}},
		{ID: "a2", Title: "Brief", TagIDs: models.StringSlice{}},
	} {
		a.Content, a.Status, a.CreatedBy, a.CreatedAt, a.UpdatedAt = "body", models.AssetStatusReady, "user-1", now, now
		if err := assets.Create(f.ctx, a); err != nil {
			t.Fatal(err)
		}
	}
	tagsOf := func(id string) []string {
		a, err := assets.GetByID(f.ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return a.TagIDs
	}
	retag := func() string {
		updated, was, err := assets.ApplyTagsBefore(f.ctx, []string{"a1", "a2"}, []string{launch}, []string{old})
		if err != nil {
			t.Fatal(err)
		}
		after := map[string][]string{}
		for _, a := range updated {
			after[a.ID] = a.TagIDs
		}
		id, err := assistantundo.Record(f.ctx, f.actions, &models.AssistantAction{ScopeKind: models.AssistantScopeWorkspace,
			Kind: models.AssistantActionAssetTags, Before: assistantundo.AssetTags(was), After: assistantundo.AssetTags(after), CreatedBy: "user-1"})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}

	id := retag()
	if !slices.Equal(tagsOf("a1"), []string{launch}) || !slices.Equal(tagsOf("a2"), []string{launch}) {
		t.Fatalf("tagged = %v, %v", tagsOf("a1"), tagsOf("a2"))
	}
	// a2 was retagged since: nothing is put back.
	if _, err := assets.ApplyTags(f.ctx, []string{"a2"}, []string{extra}, nil); err != nil {
		t.Fatal(err)
	}
	_, err := svc.Undo(f.ctx, id, "user-1")
	if ce, ok := errors.AsType[*assistantundo.ChangedSinceError](err); !errors.Is(err, assistantundo.ErrChangedSince) || !ok || ce.Changed != 1 || ce.Of != 2 {
		t.Fatalf("undo over a later change = %v; want 1 of the 2 changed since", err)
	}
	if !slices.Equal(tagsOf("a1"), []string{launch}) {
		t.Fatalf("a1 = %v; want it left as it was", tagsOf("a1"))
	}
	if _, err := assets.ApplyTags(f.ctx, []string{"a2"}, nil, []string{extra}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Undo(f.ctx, id, "user-1"); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(tagsOf("a1"), []string{old}) || len(tagsOf("a2")) != 0 {
		t.Fatalf("undone = %v, %v", tagsOf("a1"), tagsOf("a2"))
	}
}
