package models

import (
	"testing"
)

// The CON-130 edges: a scheduled post can be converted straight to
// manual publishing, and a manually-scheduled post can go straight to
// drafts — neither existed before and both are load-bearing for the
// convert-to-manual flow and channel-removal-to-drafts.
func TestCanTransitionCON130Edges(t *testing.T) {
	cases := []struct {
		from PostStatus
		to   PostStatus
		want bool
	}{
		// New CON-130 edges.
		{PostStatusScheduled, PostStatusScheduledForManualPublish, true},
		{PostStatusScheduledForManualPublish, PostStatusDraft, true},

		// Pre-existing edges still hold.
		{PostStatusScheduled, PostStatusReadyForPublish, true},
		{PostStatusScheduled, PostStatusDraft, true},
		{PostStatusReadyForPublish, PostStatusScheduled, true},
		{PostStatusScheduledForManualPublish, PostStatusPublished, true},

		// Still-illegal edges: manual publish can't jump back to auto
		// scheduled, and draft still can't leap straight to scheduled.
		{PostStatusScheduledForManualPublish, PostStatusScheduled, false},
		{PostStatusDraft, PostStatusScheduled, false},

		// Identity is always allowed.
		{PostStatusScheduled, PostStatusScheduled, true},
	}
	for _, tc := range cases {
		if got := tc.from.CanTransition(tc.to); got != tc.want {
			t.Errorf("CanTransition(%q -> %q) = %v, want %v", tc.from, tc.to, got, tc.want)
		}
	}
}

// CON-251: failed and not_published reopen straight to draft (the copy
// outside Ogen is gone, or never left, so an edit is the point). The
// pre-existing → ready_for_publish edges still hold, and neither
// submitted state gains a → draft shortcut here.
func TestCanTransitionCON251ReopenEdges(t *testing.T) {
	cases := []struct {
		from PostStatus
		to   PostStatus
		want bool
	}{
		// New CON-251 reopen edges.
		{PostStatusFailed, PostStatusDraft, true},
		{PostStatusNotPublished, PostStatusDraft, true},

		// Pre-existing reopen edges still hold.
		{PostStatusFailed, PostStatusReadyForPublish, true},
		{PostStatusNotPublished, PostStatusReadyForPublish, true},
		{PostStatusNotPublished, PostStatusScheduledForManualPublish, true},

		// The submitted states are NOT reopened to draft by this change —
		// their copy still lives outside Ogen (scheduled → draft was already
		// legal via cancel; published has no outgoing edge at all).
		{PostStatusPublished, PostStatusDraft, false},
	}
	for _, tc := range cases {
		if got := tc.from.CanTransition(tc.to); got != tc.want {
			t.Errorf("CanTransition(%q -> %q) = %v, want %v", tc.from, tc.to, got, tc.want)
		}
	}
}

// CON-251: IsSubmitted is true exactly for the two states that hold a copy
// outside Ogen — scheduled (Zernio) and published (the network) — and
// false for every editable state, including scheduled_for_manual_publishing
// (nobody holds it yet).
func TestIsSubmitted(t *testing.T) {
	submitted := map[PostStatus]bool{
		PostStatusScheduled: true,
		PostStatusPublished: true,
	}
	all := []PostStatus{
		PostStatusDraft, PostStatusReadyForPublish, PostStatusScheduled,
		PostStatusScheduledForManualPublish, PostStatusFailed,
		PostStatusPublished, PostStatusNotPublished,
	}
	for _, s := range all {
		if got := s.IsSubmitted(); got != submitted[s] {
			t.Errorf("IsSubmitted(%q) = %v, want %v", s, got, submitted[s])
		}
	}
}

// CON-284: IsThread keys off the post-type slug alone.
func TestIsThread(t *testing.T) {
	if (&Post{PlatformPostType: PostTypeThread}).IsThread() != true {
		t.Error("thread post: want IsThread=true")
	}
	if (&Post{PlatformPostType: "text-post"}).IsThread() != false {
		t.Error("text-post: want IsThread=false")
	}
	if (&Post{}).IsThread() != false {
		t.Error("empty post type: want IsThread=false")
	}
}

// CON-284: ThreadSegments round-trips through the jsonb Value/Scan pair, a nil
// slice serialises as an empty array (not JSON null, so the NOT NULL column
// holds), and RootContent returns index 0.
func TestThreadSegmentsValueScan(t *testing.T) {
	// nil normalises to "[]".
	v, err := ThreadSegments(nil).Value()
	if err != nil || v.(string) != "[]" {
		t.Fatalf("nil Value() = %v, %v; want \"[]\"", v, err)
	}

	orig := ThreadSegments{{Content: "root"}, {Content: "reply"}}
	raw, err := orig.Value()
	if err != nil {
		t.Fatalf("Value() error: %v", err)
	}
	var back ThreadSegments
	if err := back.Scan(raw.(string)); err != nil {
		t.Fatalf("Scan() error: %v", err)
	}
	if len(back) != 2 || back[0].Content != "root" || back[1].Content != "reply" {
		t.Fatalf("round-trip mismatch: %+v", back)
	}

	// Scanning SQL NULL yields an empty (non-nil) slice.
	var fromNull ThreadSegments
	if err := fromNull.Scan(nil); err != nil || fromNull == nil || len(fromNull) != 0 {
		t.Fatalf("Scan(nil) = %+v, %v; want empty non-nil slice", fromNull, err)
	}

	if orig.RootContent() != "root" {
		t.Errorf("RootContent() = %q, want %q", orig.RootContent(), "root")
	}
	if (ThreadSegments{}).RootContent() != "" {
		t.Error("empty RootContent(): want \"\"")
	}
}

// CON-284 R2: SnapshotContent is just Content for every post type — a thread's
// Content is now the canonical full body (with "---" delimiters), so it already
// records the whole chain and is injective on its own.
func TestSnapshotContent(t *testing.T) {
	plain := &Post{PlatformPostType: "text-post", Content: "hello"}
	if got := plain.SnapshotContent(); got != "hello" {
		t.Errorf("ordinary post: SnapshotContent() = %q, want %q", got, "hello")
	}

	// A thread snapshots its full body verbatim — every message is captured.
	body := "root\n\n---\n\nreply"
	thread := &Post{
		PlatformPostType: PostTypeThread,
		Content:          body,
		ThreadSegments:   ThreadSegments{{Content: "root"}, {Content: "reply"}},
	}
	if got := thread.SnapshotContent(); got != body {
		t.Errorf("thread SnapshotContent() = %q, want the full body %q", got, body)
	}

	// Injectivity: distinct thread bodies yield distinct snapshots.
	other := &Post{PlatformPostType: PostTypeThread, Content: "root\n\n---\n\ndifferent"}
	if other.SnapshotContent() == thread.SnapshotContent() {
		t.Error("distinct thread bodies produced identical SnapshotContent()")
	}
}
