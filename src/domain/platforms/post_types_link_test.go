package platforms

import (
	"strings"
	"testing"

	"github.com/ogen-app/ogen/src/domain/models"
)

// linkPlatform mirrors the Facebook seed's link-post entry.
func linkPlatform() *models.Platform {
	return &models.Platform{
		ID:   "fb",
		Name: "Facebook",
		PostTypes: models.PostTypeMap{
			"text-post": "Text post",
			"link-post": "Link post",
		},
		TextConstraints: models.TextConstraints{MaxContentChars: 100},
	}
}

func linkPost(content, link string) *models.Post {
	return &models.Post{PlatformPostType: models.PostTypeLinkPost, Content: content, CTAUrl: link}
}

func TestValidateLink(t *testing.T) {
	cases := []struct {
		link string
		ok   bool
	}{
		{"https://example.com", true},
		{"http://example.com/a/b?c=d#frag", true},
		{"https://sub.example.co.uk/path_with_underscores", true},
		{"example.com", false},
		{"ftp://example.com", false},
		{"javascript:alert(1)", false},
		{"https://localhost", false},
		{"https://example.", false},
		{"https://user:pw@example.com", false},
		{"https://exa mple.com", false},
		{"https://example.com/" + strings.Repeat("a", MaxLinkLen), false},
	}
	for _, tc := range cases {
		if err := ValidateLink(tc.link); (err == nil) != tc.ok {
			t.Errorf("ValidateLink(%q) = %v, want ok=%v", tc.link, err, tc.ok)
		}
	}
}

func TestValidatePostType_LinkPost(t *testing.T) {
	p := linkPlatform()

	t.Run("link only, no message, is publishable", func(t *testing.T) {
		if errs := ValidatePostType(linkPost("", "https://example.com/article"), p, nil); len(errs) != 0 {
			t.Fatalf("want no errors, got %+v", errs)
		}
	})
	t.Run("missing link", func(t *testing.T) {
		errs := ValidatePostType(linkPost("Read this", "  "), p, nil)
		if !hasRule(errs, RuleRequiresLink) || hasRule(errs, RuleRequiresContent) {
			t.Fatalf("want requires_link only, got %+v", errs)
		}
	})
	t.Run("malformed link", func(t *testing.T) {
		if errs := ValidatePostType(linkPost("Read this", "example.com"), p, nil); !hasRule(errs, RuleInvalidLink) {
			t.Fatalf("want invalid_link, got %+v", errs)
		}
	})
	t.Run("attachments are rejected", func(t *testing.T) {
		atts := []models.PostAttachment{{ID: "a1", MimeType: "image/png"}}
		if errs := ValidatePostType(linkPost("x", "https://example.com"), p, atts); !hasRule(errs, RuleMaxAttachments) {
			t.Fatalf("want max_attachments, got %+v", errs)
		}
	})
	t.Run("char limit counts the appended link", func(t *testing.T) {
		link := "https://example.com/" + strings.Repeat("p", 40)
		body := strings.Repeat("a", 50)
		if errs := ValidatePostType(linkPost(body, link), p, nil); !hasRule(errs, RuleMaxContentChars) {
			t.Fatalf("want max_content_chars once the link is counted, got %+v", errs)
		}
		if errs := ValidatePostType(linkPost(body[:30]+" "+link, link), p, nil); hasRule(errs, RuleMaxContentChars) {
			t.Fatalf("a link already in the body is not counted twice, got %+v", errs)
		}
	})
	t.Run("other types ignore a leftover link", func(t *testing.T) {
		post := &models.Post{PlatformPostType: "text-post", Content: "hi", CTAUrl: "not a url"}
		if errs := ValidatePostType(post, p, nil); len(errs) != 0 {
			t.Fatalf("want no errors, got %+v", errs)
		}
	})
}

func TestResolvePostTypeRules_LinkPost(t *testing.T) {
	for _, v := range ResolvePostTypeRules(linkPlatform()) {
		if v.Slug != models.PostTypeLinkPost {
			if v.Rule == nil || v.Rule.RequiresLink {
				t.Fatalf("%s: want a rule without requires_link, got %+v", v.Slug, v.Rule)
			}
			continue
		}
		r := v.Rule
		if v.WhitelistOnly || r == nil {
			t.Fatalf("link-post must carry a rule, got %+v", v)
		}
		if !r.RequiresLink || r.RequiresContent || r.MaxAttachments == nil || *r.MaxAttachments != 0 {
			t.Fatalf("link-post: want requires_link, optional content, max_attachments=0; got %+v", r)
		}
		return
	}
	t.Fatal("link-post missing from resolved rules")
}

func TestOutboundText(t *testing.T) {
	cases := []struct {
		name string
		post *models.Post
		want string
	}{
		{"link appended as last paragraph", linkPost("**Read** this\n", "https://example.com/a_b"), "Read this\n\nhttps://example.com/a_b"},
		{"link only", linkPost("", "https://example.com"), "https://example.com"},
		{"link already in body", linkPost("see https://example.com", "https://example.com"), "see https://example.com"},
		{"non-link type ignores cta_url", &models.Post{PlatformPostType: "text-post", Content: "hi", CTAUrl: "https://example.com"}, "hi"},
	}
	for _, tc := range cases {
		if got := OutboundText(tc.post); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

// Every type a seeded, enabled platform offers must carry a rule, so none is
// offered while the publish gate checks nothing.
func TestSeededSupportedPostTypesHaveRules(t *testing.T) {
	seeded := map[string][]string{
		"twitter":   {"text-post", "image-post", "video", "thread"},
		"linkedin":  {"text-post", "image-post", "carousel", "video", "article"},
		"facebook":  {"text-post", "image-post", "video", "reel", "link-post"},
		"instagram": {"image-post", "carousel", "reel", "story"},
		"youtube":   {"video", "short"},
		"threads":   {"text-post", "image-post", "carousel", "video", "thread"},
		"tiktok":    {"video", "image-post", "carousel"},
		"pinterest": {"image-post", "video"},
		"reddit":    {"text-post", "link-post", "image-post", "carousel", "video"},
	}
	for platform, slugs := range seeded {
		for _, slug := range slugs {
			if _, ok := RuleFor(slug); !ok {
				t.Errorf("%s offers %q, which has no rule", platform, slug)
			}
		}
	}
}
