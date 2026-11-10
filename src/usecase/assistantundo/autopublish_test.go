package assistantundo_test

import (
	"errors"
	"slices"
	"testing"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/usecase/assistantundo"
)

// Undoing an auto-publish change puts the allowlist back, unless it was
// changed again since.
func TestUndo_AutoPublish(t *testing.T) {
	f := newFixture(t)
	allow := repository.NewAutoPublishAllowlistRepository(f.db)
	svc := assistantundo.New(f.db, f.actions, f.campaigns).WithAutoPublish(allow)
	log := func() string {
		id, err := assistantundo.Record(f.ctx, f.actions, &models.AssistantAction{ScopeKind: models.AssistantScopeWorkspace,
			Kind: models.AssistantActionAutoPublish, EntityID: "auto_publish",
			Before: assistantundo.AutoPublish([]string{"linkedin"}), After: assistantundo.AutoPublish([]string{"linkedin", "x"}), CreatedBy: "user-1"})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	if err := allow.Set(f.ctx, []string{"x", "linkedin", "bluesky"}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Undo(f.ctx, log(), "user-1"); !errors.Is(err, assistantundo.ErrChangedSince) {
		t.Fatalf("undo over a later change = %v", err)
	}
	if err := allow.Set(f.ctx, []string{"x", "linkedin"}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Undo(f.ctx, log(), "user-1"); err != nil {
		t.Fatal(err)
	}
	if got, _ := allow.List(f.ctx); !slices.Equal(got, []string{"linkedin"}) {
		t.Fatalf("allowlist = %v", got)
	}
}
