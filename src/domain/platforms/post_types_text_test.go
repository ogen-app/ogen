package platforms

import (
	"strings"
	"testing"

	"github.com/ogen-app/ogen/src/domain/models"
)

// textPlatform mirrors the LinkedIn-style seed: a 3000-char feed default with
// an article override to 100k, plus a title cap so the title branch is
// exercised. PostTypes lists the slugs the resolver iterates.
func textPlatform() *models.Platform {
	return &models.Platform{
		ID:   "txtplat",
		Name: "TxtPlat",
		PostTypes: models.PostTypeMap{
			"text-post":  "Text post",
			"article":    "Article",
			"live-video": "Live video", // whitelist-only: rule is nil
		},
		TextConstraints: models.TextConstraints{
			MaxContentChars: 3000,
			MaxTitleChars:   100,
			PerPostType:     map[string]int{"article": 100000},
		},
	}
}

func TestResolvePostTypeRules_CharLimits(t *testing.T) {
	views := ResolvePostTypeRules(textPlatform())

	byslug := map[string]PostTypeRuleView{}
	for _, v := range views {
		byslug[v.Slug] = v
	}

	// Default limit applies to text-post.
	if r := byslug["text-post"].Rule; r == nil || r.MaxContentChars == nil || *r.MaxContentChars != 3000 {
		t.Fatalf("text-post: want max_content_chars=3000, got %+v", byslug["text-post"].Rule)
	}
	// Per-post-type override applies to article.
	if r := byslug["article"].Rule; r == nil || r.MaxContentChars == nil || *r.MaxContentChars != 100000 {
		t.Fatalf("article: want max_content_chars=100000, got %+v", byslug["article"].Rule)
	}
	// Whitelist-only slug carries no rule at all.
	if v := byslug["live-video"]; v.Rule != nil || !v.WhitelistOnly {
		t.Fatalf("live-video: want whitelist-only nil rule, got %+v", v)
	}
}

func TestResolvePostTypeRules_UnboundedIsNil(t *testing.T) {
	// A platform with no text constraints leaves max_content_chars nil so the
	// client treats it as no cap.
	p := &models.Platform{
		ID:        "nolimit",
		Name:      "NoLimit",
		PostTypes: models.PostTypeMap{"text-post": "Text post"},
	}
	views := ResolvePostTypeRules(p)
	if len(views) != 1 || views[0].Rule == nil || views[0].Rule.MaxContentChars != nil {
		t.Fatalf("want nil max_content_chars, got %+v", views)
	}
}

func TestValidatePostType_MaxContentChars(t *testing.T) {
	p := textPlatform()

	over := &models.Post{PlatformPostType: "text-post", Content: strings.Repeat("a", 3001)}
	if errs := ValidatePostType(over, p, nil); !hasRule(errs, RuleMaxContentChars) {
		t.Errorf("over-limit content: want max_content_chars, got %+v", errs)
	}

	at := &models.Post{PlatformPostType: "text-post", Content: strings.Repeat("a", 3000)}
	if errs := ValidatePostType(at, p, nil); hasRule(errs, RuleMaxContentChars) {
		t.Errorf("at-limit content: want no error, got %+v", errs)
	}

	// The article override lifts the same content well under its own cap.
	article := &models.Post{PlatformPostType: "article", Content: strings.Repeat("a", 5000)}
	if errs := ValidatePostType(article, p, nil); hasRule(errs, RuleMaxContentChars) {
		t.Errorf("article under 100k: want no error, got %+v", errs)
	}
}

func TestValidatePostType_CharsAreRunesNotBytes(t *testing.T) {
	// A platform capped at 3 chars: three multi-byte runes (9 bytes) must pass,
	// four must fail — proving the count is code points, not len().
	p := &models.Platform{
		ID:              "rune",
		Name:            "Rune",
		PostTypes:       models.PostTypeMap{"text-post": "Text post"},
		TextConstraints: models.TextConstraints{MaxContentChars: 3},
	}
	ok := &models.Post{PlatformPostType: "text-post", Content: "日本語"} // 3 runes / 9 bytes
	if errs := ValidatePostType(ok, p, nil); hasRule(errs, RuleMaxContentChars) {
		t.Errorf("3 runes at limit 3: want no error, got %+v", errs)
	}
	bad := &models.Post{PlatformPostType: "text-post", Content: "日本語字"} // 4 runes
	if errs := ValidatePostType(bad, p, nil); !hasRule(errs, RuleMaxContentChars) {
		t.Errorf("4 runes over limit 3: want max_content_chars, got %+v", errs)
	}
}

func TestValidatePostType_ContentLimitCountsVisibleLength(t *testing.T) {
	// CON-126: the char limit governs the flattened caption, not the Markdown
	// source, since we flatten before publishing. Platform capped at 4 chars.
	p := &models.Platform{
		ID:              "vis",
		Name:            "Vis",
		PostTypes:       models.PostTypeMap{"text-post": "Text post"},
		TextConstraints: models.TextConstraints{MaxContentChars: 4},
	}
	// "**bold**" is 8 raw runes but publishes "bold" (4) → must pass.
	if errs := ValidatePostType(&models.Post{PlatformPostType: "text-post", Content: "**bold**"}, p, nil); hasRule(errs, RuleMaxContentChars) {
		t.Errorf("bold within visible limit: want no error, got %+v", errs)
	}
	// "**boldx**" publishes "boldx" (5) → over.
	if errs := ValidatePostType(&models.Post{PlatformPostType: "text-post", Content: "**boldx**"}, p, nil); !hasRule(errs, RuleMaxContentChars) {
		t.Errorf("over visible limit: want max_content_chars, got %+v", errs)
	}
}

func TestValidatePostType_RequiresContentFlattens(t *testing.T) {
	// A body that is only Markdown syntax flattens to empty, so requires_content
	// must fire — it would otherwise publish as an empty post. "poll" requires
	// content; a lone "---" flattens to "".
	p := &models.Platform{
		ID:        "req",
		Name:      "Req",
		PostTypes: models.PostTypeMap{"poll": "Poll"},
	}
	if errs := ValidatePostType(&models.Post{PlatformPostType: "poll", Content: "---"}, p, nil); !hasRule(errs, RuleRequiresContent) {
		t.Errorf("markdown-only body: want requires_content, got %+v", errs)
	}
	// A real body with incidental markdown still satisfies requires_content.
	if errs := ValidatePostType(&models.Post{PlatformPostType: "poll", Content: "**vote**"}, p, nil); hasRule(errs, RuleRequiresContent) {
		t.Errorf("bold body: want no requires_content error, got %+v", errs)
	}
}

func TestValidatePostType_MaxTitleChars(t *testing.T) {
	p := textPlatform() // title cap 100

	over := &models.Post{PlatformPostType: "text-post", Title: strings.Repeat("x", 101)}
	if errs := ValidatePostType(over, p, nil); !hasRule(errs, RuleMaxTitleChars) {
		t.Errorf("over-limit title: want max_title_chars, got %+v", errs)
	}

	ok := &models.Post{PlatformPostType: "text-post", Title: "Fine title"}
	if errs := ValidatePostType(ok, p, nil); hasRule(errs, RuleMaxTitleChars) {
		t.Errorf("short title: want no error, got %+v", errs)
	}
}
