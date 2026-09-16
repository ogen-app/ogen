package platforms

import (
	"strings"
	"testing"

	"github.com/ogen-app/ogen/src/domain/models"
)

// threadPlatform mirrors the X (Twitter) seed relevant to threads: a 280-char
// per-segment limit and a 4-image-per-post cap that the per-segment media check
// reuses.
func threadPlatform() *models.Platform {
	return &models.Platform{
		ID:   "xplat",
		Name: "X",
		PostTypes: models.PostTypeMap{
			"text-post": "Text post",
			"thread":    "Thread (multi-post sequence)",
		},
		TextConstraints:  models.TextConstraints{MaxContentChars: 280},
		ImageConstraints: models.ImageConstraints{MaxFileSizeBytes: 5 << 20, AllowedFormats: []string{"jpeg", "png"}, MaxAttachmentsPerPost: 4},
	}
}

func segIdx(i int) *int { return &i }

func threadPost(contents ...string) *models.Post {
	segs := make(models.ThreadSegments, len(contents))
	for i, c := range contents {
		segs[i] = models.ThreadSegment{Content: c}
	}
	root := ""
	if len(segs) > 0 {
		root = segs[0].Content
	}
	return &models.Post{PlatformPostType: models.PostTypeThread, Content: root, ThreadSegments: segs}
}

func TestValidateThread_SegmentCount(t *testing.T) {
	p := threadPlatform()

	if errs := ValidatePostType(threadPost("only one"), p, nil); !hasRule(errs, RuleThreadSegmentCount) {
		t.Errorf("single segment: want thread_segment_count, got %+v", errs)
	}

	over := make([]string, MaxThreadSegments+1)
	for i := range over {
		over[i] = "msg"
	}
	if errs := ValidatePostType(threadPost(over...), p, nil); !hasRule(errs, RuleThreadSegmentCount) {
		t.Errorf("over-cap segments: want thread_segment_count, got %+v", errs)
	}

	if errs := ValidatePostType(threadPost("one", "two"), p, nil); hasRule(errs, RuleThreadSegmentCount) {
		t.Errorf("two segments: want no count error, got %+v", errs)
	}
}

func TestValidateThread_SegmentText(t *testing.T) {
	p := threadPlatform()

	if errs := ValidatePostType(threadPost("ok", "   "), p, nil); !hasRule(errs, RuleRequiresContent) {
		t.Errorf("blank segment: want requires_content, got %+v", errs)
	}

	over := threadPost("ok", strings.Repeat("a", 281))
	errs := ValidatePostType(over, p, nil)
	if !hasRule(errs, RuleMaxContentChars) {
		t.Fatalf("over-limit segment: want max_content_chars, got %+v", errs)
	}
	// The failure names the offending segment (index 1).
	if !hasSegment(errs, RuleMaxContentChars, 1) {
		t.Errorf("over-limit segment: want Segment=1 on the error, got %+v", errs)
	}

	if errs := ValidatePostType(threadPost(strings.Repeat("a", 280), "reply"), p, nil); hasRule(errs, RuleMaxContentChars) {
		t.Errorf("at-limit segment: want no error, got %+v", errs)
	}

	// CON-284 defect 2: the per-segment limit governs the flattened caption, not
	// the Markdown syntax. 280 visible chars wrapped in bold markers is raw-length
	// 284 but publishes 280 — it must pass, matching the composer's counter.
	bold := "**" + strings.Repeat("a", 280) + "**"
	if errs := ValidatePostType(threadPost("ok", bold), p, nil); hasRule(errs, RuleMaxContentChars) {
		t.Errorf("bold at visible limit: want no error, got %+v", errs)
	}
	// 281 visible chars is over regardless of the markers.
	tooLong := "**" + strings.Repeat("a", 281) + "**"
	if errs := ValidatePostType(threadPost("ok", tooLong), p, nil); !hasRule(errs, RuleMaxContentChars) {
		t.Errorf("bold over visible limit: want max_content_chars, got %+v", errs)
	}
}

func TestValidateThread_SegmentIndexIntegrity(t *testing.T) {
	p := threadPlatform()
	post := threadPost("root", "reply")

	// CON-284 R2: a nil segment_index is valid — the attachment defaults to the
	// root message (segment 0), so it must NOT raise an integrity error.
	nilIdx := []models.PostAttachment{{ID: "a", MimeType: "image/jpeg", SizeBytes: 100}}
	if errs := ValidatePostType(post, p, nilIdx); hasRule(errs, RuleThreadSegmentIndex) {
		t.Errorf("nil segment_index (defaults to root): want no integrity error, got %+v", errs)
	}

	// Out-of-range (2 into a 2-message thread).
	oor := []models.PostAttachment{{ID: "a", MimeType: "image/jpeg", SizeBytes: 100, SegmentIndex: segIdx(2)}}
	if errs := ValidatePostType(post, p, oor); !hasRule(errs, RuleThreadSegmentIndex) {
		t.Errorf("out-of-range segment_index: want thread_segment_index, got %+v", errs)
	}

	// A valid in-range index is clean.
	ok := []models.PostAttachment{{ID: "a", MimeType: "image/jpeg", SizeBytes: 100, SegmentIndex: segIdx(0)}}
	if errs := ValidatePostType(post, p, ok); hasRule(errs, RuleThreadSegmentIndex) {
		t.Errorf("valid segment_index: want no integrity error, got %+v", errs)
	}
}

// CON-284 R2: nil-index attachments count against the ROOT segment's media rules,
// so five of them trip X's 4-image cap at segment 0 (proving NULL ⇒ root).
func TestValidateThread_NilIndexMediaLandsOnRoot(t *testing.T) {
	p := threadPlatform() // 4 images per post → per segment
	post := threadPost("root", "reply")

	five := make([]models.PostAttachment, 5)
	for i := range five {
		five[i] = models.PostAttachment{ID: "img", MimeType: "image/jpeg", SizeBytes: 100, Position: i}
	}
	errs := ValidatePostType(post, p, five)
	if !hasRule(errs, RuleMaxAttachmentsCount) {
		t.Fatalf("5 nil-index images: want max_attachments_per_post at root, got %+v", errs)
	}
	if !hasSegment(errs, RuleMaxAttachmentsCount, 0) {
		t.Errorf("nil-index cap failure: want Segment=0 (root), got %+v", errs)
	}
}

func TestValidateThread_PerSegmentMediaCap(t *testing.T) {
	p := threadPlatform() // 4 images per post → per segment here
	post := threadPost("root", "reply")

	// Five images all in segment 0 exceeds the 4-image cap for that segment.
	five := make([]models.PostAttachment, 5)
	for i := range five {
		five[i] = models.PostAttachment{ID: "img", MimeType: "image/jpeg", SizeBytes: 100, Position: i, SegmentIndex: segIdx(0)}
	}
	errs := ValidatePostType(post, p, five)
	if !hasRule(errs, RuleMaxAttachmentsCount) {
		t.Fatalf("5 images in one segment: want max_attachments_per_post, got %+v", errs)
	}
	if !hasSegment(errs, RuleMaxAttachmentsCount, 0) {
		t.Errorf("per-segment cap failure: want Segment=0, got %+v", errs)
	}

	// The same 8 images split 4+4 across two segments is fine — the cap is
	// per segment, not per post.
	split := make([]models.PostAttachment, 8)
	for i := range split {
		split[i] = models.PostAttachment{ID: "img", MimeType: "image/jpeg", SizeBytes: 100, Position: i, SegmentIndex: segIdx(i / 4)}
	}
	if errs := ValidatePostType(post, p, split); hasRule(errs, RuleMaxAttachmentsCount) {
		t.Errorf("4+4 images across two segments: want no cap error, got %+v", errs)
	}
}

// TestValidatePublishReadiness_ThreadVsWholePost proves the router runs the
// per-segment gate for threads (so 4+4 images passes) but still enforces the
// whole-post cap for an ordinary post (8 images fails).
func TestValidatePublishReadiness_ThreadVsWholePost(t *testing.T) {
	p := threadPlatform()

	split := make([]models.PostAttachment, 8)
	for i := range split {
		split[i] = models.PostAttachment{ID: "img", MimeType: "image/jpeg", SizeBytes: 100, Position: i, SegmentIndex: segIdx(i / 4)}
	}
	thread := threadPost("root", "reply")
	if got := ValidatePublishReadiness(thread, p, split); hasAnyThreadErr(got) {
		t.Errorf("thread 4+4: want clean, got %+v", got)
	}

	// Same 8 images on an ordinary image-post: the whole-post cap fires.
	whole := make([]models.PostAttachment, 8)
	for i := range whole {
		whole[i] = models.PostAttachment{ID: "img", MimeType: "image/jpeg", SizeBytes: 100, Position: i}
	}
	imagePost := &models.Post{PlatformPostType: "image-post", Content: "x"}
	if got := ValidatePublishReadiness(imagePost, p, whole); !hasAnyThreadErr(got) {
		t.Errorf("image-post 8 images: want max_attachments_per_post, got %+v", got)
	}
}

func TestValidateThread_HappyPath(t *testing.T) {
	p := threadPlatform()
	post := threadPost("root message", "second message")
	atts := []models.PostAttachment{
		{ID: "a", MimeType: "image/jpeg", SizeBytes: 100, Position: 0, SegmentIndex: segIdx(0)},
		{ID: "b", MimeType: "image/png", SizeBytes: 100, Position: 1, SegmentIndex: segIdx(1)},
	}
	if errs := ValidatePostType(post, p, atts); len(errs) != 0 {
		t.Errorf("valid thread: want no errors, got %+v", errs)
	}
}

func TestResolvePostTypeRules_ThreadSegmented(t *testing.T) {
	views := ResolvePostTypeRules(threadPlatform())
	byslug := map[string]PostTypeRuleView{}
	for _, v := range views {
		byslug[v.Slug] = v
	}
	if r := byslug["thread"].Rule; r == nil || !r.Segmented {
		t.Errorf("thread: want segmented=true, got %+v", byslug["thread"].Rule)
	}
	if r := byslug["thread"].Rule; r == nil || r.MaxContentChars == nil || *r.MaxContentChars != 280 {
		t.Errorf("thread: want per-segment max_content_chars=280, got %+v", byslug["thread"].Rule)
	}
	if r := byslug["text-post"].Rule; r == nil || r.Segmented {
		t.Errorf("text-post: want segmented=false, got %+v", byslug["text-post"].Rule)
	}
}

// hasSegment reports whether errs carries a failure for the given rule stamped
// with the given 0-based segment index.
func hasSegment(errs []ValidationError, rule string, segment int) bool {
	for _, e := range errs {
		if e.Rule == rule && e.Segment != nil && *e.Segment == segment {
			return true
		}
	}
	return false
}

func hasAnyThreadErr(m map[string][]ValidationError) bool {
	for _, v := range m {
		if len(v) > 0 {
			return true
		}
	}
	return false
}
