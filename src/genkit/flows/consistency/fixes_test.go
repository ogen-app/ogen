package consistency

import (
	"testing"

	"github.com/ogen-app/ogen/src/domain/models"
)

// A brief finding's key holds while the brief does; each finding of one
// aspect gets its own.
func TestBriefFindings_Keys(t *testing.T) {
	c := &models.Campaign{ID: "c", Description: "Launch"}
	got := briefFindings(c, []Finding{{Aspect: "tone", Issue: "a"}, {Aspect: "tone", Issue: "b"}})
	if got[0].Key == got[1].Key || got[0].Fixes[0].Kind != FixImproveBrief || got[0].Fixes[0].Instruction == "" {
		t.Fatalf("findings = %+v", got)
	}
	again := briefFindings(c, []Finding{{Aspect: "tone", Issue: "worded otherwise"}})
	if again[0].Key != got[0].Key {
		t.Fatal("the same brief gave another key")
	}
	c.Description = "Relaunch"
	if edited := briefFindings(c, []Finding{{Aspect: "tone"}}); edited[0].Key == got[0].Key {
		t.Fatal("an edited brief kept the key")
	}
}

// A post finding is rewritten while its copy is Ogen's alone, and always
// opened; its key changes with the post's text.
func TestPostFinding_Fixes(t *testing.T) {
	draft := models.Post{ID: "p", Status: models.PostStatusDraft, Content: "x"}
	f := postFinding("c", draft, PostFinding{PostID: "p", Issue: "off", Suggestion: "on"})
	if len(f.Fixes) != 2 || f.Fixes[0].Kind != FixRevisePost || f.Fixes[1].Kind != FixOpenPost {
		t.Fatalf("draft fixes = %+v", f.Fixes)
	}
	scheduled := draft
	scheduled.Status = models.PostStatusScheduled
	if s := postFinding("c", scheduled, PostFinding{PostID: "p"}); len(s.Fixes) != 1 || s.Fixes[0].Kind != FixOpenPost {
		t.Fatalf("scheduled fixes = %+v", s.Fixes)
	}
	edited := draft
	edited.Content = "y"
	if postFinding("c", edited, PostFinding{}).Key == f.Key {
		t.Fatal("an edited post kept the key")
	}
}
