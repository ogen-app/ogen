package post_quality

import (
	"testing"

	"github.com/ogen-app/ogen/src/domain/models"
)

// The content and context hashes are the change-detection fingerprints: the
// assess flow re-runs the model only when they differ from the stored ones,
// and automatic runs look at the content side alone. These tests pin the
// properties cache correctness depends on: each is deterministic, and each
// changes whenever something on its side changes.
func TestContextHash(t *testing.T) {
	prof := Profile{Correctness: 0.30, Clarity: 0.30, Engagement: 0.20, Delivery: 0.20}
	base := &renderedPrompts{system: "sys", userContext: "ctx"}
	want := contextHash(base, "claude-sonnet", prof)

	if got := contextHash(&renderedPrompts{system: "sys", userContext: "ctx", user: "other body"}, "claude-sonnet", prof); got != want {
		t.Fatalf("the post body must not affect the context hash")
	}
	cases := map[string]string{
		"system":  contextHash(&renderedPrompts{system: "SYS", userContext: "ctx"}, "claude-sonnet", prof),
		"context": contextHash(&renderedPrompts{system: "sys", userContext: "CTX"}, "claude-sonnet", prof),
		"model":   contextHash(base, "claude-haiku", prof),
		"weights": contextHash(base, "claude-sonnet", Profile{Correctness: 0.40, Clarity: 0.20, Engagement: 0.20, Delivery: 0.20}),
	}
	for field, got := range cases {
		if got == want {
			t.Errorf("changing %s did not change the context hash", field)
		}
	}
}

func TestContentHash(t *testing.T) {
	post := &models.Post{Title: "t", Content: "body", PlatformID: "pl", PlatformPostType: "text-post"}
	want := contentHash(post, false)
	if got := contentHash(&models.Post{Title: "t", Content: "body", PlatformID: "pl", PlatformPostType: "text-post"}, false); got != want {
		t.Fatalf("identical posts produced different hashes")
	}
	cases := map[string]string{
		"title":   contentHash(&models.Post{Title: "T", Content: "body", PlatformID: "pl", PlatformPostType: "text-post"}, false),
		"body":    contentHash(&models.Post{Title: "t", Content: "Body", PlatformID: "pl", PlatformPostType: "text-post"}, false),
		"type":    contentHash(&models.Post{Title: "t", Content: "body", PlatformID: "pl", PlatformPostType: "thread"}, false),
		"caption": contentHash(post, true),
	}
	for field, got := range cases {
		if got == want {
			t.Errorf("changing %s did not change the content hash", field)
		}
	}
}

func TestHashPartsSeparator(t *testing.T) {
	// Without a delimiter between parts, ("ab","c") and ("a","bc") would
	// hash identically.
	if hashParts("ab", "c") == hashParts("a", "bc") {
		t.Fatal("adjacent parts collided: separator not effective")
	}
}

func TestReuse(t *testing.T) {
	in := &inputs{contentHash: "content", contextHash: "context"}
	same := &models.PostEvaluation{ContentHash: "content", ContextHash: "context", InputHash: in.inputHash()}
	contextOnly := &models.PostEvaluation{ContentHash: "content", ContextHash: "old", InputHash: "x"}
	contentChanged := &models.PostEvaluation{ContentHash: "old", ContextHash: "context", InputHash: "x"}

	cases := []struct {
		name    string
		cached  *models.PostEvaluation
		trigger models.QualityTrigger
		want    bool
	}{
		{"never assessed", nil, models.QualityTriggerReady, false},
		{"identical inputs, manual", same, models.QualityTriggerManual, true},
		{"identical inputs, auto", same, models.QualityTriggerVersion, true},
		{"context only, manual re-runs", contextOnly, models.QualityTriggerManual, false},
		{"context only, auto marks stale", contextOnly, models.QualityTriggerReady, true},
		{"content changed, auto re-runs", contentChanged, models.QualityTriggerScheduled, false},
		{"published with an assessment", contentChanged, models.QualityTriggerPublished, true},
		{"published never assessed", nil, models.QualityTriggerPublished, false},
		{"legacy row without content hash, auto", &models.PostEvaluation{InputHash: "x"}, models.QualityTriggerVersion, true},
		{"legacy row without content hash, manual", &models.PostEvaluation{InputHash: "x"}, models.QualityTriggerManual, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := reuse(tc.cached, in, tc.trigger); got != tc.want {
				t.Fatalf("reuse = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestStaleReason(t *testing.T) {
	e := &models.PostEvaluation{ContentHash: "a", ContextHash: "b"}
	cases := map[models.QualityStaleReason][2]string{
		"":                         {"a", "b"},
		models.QualityStaleContent: {"x", "b"},
		models.QualityStaleContext: {"a", "x"},
		models.QualityStaleBoth:    {"x", "y"},
	}
	for want, h := range cases {
		if got := e.StaleReason(h[0], h[1]); got != want {
			t.Errorf("StaleReason(%q, %q) = %q, want %q", h[0], h[1], got, want)
		}
	}
}

func TestQuietFinish(t *testing.T) {
	cached := &PostQualityResponse{Cached: true}
	fresh := &PostQualityResponse{}
	skipped := &SkippedError{Reason: "cap"}
	cases := []struct {
		name    string
		trigger models.QualityTrigger
		out     *PostQualityResponse
		err     error
		want    bool
	}{
		{"manual cached still reports", models.QualityTriggerManual, cached, nil, false},
		{"auto cached is quiet", models.QualityTriggerReady, cached, nil, true},
		{"auto skipped is quiet", models.QualityTriggerVersion, nil, skipped, true},
		{"auto fresh reports", models.QualityTriggerVersion, fresh, nil, false},
	}
	for _, tc := range cases {
		if got := quietFinish(tc.trigger, tc.out, tc.err); got != tc.want {
			t.Errorf("%s: quietFinish = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestTriggerStillApplies(t *testing.T) {
	cases := []struct {
		trigger models.QualityTrigger
		status  models.PostStatus
		want    bool
	}{
		{models.QualityTriggerReady, models.PostStatusReadyForPublish, true},
		{models.QualityTriggerReady, models.PostStatusDraft, false},
		{models.QualityTriggerVersion, models.PostStatusDraft, true},
		{models.QualityTriggerManual, models.PostStatusDraft, true},
	}
	for _, tc := range cases {
		if got := triggerStillApplies(tc.trigger, tc.status); got != tc.want {
			t.Errorf("triggerStillApplies(%s, %s) = %v, want %v", tc.trigger, tc.status, got, tc.want)
		}
	}
}
