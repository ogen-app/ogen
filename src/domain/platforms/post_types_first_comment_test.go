package platforms

import (
	"strings"
	"testing"

	"github.com/ogen-app/ogen/src/domain/models"
)

// commentPlatform takes a 20-char first comment on text posts and none on
// stories.
func commentPlatform() *models.Platform {
	return &models.Platform{
		ID:        "cmtplat",
		Name:      "CmtPlat",
		PostTypes: models.PostTypeMap{"text-post": "Text post", "story": "Story", "live-video": "Live video"},
		TextConstraints: models.TextConstraints{
			MaxFirstCommentChars:    20,
			FirstCommentPerPostType: map[string]int{"story": 0},
		},
	}
}

func firstCommentRules(t *testing.T, post *models.Post, p *models.Platform) []string {
	t.Helper()
	var rules []string
	for _, e := range ValidatePublishReadiness(post, p, nil)[p.ID] {
		if strings.Contains(e.Rule, "first_comment") {
			rules = append(rules, e.Rule)
		}
	}
	return rules
}

func TestValidatePublishReadiness_FirstComment(t *testing.T) {
	p := commentPlatform()
	cases := []struct {
		name string
		post models.Post
		want []string
	}{
		{"no comment", models.Post{Content: "hi", PlatformPostType: "text-post"}, nil},
		{"fits", models.Post{Content: "hi", PlatformPostType: "text-post", FirstComment: "link below", FirstCommentDelayMinutes: 3}, nil},
		// Counted as it publishes: the Markdown markers don't count.
		{"markdown flattened", models.Post{Content: "hi", PlatformPostType: "text-post", FirstComment: "**seventeen chars!!**"}, nil},
		{"too long", models.Post{Content: "hi", PlatformPostType: "text-post", FirstComment: strings.Repeat("x", 21)},
			[]string{RuleMaxFirstCommentChars}},
		{"type takes none", models.Post{Content: "hi", PlatformPostType: "story", FirstComment: "link"},
			[]string{RuleFirstCommentUnsupported}},
		{"bad delay", models.Post{Content: "hi", PlatformPostType: "text-post", FirstComment: "link", FirstCommentDelayMinutes: 2},
			[]string{RuleFirstCommentDelay}},
	}
	for _, tc := range cases {
		got := firstCommentRules(t, &tc.post, p)
		if strings.Join(got, ",") != strings.Join(tc.want, ",") {
			t.Errorf("%s: rules = %v, want %v", tc.name, got, tc.want)
		}
	}

	// A platform with no first-comment limit rejects any comment.
	none := &models.Platform{ID: "x", Name: "X", PostTypes: models.PostTypeMap{"text-post": "Text"}}
	post := models.Post{Content: "hi", PlatformPostType: "text-post", FirstComment: "link"}
	if got := firstCommentRules(t, &post, none); len(got) != 1 || got[0] != RuleFirstCommentUnsupported {
		t.Errorf("unsupported platform: rules = %v", got)
	}

	// Threads skip the whole-post media rules but still check the comment.
	thread := &models.Platform{
		ID: "th", Name: "Threads", PostTypes: models.PostTypeMap{"thread": "Thread"},
		TextConstraints: models.TextConstraints{MaxContentChars: 500, MaxFirstCommentChars: 5},
	}
	tp := models.Post{
		Content: "one\n---\ntwo", PlatformPostType: models.PostTypeThread, FirstComment: "too long",
		ThreadSegments: models.ThreadSegments{{Content: "one"}, {Content: "two"}},
	}
	if got := firstCommentRules(t, &tp, thread); len(got) != 1 || got[0] != RuleMaxFirstCommentChars {
		t.Errorf("thread: rules = %v", got)
	}
}

func TestResolvePostTypeRules_FirstCommentLimit(t *testing.T) {
	want := map[string]int{"text-post": 20, "story": 0, "live-video": 20}
	for _, v := range ResolvePostTypeRules(commentPlatform()) {
		if v.MaxFirstCommentChars != want[v.Slug] {
			t.Errorf("%s: max_first_comment_chars = %d, want %d", v.Slug, v.MaxFirstCommentChars, want[v.Slug])
		}
	}
}
