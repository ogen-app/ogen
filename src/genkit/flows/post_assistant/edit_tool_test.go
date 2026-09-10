package post_assistant

import (
	"strings"
	"testing"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/usecase/post_actions/schedule"
)

// The editPost tool must reject an empty instruction before it ever spins up
// the writer sub-call, and must not record an edit result.
func TestToolEditPost_RequiresInstruction(t *testing.T) {
	st := &requestState{postID: "p1"}
	ctx := withRequestState(t.Context(), st)
	if _, err := toolEditPost(ctx, EditPostInput{Instruction: "   "}); err == nil {
		t.Fatal("expected an error for an empty instruction")
	}
	if st.editResult != nil {
		t.Fatal("editResult must stay nil when no writer ran")
	}
}

// CON-251: a schedule committed earlier in the same generation must lock the
// content against a later editPost. The editPost guard reads postStatus, so
// schedulePost has to advance it — otherwise edit-after-schedule in one turn
// would rewrite a post whose content just locked at schedule time. Exercise the
// state seam recordScheduled fills, then confirm editPost refuses gracefully.
func TestToolEditPost_RefusesAfterScheduleThisTurn(t *testing.T) {
	st := &requestState{postID: "p1", postStatus: models.PostStatusDraft}

	// schedulePost commits an auto-publish schedule this turn.
	st.recordScheduled(&schedule.Result{Status: models.PostStatusScheduled})
	if st.postStatus != models.PostStatusScheduled {
		t.Fatalf("recordScheduled must advance postStatus to the routed status; got %q", st.postStatus)
	}

	ctx := withRequestState(t.Context(), st)
	out, err := toolEditPost(ctx, EditPostInput{Instruction: "make it punchier"})
	if err != nil {
		// A start-of-turn status that was never refreshed would fall through to
		// the (unavailable) writer and error — the regression this guards.
		t.Fatalf("editPost after schedule must refuse gracefully, not error: %v", err)
	}
	if out == nil || out.OK {
		t.Fatal("editPost must refuse once the post was scheduled to auto-publish this turn")
	}
	if st.editResult != nil {
		t.Fatal("editResult must stay nil when the post locked this turn")
	}
}

// A manual-publish schedule leaves nothing outside Ogen yet, so the content
// stays editable within the same turn — recordScheduled advances the status but
// IsSubmitted (and thus the editPost guard) excludes it.
func TestToolEditPost_ManualScheduleStaysEditable(t *testing.T) {
	st := &requestState{postID: "p1", postStatus: models.PostStatusDraft}
	st.recordScheduled(&schedule.Result{Status: models.PostStatusScheduledForManualPublish})

	ctx := withRequestState(t.Context(), st)
	// Writer is unavailable in this bare state, so a permitted edit reaches it
	// and surfaces the write-content error — proving the lock did NOT engage.
	_, err := toolEditPost(ctx, EditPostInput{Instruction: "make it punchier"})
	if err == nil || !strings.Contains(err.Error(), "write content") {
		t.Fatalf("a manual-publish schedule must leave content editable (reach the writer); got err=%v", err)
	}
}

// runWriter is a no-op in the legacy state (no genkit instance / provider /
// writer system prompt) — the guard keeps a stray call from panicking and
// clearly reports the writer is unavailable.
func TestRunWriter_Unavailable(t *testing.T) {
	st := &requestState{postID: "p1"} // g / provider / writerSystem all zero
	if _, err := runWriter(t.Context(), st, "shorten it", false); err == nil {
		t.Fatal("expected an error when the writer is unavailable")
	}
}

// A well-formed editPost call whose writer is unavailable surfaces a wrapped
// write-content error and leaves editResult unset, so the runner never
// finalises a bogus "edited" turn.
func TestToolEditPost_WriterUnavailable(t *testing.T) {
	st := &requestState{postID: "p1"} // writerSystem empty → runWriter errors
	ctx := withRequestState(t.Context(), st)
	_, err := toolEditPost(ctx, EditPostInput{Instruction: "make it punchier"})
	if err == nil {
		t.Fatal("expected an error when content writing is unavailable")
	}
	if !strings.Contains(err.Error(), "write content") {
		t.Fatalf("expected a wrapped write-content error, got: %v", err)
	}
	if st.editResult != nil {
		t.Fatal("editResult must stay nil on writer failure")
	}
}

// The writer must receive the full retrieved excerpts as source material —
// alongside the unchanged, verbatim instruction — so an asset-grounded edit
// grounds on the retrieved text rather than the short preview (CON-128).
func TestComposeWriterInstruction_IncludesRetrievedExcerpts(t *testing.T) {
	instruction := "Add a section on goroutine scheduling from the whitepaper."
	excerpts := []retrievedExcerpt{
		{AssetID: "asset1", ChunkID: "c1", Content: "Goroutines are multiplexed onto OS threads by the Go runtime scheduler."},
	}

	out := composeWriterInstruction(instruction, excerpts)

	if !strings.HasPrefix(out, instruction) {
		t.Fatalf("the verbatim instruction must lead the writer prompt; got: %q", out)
	}
	if !strings.Contains(out, "multiplexed onto OS threads") {
		t.Fatalf("the retrieved excerpt content must reach the writer as source material; got: %q", out)
	}
	if !strings.Contains(out, "Source material") {
		t.Fatalf("excerpts should be labelled as source material; got: %q", out)
	}

	// No retrieval this turn → the instruction is passed through untouched.
	if got := composeWriterInstruction(instruction, nil); got != instruction {
		t.Fatalf("with no excerpts the instruction must be unchanged; got: %q", got)
	}
}

// Asset-retrieval tools may return the same chunk more than once across a turn;
// captureExcerpts must record each chunk once so the writer prompt isn't padded
// with duplicates.
func TestCaptureExcerpts_DedupesByChunkID(t *testing.T) {
	st := &requestState{}
	out := &ChunksOutput{Chunks: []ChunkContent{
		{ID: "c1", Content: "alpha"},
		{ID: "c2", Content: "beta"},
	}}

	st.captureExcerpts("a1", out)
	st.captureExcerpts("a1", out) // same chunks retrieved again

	if len(st.retrieved) != 2 {
		t.Fatalf("expected 2 deduped excerpts, got %d", len(st.retrieved))
	}
	if st.retrieved[0].Content != "alpha" || st.retrieved[1].Content != "beta" {
		t.Fatalf("captured excerpts lost their content: %+v", st.retrieved)
	}
}
