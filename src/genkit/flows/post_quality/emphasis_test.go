package post_quality

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestClipEmphasized(t *testing.T) {
	for _, tt := range []struct {
		name     string
		in       string
		want     string
		stripped bool
	}{
		{"no markers", "Your hook works.", "Your hook works.", false},
		{"one pair", "Your hook **buries the ask** at the end.", "Your hook **buries the ask** at the end.", false},
		{"pair at start", "**Lead with the number** so the claim lands first.", "**Lead with the number** so the claim lands first.", false},
		{"pair at end", "The close is **too vague**", "The close is **too vague**", false},
		{"spaces inside markers move out", "Your hook ** buries the ask ** at the end.", "Your hook **buries the ask** at the end.", false},
		{"space inside markers keeps a word break", "Your hook** buries the ask **at the end.", "Your hook **buries the ask** at the end.", false},
		{"surrounding space trimmed", "  Your **hook** works.  ", "Your **hook** works.", false},
		{"spanish", "Tu gancho **esconde la petición** al final.", "Tu gancho **esconde la petición** al final.", false},
		{"two pairs", "Your **hook** and your **close** are weak.", "Your hook and your close are weak.", true},
		{"unbalanced", "Your **hook is weak.", "Your hook is weak.", true},
		{"empty phrase", "Your hook **  ** is weak.", "Your hook    is weak.", true},
		{"phrase too long", "**Your hook buries the ask** here.", "Your hook buries the ask here.", true},
		{"stray asterisk", "Rated 5* but **the close** is vague.", "Rated 5 but the close is vague.", true},
		{"single asterisks", "Your *hook* is weak.", "Your hook is weak.", true},
		{"triple asterisks", "Your ***hook*** is weak.", "Your hook is weak.", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, stripped := clipEmphasized(tt.in, maxVerdictChars)
			if got != tt.want || stripped != tt.stripped {
				t.Errorf("clipEmphasized(%q) = %q, %v; want %q, %v", tt.in, got, stripped, tt.want, tt.stripped)
			}
		})
	}
}

func TestClipEmphasizedLimitExcludesMarkers(t *testing.T) {
	// The unmarked text is exactly at the limit: the markers must not push
	// it over and trigger a clip.
	in := "Your hook **buries the ask** here."
	got, stripped := clipEmphasized(in, utf8.RuneCountInString("Your hook buries the ask here."))
	if got != in || stripped {
		t.Errorf("got %q (stripped %v), want it unchanged", got, stripped)
	}
}

func TestClipEmphasizedKeepsPhraseThatSurvivesClip(t *testing.T) {
	in := "Your **opening line** works. " + strings.Repeat("Then it drifts. ", 20)
	got, stripped := clipEmphasized(in, maxVerdictChars)
	if stripped || !strings.HasPrefix(got, "Your **opening line** works.") {
		t.Fatalf("got %q (stripped %v)", got, stripped)
	}
	if n := utf8.RuneCountInString(strings.ReplaceAll(got, "*", "")); n > maxVerdictChars {
		t.Errorf("unmarked text is %d runes, want <= %d", n, maxVerdictChars)
	}
}

func TestClipEmphasizedDropsPhraseCutByClip(t *testing.T) {
	// The phrase starts inside the limit and ends past it, so the clip cuts
	// into it: the text is kept, the markers are not.
	head := strings.Repeat("Fine. ", 38) // 228 runes
	in := head + "**the close never names a date or a price**."
	got, stripped := clipEmphasized(in, maxVerdictChars)
	if stripped {
		t.Errorf("a phrase cut by the clip is not a contract breach")
	}
	if strings.Contains(got, "*") {
		t.Errorf("markers should be dropped when the clip cuts the phrase: %q", got)
	}
	if n := utf8.RuneCountInString(got); n > maxVerdictChars {
		t.Errorf("got %d runes, want <= %d", n, maxVerdictChars)
	}
}

func TestValidateOutputEmphasis(t *testing.T) {
	o := goodOutput()
	o.Clarity.Verdict = "Your hook **buries the ask** at the end."
	o.Clarity.Suggestions[0].Advice = "Your **hook** and your **close** are weak."
	o.Delivery.Suggestions[0].Advice = "****"
	if err := validateOutput(t.Context(), o, 3); err != nil {
		t.Fatal(err)
	}
	if o.Clarity.Verdict != "Your hook **buries the ask** at the end." {
		t.Errorf("valid emphasis should be kept: %q", o.Clarity.Verdict)
	}
	if got := o.Clarity.Suggestions[0].Advice; got != "Your hook and your close are weak." {
		t.Errorf("two pairs should be stripped: %q", got)
	}
	if len(o.Delivery.Suggestions) != 0 {
		t.Errorf("advice of markers only is blank and should be dropped: %v", o.Delivery.Suggestions)
	}
}
