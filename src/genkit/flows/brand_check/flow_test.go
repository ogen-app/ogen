package brand_check

import (
	"context"
	"strings"
	"testing"

	"github.com/firebase/genkit/go/genkit"

	"github.com/ogen-app/ogen/src/domain/modelconfig"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/fakemodel"
	"github.com/ogen-app/ogen/src/infra/vendors/llm"
)

func TestCheck(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	g := genkit.Init(ctx)
	m := fakemodel.Define(g, "brand-check", fakemodel.Answer(`{"findings": [
		{"postId": "p1", "rule": "unbacked_claim", "severity": "high", "quote": "10,000 happy guests", "issue": "No fact backs the guest count.", "suggestion": "Drop the number."},
		{"postId": "p1", "rule": "banned_word", "severity": "high", "quote": "cheap", "issue": "Banned.", "suggestion": ""},
		{"postId": "p2", "rule": "voice", "severity": "urgent", "quote": "not in the post", "issue": "Too casual for LinkedIn.", "suggestion": "Drop the slang."},
		{"postId": "p9", "rule": "voice", "severity": "low", "quote": "", "issue": "Unknown post.", "suggestion": ""},
		{"postId": "p2", "rule": "taste", "severity": "low", "quote": "", "issue": "Meh.", "suggestion": ""}
	]}`))
	fakemodel.Route(ctx, m, fakemodel.Slot{Flow: modelconfig.FlowAssistant, Slot: modelconfig.SlotBrandCheck})

	res, err := New(g, llm.NewProvider(), nil)(ctx, Request{Groups: []Group{{
		Brand:       "## Brand voice — write in this voice\n**Formal**",
		BannedWords: []string{"Cheap", "synergy"},
		Posts: []Post{
			{ID: "p1", Platform: "Instagram", Text: "Join 10,000 happy guests — CHEAP rooms this week!"},
			{ID: "p2", Platform: "LinkedIn", Text: "Yo, we're hiring lol"},
			{ID: "p3", Platform: "LinkedIn", Text: "Synergyless planning is not a word."},
		},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Checked != 3 {
		t.Fatalf("checked = %d", res.Checked)
	}
	var got []string
	for _, f := range res.Findings {
		got = append(got, f.PostID+":"+f.Rule+":"+f.Severity+":"+f.Quote)
	}
	// Banned words come from code, whole words only; the model's own is
	// dropped, as are findings on unknown posts or rules, and quotes not in
	// the post.
	want := "p1:banned_word:high:CHEAP|p1:unbacked_claim:high:10,000 happy guests|p2:voice:medium:"
	if strings.Join(got, "|") != want {
		t.Fatalf("findings = %v\nwant %s", got, want)
	}
	prompt := m.Requests()[0].Messages[len(m.Requests()[0].Messages)-1].Text()
	for _, w := range []string{"<brand>\n## Brand voice", `<post id="p2" platform="LinkedIn">`} {
		if !strings.Contains(prompt, w) {
			t.Fatalf("prompt lacks %q:\n%s", w, prompt)
		}
	}
}
