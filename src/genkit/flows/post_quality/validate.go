package post_quality

import (
	"cmp"
	"fmt"
	"slices"
	"strings"

	"github.com/ogen-app/ogen/src/domain/models"
)

// validateInput enforces the flow's preconditions on the loaded Post:
// it must have non-whitespace body text, a Platform, and a post Type.
// Returns *ValidationError (→ HTTP 400) so an unevaluable post never
// reaches a model call.
func validateInput(post *models.Post) error {
	if strings.TrimSpace(post.Content) == "" {
		return &ValidationError{Msg: "post has no body text to evaluate"}
	}
	if post.PlatformID == "" {
		return &ValidationError{Msg: "post has no platform; assign a platform before assessing quality"}
	}
	if post.PlatformPostType == "" {
		return &ValidationError{Msg: "post has no type; assign a post type before assessing quality"}
	}
	return nil
}

// Length limits for the model's prose. The prompt asks for them; the backend
// enforces them so one verbose answer cannot bloat the stored assessment.
const (
	maxVerdictChars = 240
	maxAdviceChars  = 200
)

// validateOutput enforces the one part of the contract that makes an
// evaluation unusable and sanitizes the rest in place rather than discarding
// the whole response: the four scores are the product, so a thin suggestion
// must not fail an otherwise usable evaluation.
//
// Hard failure (triggers the retry): a dimension score outside [0,10], which
// would break the deterministic overall.
//
// Sanitized in place: verdicts are clipped to maxVerdictChars; suggestions
// missing a span or advice, or with an invalid severity, are dropped;
// advice is clipped to maxAdviceChars; survivors are trimmed to
// suggestionCap per dimension.
func validateOutput(out *assessmentOutput, suggestionCap int) error {
	dims := []struct {
		key models.EvaluationDimensionKey
		dim *dimensionOutput
	}{
		{models.DimensionCorrectness, &out.Correctness},
		{models.DimensionClarity, &out.Clarity},
		{models.DimensionEngagement, &out.Engagement},
		{models.DimensionDelivery, &out.Delivery},
	}
	for _, d := range dims {
		if d.dim.Score < 0 || d.dim.Score > 10 {
			return fmt.Errorf("%s: score %d out of range 0-10", d.key, d.dim.Score)
		}
		d.dim.Verdict = clipSentences(d.dim.Verdict, maxVerdictChars)
		d.dim.Suggestions = sanitizeSuggestions(d.dim.Suggestions, suggestionCap)
	}
	return nil
}

// sanitizeSuggestions drops suggestions that are unusable (no span, no
// advice, or an invalid severity), clips the advice, and trims the
// survivors to suggestionCap.
func sanitizeSuggestions(in []suggestionOutput, suggestionCap int) []suggestionOutput {
	kept := in[:0]
	for _, s := range in {
		if strings.TrimSpace(s.Span) == "" ||
			strings.TrimSpace(s.Advice) == "" ||
			!validSeverity(s.Severity) {
			continue
		}
		s.Advice = clipSentences(s.Advice, maxAdviceChars)
		kept = append(kept, s)
	}
	if suggestionCap >= 0 && len(kept) > suggestionCap {
		kept = kept[:suggestionCap]
	}
	return kept
}

func validSeverity(s string) bool {
	switch models.SuggestionSeverity(s) {
	case models.SeverityHigh, models.SeverityMedium, models.SeverityLow:
		return true
	}
	return false
}

// clipSentences shortens s to at most maxChars runes. It keeps whole
// sentences when one ends inside the limit, otherwise cuts at the last word
// boundary and adds an ellipsis.
func clipSentences(s string, maxChars int) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= maxChars {
		return s
	}
	head := string(r[:maxChars])
	if i := strings.LastIndexAny(head, ".!?"); i > 0 {
		return head[:i+1]
	}
	if i := strings.LastIndexByte(head, ' '); i > 0 {
		head = head[:i]
	}
	return strings.TrimRight(head, " ,;:") + "…"
}

// capPostSuggestions keeps at most limit suggestions across the whole post.
// A span flagged by more than one dimension is kept once, under the
// dimension that rated it most severe. Survivors are chosen by severity and
// then by the dimension's weight, so the fixes that move the overall score
// most stay. Each dimension keeps its survivors in severity order.
func capPostSuggestions(result *models.EvaluationResult, limit int) {
	dims := result.Dimensions()
	type ranked struct {
		s      models.EvaluationSuggestion
		weight float64
	}
	bySpan := map[string]int{}
	var all []ranked
	for _, d := range dims {
		for _, s := range d.Suggestions {
			key := strings.ToLower(strings.TrimSpace(s.Span))
			if i, seen := bySpan[key]; seen {
				if s.Severity.Rank() < all[i].s.Severity.Rank() {
					all[i] = ranked{s, d.Weight}
				}
				continue
			}
			bySpan[key] = len(all)
			all = append(all, ranked{s, d.Weight})
		}
	}
	slices.SortStableFunc(all, func(a, b ranked) int {
		return cmp.Or(
			cmp.Compare(a.s.Severity.Rank(), b.s.Severity.Rank()),
			cmp.Compare(b.weight, a.weight),
		)
	})
	if len(all) > limit {
		all = all[:limit]
	}
	for _, d := range dims {
		d.Suggestions = d.Suggestions[:0]
	}
	for _, r := range all {
		d := result.Dimension(r.s.Dimension)
		d.Suggestions = append(d.Suggestions, r.s)
	}
}
