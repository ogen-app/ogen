package models

import "testing"

func TestFirstCommentLimitFor(t *testing.T) {
	c := TextConstraints{MaxFirstCommentChars: 2200, FirstCommentPerPostType: map[string]int{"story": 0, "reel": 1000}}
	for slug, want := range map[string]int{"image-post": 2200, "story": 0, "reel": 1000} {
		if got := c.FirstCommentLimitFor(slug); got != want {
			t.Errorf("FirstCommentLimitFor(%q) = %d, want %d", slug, got, want)
		}
	}
	if got := (TextConstraints{}).FirstCommentLimitFor("text-post"); got != 0 {
		t.Errorf("a platform without limits takes no first comment, got %d", got)
	}
}

func TestSendsFirstComment(t *testing.T) {
	linkedin := &Platform{TextConstraints: TextConstraints{MaxFirstCommentChars: 1250, FirstCommentPerPostType: map[string]int{"article": 0}}}
	cases := []struct {
		name string
		post Post
		want bool
	}{
		{"supported", Post{FirstComment: "link", PlatformPostType: "text-post", Platform: linkedin}, true},
		{"blank comment", Post{FirstComment: "  \n", PlatformPostType: "text-post", Platform: linkedin}, false},
		{"type turned off", Post{FirstComment: "link", PlatformPostType: "article", Platform: linkedin}, false},
		{"platform without first comments", Post{FirstComment: "link", PlatformPostType: "text-post", Platform: &Platform{}}, false},
		{"no platform", Post{FirstComment: "link", PlatformPostType: "text-post"}, false},
	}
	for _, tc := range cases {
		if got := tc.post.SendsFirstComment(); got != tc.want {
			t.Errorf("%s: SendsFirstComment() = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestValidFirstCommentDelay(t *testing.T) {
	for _, m := range []int{0, 1, 3, 5, 10} {
		if !ValidFirstCommentDelay(m) {
			t.Errorf("%d should be a valid delay", m)
		}
	}
	for _, m := range []int{-1, 2, 4, 15, 60} {
		if ValidFirstCommentDelay(m) {
			t.Errorf("%d should not be a valid delay", m)
		}
	}
}

func TestTextConstraintsFirstCommentJSON(t *testing.T) {
	var c TextConstraints
	if err := c.Scan(`{"max_content_chars":3000,"max_first_comment_chars":1250,"first_comment_per_post_type":{"article":0}}`); err != nil {
		t.Fatal(err)
	}
	if c.MaxFirstCommentChars != 1250 || c.FirstCommentLimitFor("article") != 0 {
		t.Errorf("scanned %+v", c)
	}
	// A platform without first comments keeps its stored JSON unchanged.
	v, err := TextConstraints{MaxContentChars: 280}.Value()
	if err != nil {
		t.Fatal(err)
	}
	if v != `{"max_content_chars":280,"max_title_chars":0}` {
		t.Errorf("Value() = %s", v)
	}
	kept := TextConstraints{MaxContentChars: 1}.WithFirstCommentOf(c)
	if kept.MaxContentChars != 1 || kept.MaxFirstCommentChars != 1250 || kept.FirstCommentLimitFor("article") != 0 {
		t.Errorf("WithFirstCommentOf = %+v", kept)
	}
}
