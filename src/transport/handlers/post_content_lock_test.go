package handlers

import (
	"testing"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/usecase/post_actions/update"
)

// mutatesLockedContent is the pure comparison behind the submitted-
// post 409 content lock. It must flag a change to any content-identity field
// (body/title/link/media/platform/post-type/sources) and ignore everything else,
// so a no-op save or a status-only transition off a submitted post still
// passes. Pure logic — no DB needed.
func TestMutatesLockedContent(t *testing.T) {
	base := func() *models.Post {
		return &models.Post{
			Content:          "hello world",
			Title:            "a title",
			CTAUrl:           "https://example.com/a",
			PlatformID:       "plat_1",
			PlatformPostType: "feed",
			MediaURLs:        models.StringSlice{"a", "b"},
			UsedAssetIDs:     models.StringSlice{"src_1"},
		}
	}
	// reqFor builds a request that mirrors the post exactly (a no-op save). Sources
	// are presence-aware, so a faithful mirror sends them present.
	reqFor := func(p *models.Post) postRequest {
		return postRequest{
			Content:          p.Content,
			Title:            p.Title,
			CTAUrl:           p.CTAUrl,
			PlatformID:       p.PlatformID,
			PlatformPostType: p.PlatformPostType,
			MediaURLs:        p.MediaURLs,
			UsedAssetIDs:     present(p.UsedAssetIDs),
		}
	}

	p := base()
	noop := reqFor(p)
	if noop.mutatesLockedContent(p) {
		t.Fatal("an identical (no-op) request must not count as a content mutation")
	}

	// Each locked field, changed in isolation, must trip the lock.
	locked := []struct {
		name string
		edit func(r *postRequest)
	}{
		{"content", func(r *postRequest) { r.Content = "rewritten" }},
		{"title", func(r *postRequest) { r.Title = "new title" }},
		{"link", func(r *postRequest) { r.CTAUrl = "https://example.com/b" }},
		{"platform", func(r *postRequest) { r.PlatformID = "plat_2" }},
		{"post_type", func(r *postRequest) { r.PlatformPostType = "story" }},
		{"media_removed", func(r *postRequest) { r.MediaURLs = models.StringSlice{"a"} }},
		{"media_reordered", func(r *postRequest) { r.MediaURLs = models.StringSlice{"b", "a"} }},
		{"sources", func(r *postRequest) { r.UsedAssetIDs = present(models.StringSlice{"src_1", "src_2"}) }},
	}
	for _, tc := range locked {
		r := reqFor(p)
		tc.edit(&r)
		if !r.mutatesLockedContent(p) {
			t.Errorf("changing %s must count as a locked-content mutation", tc.name)
		}
	}

	// A save that OMITS used_asset_ids preserves the set (the membership
	// endpoints own it), so it must not count as touching the locked sources —
	// even though the post has sources the request doesn't restate.
	omitsSources := reqFor(p)
	omitsSources.UsedAssetIDs = Optional[models.StringSlice]{} // key absent
	if omitsSources.mutatesLockedContent(p) {
		t.Error("omitting used_asset_ids must not count as a content mutation")
	}

	// content_format is presence-aware: omitted preserves it, a present value
	// different from the stored one (including null) is a mutation.
	explainer := models.ContentFormatExplainer
	withFormat := base()
	withFormat.ContentFormat = &explainer
	formatCases := []struct {
		name   string
		format Optional[models.ContentFormat]
		want   bool
	}{
		{"omitted", Optional[models.ContentFormat]{}, false},
		{"same", present(models.ContentFormatExplainer), false},
		{"changed", present(models.ContentFormatHowTo), true},
		{"cleared", Optional[models.ContentFormat]{Present: true}, true},
	}
	for _, tc := range formatCases {
		r := reqFor(withFormat)
		r.ContentFormat = tc.format
		if got := r.mutatesLockedContent(withFormat); got != tc.want {
			t.Errorf("content_format %s: mutatesLockedContent = %v, want %v", tc.name, got, tc.want)
		}
	}
	digestID, otherID := "series-1", "series-2"
	inSeries := base()
	inSeries.SeriesID = &digestID
	seriesCases := []struct {
		name   string
		series Optional[string]
		want   bool
	}{
		{"omitted", Optional[string]{}, false},
		{"same", present(digestID), false},
		{"changed", present(otherID), true},
		{"cleared", Optional[string]{Present: true}, true},
	}
	for _, tc := range seriesCases {
		r := reqFor(inSeries)
		r.SeriesID = tc.series
		if got := r.mutatesLockedContent(inSeries); got != tc.want {
			t.Errorf("series_id %s: mutatesLockedContent = %v, want %v", tc.name, got, tc.want)
		}
	}

	setOnUnformatted := reqFor(p)
	setOnUnformatted.ContentFormat = present(models.ContentFormatHowTo)
	if !setOnUnformatted.mutatesLockedContent(p) {
		t.Error("setting a format on a post without one must count as a content mutation")
	}

	// Fields the lock deliberately does NOT own (date/account/cta_type/notes/status
	// are governed elsewhere) must not trip it — that's what lets a status-only
	// unschedule through on a submitted post.
	unlocked := reqFor(p)
	unlocked.Status = models.PostStatusDraft
	unlocked.CTAType = models.CTATypeNone
	unlocked.TargetAudienceNotes = "reminder"
	unlocked.SocialAccountID = "acct_9"
	if unlocked.mutatesLockedContent(p) {
		t.Error("changing only non-locked fields (status/cta_type/notes/account) must not count as a content mutation")
	}
}

// A thread's content lock rides on the canonical body — the whole
// thread lives in Content, so editing it trips the lock while the derived (and
// ignored) thread_segments field does not. Pure logic — no DB.
func TestMutatesLockedContentThread(t *testing.T) {
	post := &models.Post{
		PlatformPostType: models.PostTypeThread,
		Content:          "root\n\n---\n\nreply",
	}
	mirror := postRequest{
		PlatformPostType: post.PlatformPostType,
		Content:          post.Content,
	}
	if mirror.mutatesLockedContent(post) {
		t.Fatal("mirroring the thread body exactly must not count as a mutation")
	}

	edited := mirror
	edited.Content = "root\n\n---\n\nchanged"
	if !edited.mutatesLockedContent(post) {
		t.Error("editing the thread body must count as a locked-content mutation")
	}

	// thread_segments is server-derived: with the body unchanged, a stray client
	// array must NOT count as a mutation (the lock ignores it entirely).
	strayArray := mirror
	strayArray.ThreadSegments = models.ThreadSegments{{Content: "root"}, {Content: "different"}}
	if strayArray.mutatesLockedContent(post) {
		t.Error("a client-sent thread_segments with an unchanged body must not count as a mutation")
	}
}

// update.ApplyThreadSegments derives the segment list from the canonical
// body for a thread (WITHOUT restamping the body) and clears it for any other
// type. Pure — the caller supplies the per-segment limit.
func TestApplyThreadSegments(t *testing.T) {
	// Manual delimiter: two segments, body preserved verbatim.
	post := &models.Post{PlatformPostType: models.PostTypeThread, Content: "root\n\n---\n\nreply"}
	update.ApplyThreadSegments(post, 280)
	if len(post.ThreadSegments) != 2 || post.ThreadSegments[0].Content != "root" || post.ThreadSegments[1].Content != "reply" {
		t.Errorf("manual thread: segments = %+v, want [root reply]", post.ThreadSegments)
	}
	if post.Content != "root\n\n---\n\nreply" {
		t.Errorf("manual thread: Content = %q, want the body unchanged (not restamped)", post.Content)
	}

	// Auto-split: a delimiter-free body over the limit splits by the limit.
	auto := &models.Post{PlatformPostType: models.PostTypeThread, Content: "aaaaa\n\nbbbbb"}
	update.ApplyThreadSegments(auto, 5)
	if len(auto.ThreadSegments) != 2 {
		t.Errorf("auto thread: segments = %d, want 2", len(auto.ThreadSegments))
	}

	// Demotion / non-thread: segments cleared, body kept.
	demote := &models.Post{
		PlatformPostType: "text-post",
		Content:          "just this",
		ThreadSegments:   models.ThreadSegments{{Content: "old"}, {Content: "seg"}},
	}
	update.ApplyThreadSegments(demote, 280)
	if len(demote.ThreadSegments) != 0 {
		t.Errorf("demote: segments = %d, want 0", len(demote.ThreadSegments))
	}
	if demote.Content != "just this" {
		t.Errorf("demote: Content = %q, want %q", demote.Content, "just this")
	}
}
