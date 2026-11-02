package post_quality

import (
	"reflect"
	"strings"
	"testing"
)

func fieldIndex(t *testing.T, typ reflect.Type, name string) int {
	t.Helper()
	for i := range typ.NumField() {
		if typ.Field(i).Name == name {
			return i
		}
	}
	t.Fatalf("field %q not found on %s", name, typ.Name())
	return -1
}

// The model emits fields in schema order, which follows struct field
// order. Verdict must precede Score so the model reasons before it commits
// to a number. This is the guard against an accidental reorder.
func TestVerdictPrecedesScore(t *testing.T) {
	typ := reflect.TypeFor[dimensionOutput]()
	verdict := fieldIndex(t, typ, "Verdict")
	score := fieldIndex(t, typ, "Score")
	if verdict >= score {
		t.Errorf("Verdict (idx %d) must precede Score (idx %d)", verdict, score)
	}
}

func TestLoadTemplates(t *testing.T) {
	tpl, err := loadTemplates()
	if err != nil {
		t.Fatalf("loadTemplates: %v", err)
	}

	sys, err := tpl.renderSystem()
	if err != nil {
		t.Fatalf("renderSystem: %v", err)
	}
	for _, marker := range []string{"CORRECTNESS", "CLARITY", "ENGAGEMENT", "DELIVERY", "verdict", "span", "second person", "240 characters"} {
		if !strings.Contains(sys, marker) {
			t.Errorf("system prompt missing %q", marker)
		}
	}

	usr, err := tpl.renderUser(assessmentTemplateData{
		Platform:      "LinkedIn",
		PostType:      "carousel",
		CaptionScoped: true,
		CampaignName:  "Launch",
		PostBody:      "UNIQUE_BODY_TOKEN here",
		Assets:        []assetContext{{Title: "Brand doc", Preview: "10x faster"}},
	})
	if err != nil {
		t.Fatalf("renderUser: %v", err)
	}
	for _, marker := range []string{"LinkedIn", "carousel", "UNIQUE_BODY_TOKEN", "Brand doc", "caption-scoped"} {
		if !strings.Contains(usr, marker) {
			t.Errorf("user prompt missing %q\n---\n%s", marker, usr)
		}
	}

	// The per-dimension scoring directive is added in code, not the
	// template; verify it renders for a named dimension.
	instr := dimensionInstruction("Clarity")
	for _, marker := range []string{"Clarity", "one or two"} {
		if !strings.Contains(instr, marker) {
			t.Errorf("dimension instruction missing %q: %s", marker, instr)
		}
	}

	// Caption-scoped notice must be absent when the flag is off.
	usr2, err := tpl.renderUser(assessmentTemplateData{Platform: "X", PostType: "text-post", PostBody: "b"})
	if err != nil {
		t.Fatalf("renderUser (no media): %v", err)
	}
	if strings.Contains(usr2, "caption-scoped") {
		t.Errorf("caption-scoped notice should be absent when CaptionScoped is false")
	}
}
