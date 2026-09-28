package post_quality

import "testing"

// inputHash is the change-detection fingerprint: the assess flow
// re-runs the model only when this value differs from the stored one. These
// tests pin the properties cache correctness depends on — it is
// deterministic for identical inputs, and it changes whenever anything that
// determines the stored result changes: a field the model sees (system
// prompt, user prompt, model id) or the weight profile that ComposeScore
// folds into OverallPct and each dimension's Weight/Contribution.
func TestInputHash(t *testing.T) {
	prof := Profile{Correctness: 0.30, Clarity: 0.30, Engagement: 0.20, Delivery: 0.20}
	base := &renderedPrompts{system: "sys", user: "usr"}
	want := inputHash(base, "claude-sonnet", prof)

	t.Run("deterministic for identical inputs", func(t *testing.T) {
		got := inputHash(&renderedPrompts{system: "sys", user: "usr"}, "claude-sonnet", prof)
		if got != want {
			t.Fatalf("identical inputs produced different hashes:\n %s\n %s", want, got)
		}
	})

	t.Run("captionScoped does not affect the hash", func(t *testing.T) {
		// captionScoped is a derived flag, not part of what the model reads,
		// so it must not perturb the fingerprint.
		got := inputHash(&renderedPrompts{system: "sys", user: "usr", captionScoped: true}, "claude-sonnet", prof)
		if got != want {
			t.Fatalf("captionScoped changed the hash; it should not")
		}
	})

	t.Run("changes when any score-determining input changes", func(t *testing.T) {
		cases := map[string]string{
			"system":  inputHash(&renderedPrompts{system: "SYS", user: "usr"}, "claude-sonnet", prof),
			"user":    inputHash(&renderedPrompts{system: "sys", user: "USR"}, "claude-sonnet", prof),
			"model":   inputHash(base, "claude-haiku", prof),
			"weights": inputHash(base, "claude-sonnet", Profile{Correctness: 0.40, Clarity: 0.20, Engagement: 0.20, Delivery: 0.20}),
		}
		for field, got := range cases {
			if got == want {
				t.Errorf("changing %s did not change the hash", field)
			}
		}
	})

	t.Run("separator prevents field bleed", func(t *testing.T) {
		// Without a delimiter between fields, ("ab","c") and ("a","bc")
		// would hash identically. The unit-separator must keep them distinct.
		a := inputHash(&renderedPrompts{system: "ab", user: "c"}, "m", prof)
		b := inputHash(&renderedPrompts{system: "a", user: "bc"}, "m", prof)
		if a == b {
			t.Fatalf("adjacent fields collided: separator not effective")
		}
	})
}
