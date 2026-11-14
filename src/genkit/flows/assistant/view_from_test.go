package assistant

import (
	"strings"
	"testing"
	"text/template"
	"time"

	"github.com/ogen-app/ogen/src/domain/viewcontext"
	"github.com/ogen-app/ogen/src/usecase/viewresolve"
)

// A post thread opened from a review finding has the finding in its <view>:
// the issue, the words it is about and the review's suggestion.
func TestRenderView_FromFinding(t *testing.T) {
	raw, err := promptFS.ReadFile("prompts/assistant.tmpl")
	if err != nil {
		t.Fatal(err)
	}
	tmpl, err := template.New("assistant").Funcs(template.FuncMap{"inc": func(i int) int { return i + 1 }}).Parse(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	v := &viewresolve.Resolved{View: &viewcontext.ViewContext{}, Summary: `On post in post "Launch day"`,
		From: &viewcontext.Finding{Severity: "high", Issue: "Too salesy.", Quote: "Book now!!!", Suggestion: "Invite, don't push."}}
	got, err := renderView(&prompts{view: tmpl.Lookup("view")}, v, nil, time.Date(2026, 11, 14, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"- Opened from the campaign review's finding about this post (high): Too salesy.",
		`The words it is about: "Book now!!!"`,
		"The review suggests: Invite, don't push.",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("view lacks %q:\n%s", want, got)
		}
	}
}
