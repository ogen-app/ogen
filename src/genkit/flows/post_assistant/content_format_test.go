package post_assistant

import (
	"strings"
	"testing"
	"text/template"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/flowkit"
)

// The post's format reaches both the planner and the writer through the shared
// context block, as the bare label; a post without one gets no section.
func TestContextBlock_ContentFormat(t *testing.T) {
	raw, err := promptFS.ReadFile("prompts/post_assistant.tmpl")
	if err != nil {
		t.Fatal(err)
	}
	tmpl := template.Must(template.New("post_assistant").Parse(string(raw)))
	contextTmpl := tmpl.Lookup("context")

	with, err := flowkit.RenderTemplate(contextTmpl, contextTemplateData{ContentFormat: "how-to"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(with, "## Content format") || !strings.Contains(with, "**how-to**") {
		t.Errorf("context block missing the format section:\n%s", with)
	}

	without, err := flowkit.RenderTemplate(contextTmpl, contextTemplateData{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(without, "Content format") {
		t.Errorf("a post without a format must render no format section:\n%s", without)
	}
}

// A format change must bust the cached assistant context.
func TestPostFingerprint_ContentFormat(t *testing.T) {
	howTo, story := models.ContentFormatHowTo, models.ContentFormatStory
	none := postFingerprint(&models.Post{Content: "x"})
	a := postFingerprint(&models.Post{Content: "x", ContentFormat: &howTo})
	b := postFingerprint(&models.Post{Content: "x", ContentFormat: &story})
	if none == a || a == b {
		t.Errorf("fingerprint must change with the format: none=%q how-to=%q story=%q", none, a, b)
	}
}
