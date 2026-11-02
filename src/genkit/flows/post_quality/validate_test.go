package post_quality

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/ogen-app/ogen/src/domain/models"
)

func TestValidateInput(t *testing.T) {
	ok := &models.Post{Content: "hello", PlatformID: "pl1", PlatformPostType: "text-post"}
	if err := validateInput(ok); err != nil {
		t.Errorf("valid post rejected: %v", err)
	}

	tests := []struct {
		name string
		post *models.Post
	}{
		{"empty body", &models.Post{Content: "   ", PlatformID: "pl1", PlatformPostType: "text-post"}},
		{"no platform", &models.Post{Content: "hi", PlatformPostType: "text-post"}},
		{"no type", &models.Post{Content: "hi", PlatformID: "pl1"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateInput(tt.post)
			if err == nil {
				t.Fatal("expected ValidationError, got nil")
			}
			if _, ok := err.(*ValidationError); !ok {
				t.Errorf("expected *ValidationError, got %T", err)
			}
		})
	}
}

func goodDim() dimensionOutput {
	return dimensionOutput{
		Score: 7, Verdict: "v",
		Suggestions: []suggestionOutput{{Severity: "high", Span: "s", Advice: "a"}},
	}
}

func goodOutput() *assessmentOutput {
	return &assessmentOutput{
		Correctness: goodDim(), Clarity: goodDim(), Engagement: goodDim(), Delivery: goodDim(),
	}
}

func TestValidateOutputValid(t *testing.T) {
	o := goodOutput()
	if err := validateOutput(o, 3); err != nil {
		t.Errorf("valid output rejected: %v", err)
	}
	if len(o.Correctness.Suggestions) != 1 {
		t.Errorf("valid suggestion should be kept, got %d", len(o.Correctness.Suggestions))
	}
}

func TestValidateOutputHardFailsOnScore(t *testing.T) {
	// An out-of-range score is the only fatal condition.
	for _, tt := range []struct {
		name   string
		mutate func(*assessmentOutput)
	}{
		{"score too high", func(o *assessmentOutput) { o.Clarity.Score = 11 }},
		{"score negative", func(o *assessmentOutput) { o.Delivery.Score = -1 }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			o := goodOutput()
			tt.mutate(o)
			if err := validateOutput(o, 3); err == nil {
				t.Error("expected hard failure, got nil")
			}
		})
	}
}

func TestValidateOutputToleratesBlankProse(t *testing.T) {
	// A blank verdict does not fail the evaluation; the scores are still
	// usable.
	o := goodOutput()
	o.Clarity.Verdict = ""
	if err := validateOutput(o, 3); err != nil {
		t.Errorf("blank verdict should be tolerated, got %v", err)
	}
}

func TestValidateOutputDropsBadSuggestions(t *testing.T) {
	for _, tt := range []struct {
		name   string
		mutate func(*assessmentOutput)
	}{
		{"missing span", func(o *assessmentOutput) { o.Correctness.Suggestions[0].Span = "" }},
		{"missing advice", func(o *assessmentOutput) { o.Correctness.Suggestions[0].Advice = "  " }},
		{"bad severity", func(o *assessmentOutput) { o.Correctness.Suggestions[0].Severity = "urgent" }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			o := goodOutput()
			tt.mutate(o)
			if err := validateOutput(o, 3); err != nil {
				t.Fatalf("should sanitize, not fail: %v", err)
			}
			if len(o.Correctness.Suggestions) != 0 {
				t.Errorf("bad suggestion should be dropped, got %d", len(o.Correctness.Suggestions))
			}
		})
	}
}

func TestValidateOutputTrimsOverCap(t *testing.T) {
	o := goodOutput()
	o.Clarity.Suggestions = []suggestionOutput{
		{Severity: "low", Span: "s", Advice: "a"},
		{Severity: "low", Span: "s", Advice: "a"},
		{Severity: "low", Span: "s", Advice: "a"},
	}
	if err := validateOutput(o, 2); err != nil {
		t.Fatalf("over-cap should trim, not fail: %v", err)
	}
	if len(o.Clarity.Suggestions) != 2 {
		t.Errorf("expected trim to 2, got %d", len(o.Clarity.Suggestions))
	}
}

func TestToEvaluationResult(t *testing.T) {
	out := goodOutput()
	out.Engagement.Score = 9
	r := toEvaluationResult(out)

	if r.Engagement.Score != 9 || r.Correctness.Verdict != "v" || r.Clarity.Suggestions[0].Advice != "a" {
		t.Errorf("dimension fields not mapped: %+v", r)
	}
	// Each suggestion must be stamped with its parent dimension.
	if r.Delivery.Suggestions[0].Dimension != models.DimensionDelivery {
		t.Errorf("suggestion dimension = %q, want %q",
			r.Delivery.Suggestions[0].Dimension, models.DimensionDelivery)
	}
	if r.Correctness.Suggestions[0].Severity != models.SeverityHigh {
		t.Errorf("severity not mapped: %q", r.Correctness.Suggestions[0].Severity)
	}
	// Weight/Contribution must be left zero — ComposeScore fills them.
	if r.Correctness.Weight != 0 || r.Correctness.Contribution != 0 {
		t.Errorf("weight/contribution should be zero before ComposeScore")
	}
}

func TestValidateOutputClipsProse(t *testing.T) {
	o := goodOutput()
	o.Clarity.Verdict = strings.Repeat("Your hook is fine. ", 20)
	o.Clarity.Suggestions[0].Advice = strings.Repeat("word ", 60)
	if err := validateOutput(o, 3); err != nil {
		t.Fatal(err)
	}
	if n := utf8.RuneCountInString(o.Clarity.Verdict); n > maxVerdictChars {
		t.Errorf("verdict is %d runes, want <= %d", n, maxVerdictChars)
	}
	if !strings.HasSuffix(o.Clarity.Verdict, ".") {
		t.Errorf("verdict should end on a whole sentence: %q", o.Clarity.Verdict)
	}
	advice := o.Clarity.Suggestions[0].Advice
	if n := utf8.RuneCountInString(advice); n > maxAdviceChars {
		t.Errorf("advice is %d runes, want <= %d", n, maxAdviceChars)
	}
	if !strings.HasSuffix(advice, "…") {
		t.Errorf("advice without a sentence end should be cut at a word: %q", advice)
	}
}

func TestClipSentencesWithoutBreaksStaysInCap(t *testing.T) {
	got := clipSentences(strings.Repeat("x", 300), maxAdviceChars)
	if n := utf8.RuneCountInString(got); n != maxAdviceChars || !strings.HasSuffix(got, "…") {
		t.Fatalf("clipped to %d runes (%q…), want %d ending in an ellipsis", n, got[:10], maxAdviceChars)
	}
}

func TestClipSentencesShortTextUnchanged(t *testing.T) {
	if got := clipSentences("  Short one.  ", 240); got != "Short one." {
		t.Errorf("got %q", got)
	}
}

func sug(dim models.EvaluationDimensionKey, sev models.SuggestionSeverity, span string) models.EvaluationSuggestion {
	return models.EvaluationSuggestion{Dimension: dim, Severity: sev, Span: span, Advice: "a"}
}

func TestCapPostSuggestions(t *testing.T) {
	r := models.EvaluationResult{
		Correctness: models.EvaluationDimension{Weight: 0.1, Suggestions: []models.EvaluationSuggestion{
			sug(models.DimensionCorrectness, models.SeverityLow, "c1"),
			sug(models.DimensionCorrectness, models.SeverityLow, "shared"),
		}},
		Clarity: models.EvaluationDimension{Weight: 0.2, Suggestions: []models.EvaluationSuggestion{
			sug(models.DimensionClarity, models.SeverityMedium, "k1"),
			sug(models.DimensionClarity, models.SeverityLow, "k2"),
		}},
		Engagement: models.EvaluationDimension{Weight: 0.4, Suggestions: []models.EvaluationSuggestion{
			sug(models.DimensionEngagement, models.SeverityHigh, "Shared "),
			sug(models.DimensionEngagement, models.SeverityLow, "e2"),
		}},
		Delivery: models.EvaluationDimension{Weight: 0.3, Suggestions: []models.EvaluationSuggestion{
			sug(models.DimensionDelivery, models.SeverityMedium, "d1"),
			sug(models.DimensionDelivery, models.SeverityLow, "d2"),
		}},
	}
	capPostSuggestions(&r, 5)

	var got []string
	total := 0
	for _, d := range r.Dimensions() {
		for _, s := range d.Suggestions {
			got = append(got, string(s.Dimension)+":"+strings.TrimSpace(s.Span))
			total++
		}
	}
	if total != 5 {
		t.Fatalf("kept %d suggestions, want 5: %v", total, got)
	}
	// The shared span survives once, under the dimension that rated it high.
	if len(r.Correctness.Suggestions) != 0 {
		t.Errorf("correctness keeps %v; its low 'shared' is a duplicate and c1 is the lowest-weight low", r.Correctness.Suggestions)
	}
	if r.Engagement.Suggestions[0].Span != "Shared " || r.Engagement.Suggestions[0].Severity != models.SeverityHigh {
		t.Errorf("engagement should keep the high-severity shared span first: %v", r.Engagement.Suggestions)
	}
	// Medium items outrank lows; among lows the heavier dimensions win.
	want := []string{"clarity:k1", "engagement:Shared", "engagement:e2", "delivery:d1", "delivery:d2"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("kept %v, want %v", got, want)
	}
}
