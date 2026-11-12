package ideas_common

import (
	"strings"
	"testing"
)

// The prompt gives each idea its title and its angle, or its note without
// one.
func TestPrompt_CarriesEachIdea(t *testing.T) {
	p := prompt(Request{Ideas: []Idea{
		{Title: "Spa reopening", Angle: "Show the new pools first", Note: "ignored"},
		{Title: "Spring offers", Note: "Two nights for one"},
	}})
	for _, want := range []string{`title="Spa reopening"`, "Show the new pools first", `n=2 title="Spring offers"`, "Two nights for one"} {
		if !strings.Contains(p, want) {
			t.Fatalf("prompt lacks %q:\n%s", want, p)
		}
	}
	if strings.Contains(p, "ignored") {
		t.Fatal("an idea's angle stands in for its note")
	}
}
