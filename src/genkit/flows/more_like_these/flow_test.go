package more_like_these

import (
	"slices"
	"strings"
	"testing"
)

// The model's ideas keep a title, stop at the count, and cite only the refs
// they were given.
func TestIdeas_Filters(t *testing.T) {
	req := Request{Count: 2, Examples: []Example{{ID: "p1"}}, Sources: []Source{{Ref: "asset:a1"}}}
	got := ideas(req, &output{Ideas: []modelIdea{
		{Title: " ", Angle: "dropped"},
		{Title: "Pricing pages that convert", Theses: []string{"one", " "}, Sources: []string{"asset:a1", "asset:made-up", "asset:a1"}},
		{Title: "From the post", Sources: []string{"post:p1"}},
		{Title: "Over the count"},
	}})
	if len(got) != 2 || got[0].Title != "Pricing pages that convert" || !slices.Equal(got[0].Sources, []string{"asset:a1"}) ||
		!slices.Equal(got[0].Theses, []string{"one"}) || !slices.Equal(got[1].Sources, []string{"post:p1"}) {
		t.Fatalf("ideas = %+v", got)
	}
}

func TestPrompt_CarriesExamplesAndSources(t *testing.T) {
	p := prompt(Request{Count: 3, Instruction: "shorter", Examples: []Example{{ID: "p1", Title: "Why discounts backfire", Text: "Most buyers…"}},
		Sources: []Source{{Ref: "idea:i1", Title: "Pricing myths", Text: "angle"}}})
	for _, want := range []string{"Write 3 new idea(s)", "Steer: shorter", `ref="post:p1"`, "Most buyers", `ref="idea:i1"`} {
		if !strings.Contains(p, want) {
			t.Fatalf("prompt lacks %q:\n%s", want, p)
		}
	}
}
