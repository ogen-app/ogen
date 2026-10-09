package handlers

import (
	"slices"
	"testing"

	"github.com/ogen-app/ogen/src/domain/models"
)

func TestPostRequestFirstComment(t *testing.T) {
	stored := func() *models.Post {
		return &models.Post{FirstComment: "link below", FirstCommentDelayMinutes: 3}
	}

	// Omitted fields leave the stored comment alone and aren't written back.
	var omitted postRequest
	p := stored()
	omitted.apply(p, models.PostStatusDraft)
	if p.FirstComment != "link below" || p.FirstCommentDelayMinutes != 3 {
		t.Errorf("omitted fields changed the post: %+v", p)
	}
	if omitted.mutatesLockedContent(stored()) {
		t.Error("omitting the first comment must not count as a content change")
	}
	for _, col := range []string{"first_comment", "first_comment_delay_minutes", "first_comment_status", "first_comment_id"} {
		if !slices.Contains(omitted.omitColumns(), col) {
			t.Errorf("omitColumns() lacks %s", col)
		}
	}

	// A present comment is trimmed; null clears it.
	set := postRequest{FirstComment: present("  new link  "), FirstCommentDelayMinutes: present(10)}
	p = stored()
	set.apply(p, models.PostStatusDraft)
	if p.FirstComment != "new link" || p.FirstCommentDelayMinutes != 10 {
		t.Errorf("apply = %q / %d", p.FirstComment, p.FirstCommentDelayMinutes)
	}
	cleared := postRequest{FirstComment: Optional[string]{Present: true}}
	p = stored()
	cleared.apply(p, models.PostStatusDraft)
	if p.FirstComment != "" {
		t.Errorf("null should clear the comment, got %q", p.FirstComment)
	}

	// Writing a PUT never writes the workers' outcome columns, even with the
	// comment present.
	if cols := set.omitColumns(); slices.Contains(cols, "first_comment") || !slices.Contains(cols, "first_comment_status") {
		t.Errorf("omitColumns() with the comment present = %v", cols)
	}

	// The comment is locked content once the post is submitted.
	same := postRequest{FirstComment: present("link below "), FirstCommentDelayMinutes: present(3)}
	if same.mutatesLockedContent(stored()) {
		t.Error("restating the stored comment must not count as a change")
	}
	for name, r := range map[string]postRequest{
		"text":  {FirstComment: present("other")},
		"delay": {FirstCommentDelayMinutes: present(5)},
	} {
		if !r.mutatesLockedContent(stored()) {
			t.Errorf("changing the first comment %s must count as a content change", name)
		}
	}
}

func TestValidateFirstCommentDelay(t *testing.T) {
	for _, r := range []postRequest{{}, {FirstCommentDelayMinutes: present(0)}, {FirstCommentDelayMinutes: present(10)}} {
		if err := r.validateFirstCommentDelay(); err != nil {
			t.Errorf("%+v: unexpected %v", r.FirstCommentDelayMinutes, err)
		}
	}
	for _, r := range []postRequest{{FirstCommentDelayMinutes: present(2)}, {FirstCommentDelayMinutes: Optional[int]{Present: true}}} {
		if err := r.validateFirstCommentDelay(); err == nil {
			t.Errorf("%+v: want an error", r.FirstCommentDelayMinutes)
		}
	}
}
