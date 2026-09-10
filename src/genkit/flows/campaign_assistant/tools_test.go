package campaign_assistant

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/genkit/flows/content_plan"
)

func day(s string) time.Time {
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		panic(err)
	}
	return t
}

func ptrTime(t time.Time) *time.Time { return &t }

func timelineCampaign() *models.Campaign {
	return &models.Campaign{
		StartDate: ptrTime(day("2026-01-01")),
		EndDate:   ptrTime(day("2026-01-30")), // 30 days, 3 phases → 10 days each
		CampaignType: &models.CampaignType{
			Phases: []models.CampaignTypePhase{
				{ID: "p1", Name: "Hook", Sequence: 1},
				{ID: "p2", Name: "Nurture", Sequence: 2},
				{ID: "p3", Name: "Convert", Sequence: 3},
			},
		},
		Platforms: []models.Platform{
			{ID: "pl1", Name: "LinkedIn"},
			{ID: "pl2", Name: "Threads"},
		},
	}
}

func TestCurrentPhase_Timeline(t *testing.T) {
	c := timelineCampaign()
	cases := []struct{ today, want string }{
		{"2026-01-05", "p1"}, // window 1: 01-01..01-10
		{"2026-01-15", "p2"}, // window 2: 01-11..01-20
		{"2026-01-28", "p3"}, // window 3: 01-21..01-30
		{"2025-12-20", "p1"}, // before start → first
		{"2026-03-01", "p3"}, // after end → last
	}
	for _, tc := range cases {
		if got := currentPhase(c, day(tc.today)); got.ID != tc.want {
			t.Errorf("today %s: currentPhase = %q, want %q", tc.today, got.ID, tc.want)
		}
	}
}

func TestCurrentPhase_NoDatesFallsBackToFirst(t *testing.T) {
	c := timelineCampaign()
	c.StartDate, c.EndDate = nil, nil
	if got := currentPhase(c, day("2026-01-15")); got.ID != "p1" {
		t.Fatalf("no-dates fallback = %q, want p1", got.ID)
	}
}

func TestResolvePhase(t *testing.T) {
	c := timelineCampaign()
	today := day("2026-01-15") // → p2 for "current"

	for _, tc := range []struct{ in, wantID string }{
		{"current", "p2"},
		{"", "p2"},
		{"Nurture", "p2"}, // by name
		{"p3", "p3"},      // by id
	} {
		id, _, err := resolvePhase(c, tc.in, today)
		if err != nil {
			t.Fatalf("resolvePhase(%q): %v", tc.in, err)
		}
		if id != tc.wantID {
			t.Errorf("resolvePhase(%q) = %q, want %q", tc.in, id, tc.wantID)
		}
	}

	if _, _, err := resolvePhase(c, "Nonexistent", today); err == nil {
		t.Fatal("expected error for unknown phase")
	}
}

func TestResolveTargetPlatforms(t *testing.T) {
	c := timelineCampaign()

	ids, names, err := resolveTargetPlatforms(c, []string{"Threads"})
	if err != nil {
		t.Fatalf("by name: %v", err)
	}
	if len(ids) != 1 || ids[0] != "pl2" || names[0] != "Threads" {
		t.Fatalf("by name = %v / %v", ids, names)
	}

	// Case-insensitive name + id both resolve; dupes collapse.
	ids, _, err = resolveTargetPlatforms(c, []string{"threads", "pl2", "pl1"})
	if err != nil {
		t.Fatalf("mixed: %v", err)
	}
	if len(ids) != 2 {
		t.Fatalf("dupes should collapse, got %v", ids)
	}

	// A non-target platform is rejected (offer-to-add path).
	if _, _, err := resolveTargetPlatforms(c, []string{"TikTok"}); err == nil {
		t.Fatal("expected error for a non-target platform")
	}
}

// TestGeneratePosts_SoftFailsUserInput verifies that generatePosts does NOT
// abort the turn (return a Go error) for user-correctable input — a past date,
// a non-target platform, or an unknown phase. Instead it returns a zero-post
// result carrying the reason as a warning, so the assistant can relay it and
// suggest a valid alternative (CON-215). A Go error here would surface to the
// user as a raw "model call failed".
func TestGeneratePosts_SoftFailsUserInput(t *testing.T) {
	newState := func() *requestState {
		return &requestState{
			campaignID:       "c1",
			campaign:         timelineCampaign(), // no repos → ensureCampaignAssetUse no-ops
			maxGeneratePosts: 10,
			generatePosts: func(context.Context, content_plan.GeneratePostsRequest, content_plan.OnEventFunc) (*content_plan.ContentPlanResponse, error) {
				t.Fatal("generation engine must not run for invalid input")
				return nil, nil
			},
		}
	}

	cases := []struct {
		name string
		in   GeneratePostsInput
	}{
		// 2020 is unambiguously before any real "today", so this is clock-safe.
		{"past window", GeneratePostsInput{Platforms: []string{"Threads"}, WindowStart: "2020-01-01", WindowEnd: "2020-01-01"}},
		{"non-target platform", GeneratePostsInput{Platforms: []string{"TikTok"}}},
		{"unknown phase", GeneratePostsInput{Platforms: []string{"Threads"}, Phase: "Nonexistent"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newState()
			ctx := withRequestState(context.Background(), st)
			out, err := toolGeneratePosts(ctx, tc.in)
			if err != nil {
				t.Fatalf("want soft failure (nil error), got %v", err)
			}
			if out == nil || out.PostCount != 0 || len(out.Warnings) == 0 {
				t.Fatalf("want zero posts + a warning, got %+v", out)
			}
			// Must stay conversational: a soft failure never records a generated-posts result.
			if st.generatedPostsResult != nil {
				t.Fatalf("soft failure must not set generatedPostsResult")
			}
		})
	}
}

// TestGeneratePosts_HeavySlotOnlyBurnedOnSuccess guards the CON-216 fix: the
// heavy-action reservation moved to AFTER generatePosts' user-correctable
// validations (mirroring draftPost). So a generatePosts that fails soft on a
// past date / non-target platform / unknown phase — including a mis-routed one
// where the planner replayed a stale past-date instruction onto a read-only
// question — must NOT consume the turn's single heavy slot, leaving it free for
// a legitimate heavy action in the same turn. A successful generation still
// claims the slot so a second heavy tool is turned away.
func TestGeneratePosts_HeavySlotOnlyBurnedOnSuccess(t *testing.T) {
	t.Run("soft failure leaves the slot free", func(t *testing.T) {
		st := &requestState{
			campaignID:       "c1",
			campaign:         timelineCampaign(), // no repos → ensureCampaignAssetUse no-ops
			maxGeneratePosts: 10,
			generatePosts: func(context.Context, content_plan.GeneratePostsRequest, content_plan.OnEventFunc) (*content_plan.ContentPlanResponse, error) {
				t.Fatal("generation engine must not run for invalid input")
				return nil, nil
			},
		}
		ctx := withRequestState(context.Background(), st)
		// 2020 is unambiguously before any real "today", so this is clock-safe.
		if _, err := toolGeneratePosts(ctx, GeneratePostsInput{Platforms: []string{"Threads"}, WindowStart: "2020-01-01", WindowEnd: "2020-01-01"}); err != nil {
			t.Fatalf("want soft failure (nil error), got %v", err)
		}
		if st.heavyReserved {
			t.Fatal("a soft-failed generatePosts must not reserve the heavy slot")
		}
		if !st.reserveHeavyAction() {
			t.Fatal("the heavy slot must still be claimable after a soft failure")
		}
	})

	t.Run("success claims the slot", func(t *testing.T) {
		st := &requestState{
			campaignID:       "c1",
			campaign:         timelineCampaign(),
			maxGeneratePosts: 10,
			generatePosts: func(context.Context, content_plan.GeneratePostsRequest, content_plan.OnEventFunc) (*content_plan.ContentPlanResponse, error) {
				return &content_plan.ContentPlanResponse{Posts: []content_plan.DraftPost{{Title: "t", PublishDate: "2026-01-15"}}}, nil
			},
		}
		ctx := withRequestState(context.Background(), st)
		// Omit the window → defaults to the next 14 days from today (clock-safe).
		out, err := toolGeneratePosts(ctx, GeneratePostsInput{Platforms: []string{"Threads"}})
		if err != nil {
			t.Fatalf("valid generation: %v", err)
		}
		if out == nil || out.PostCount != 1 {
			t.Fatalf("want one generated post, got %+v", out)
		}
		if !st.heavyReserved {
			t.Fatal("a successful generatePosts must reserve the heavy slot")
		}
		if st.reserveHeavyAction() {
			t.Fatal("a second heavy reservation must be turned away after a successful generation")
		}
	})
}

func TestResolveWindow(t *testing.T) {
	today := day("2026-02-10")

	// Omitted → next 14 days.
	s, e, err := resolveWindow("", "", today)
	if err != nil {
		t.Fatalf("default: %v", err)
	}
	if s != "2026-02-10" || e != "2026-02-24" {
		t.Fatalf("default window = %s..%s", s, e)
	}

	// Valid future window passes through.
	s, e, err = resolveWindow("2026-02-15", "2026-02-28", today)
	if err != nil || s != "2026-02-15" || e != "2026-02-28" {
		t.Fatalf("passthrough = %s..%s err=%v", s, e, err)
	}

	// Past start is rejected, not clamped (CON-114: never date drafts in the past).
	if _, _, err := resolveWindow("2026-01-01", "2026-02-28", today); err == nil {
		t.Fatal("expected error for a past windowStart")
	}
	// Today itself is allowed (equal, not before).
	if s, _, err := resolveWindow("2026-02-10", "", today); err != nil || s != "2026-02-10" {
		t.Fatalf("today start = %s err=%v", s, err)
	}

	// End before start → error.
	if _, _, err := resolveWindow("2026-02-20", "2026-02-15", today); err == nil {
		t.Fatal("expected error when end precedes start")
	}
	// Bad format → error.
	if _, _, err := resolveWindow("nope", "2026-02-15", today); err == nil {
		t.Fatal("expected error for bad windowStart")
	}

	// Only a start → end derived as start + 14 days (the "upcoming weeks" bug:
	// the model resolves a start and omits the end). Must not error.
	s, e, err = resolveWindow("2026-02-10", "", today)
	if err != nil || s != "2026-02-10" || e != "2026-02-24" {
		t.Fatalf("start-only = %s..%s err=%v", s, e, err)
	}

	// Only an end → start defaults to today.
	s, e, err = resolveWindow("", "2026-02-28", today)
	if err != nil || s != "2026-02-10" || e != "2026-02-28" {
		t.Fatalf("end-only = %s..%s err=%v", s, e, err)
	}

	// A non-empty but malformed end is still rejected.
	if _, _, err := resolveWindow("2026-02-15", "nope", today); err == nil {
		t.Fatal("expected error for bad windowEnd")
	}
}

// CON-118: page citation rendering for asset Q&A excerpts.
func TestPageRef(t *testing.T) {
	p := func(i int) *int { return &i }
	cases := []struct {
		start, end *int
		want       string
	}{
		{nil, nil, ""},
		{p(4), nil, "p. 4"},
		{p(4), p(4), "p. 4"},
		{p(3), p(5), "pp. 3-5"},
	}
	for _, c := range cases {
		if got := pageRef(c.start, c.end); got != c.want {
			t.Errorf("pageRef(%v, %v) = %q, want %q", c.start, c.end, got, c.want)
		}
	}
}

// CON-114: a count the user names is honored exactly; an omitted/zero count
// defaults to 1 (the safe minimum) so an omitting planner can't over-produce.
// Guards the "generate 1 post" -> 3 regression.
func TestResolveGenerateCount(t *testing.T) {
	cases := []struct {
		name        string
		requested   int
		maxN        int
		wantCount   int
		wantReq     int
		wantClamped bool
	}{
		{"exact one is honored", 1, 10, 1, 1, false},
		{"exact five is honored", 5, 10, 5, 5, false},
		{"omitted defaults to 1 (safe minimum)", 0, 10, 1, 1, false},
		{"negative treated as omitted", -2, 10, 1, 1, false},
		{"above cap clamps down", 25, 10, 10, 25, true},
		{"unset cap falls back to 10", 25, 0, 10, 25, true},
	}
	for _, c := range cases {
		gotCount, gotReq, gotClamped := resolveGenerateCount(c.requested, c.maxN)
		if gotCount != c.wantCount || gotReq != c.wantReq || gotClamped != c.wantClamped {
			t.Errorf("%s: resolveGenerateCount(%d, %d) = (count=%d, requested=%d, clamped=%v); want (count=%d, requested=%d, clamped=%v)",
				c.name, c.requested, c.maxN, gotCount, gotReq, gotClamped, c.wantCount, c.wantReq, c.wantClamped)
		}
	}
}

// CON-213: the per-turn heavy-action latch. Exactly one reservation succeeds
// per requestState; every later caller is turned away and skips its sub-flow.
func TestReserveHeavyAction_Sequential(t *testing.T) {
	st := &requestState{}
	if !st.reserveHeavyAction() {
		t.Fatal("first reservation must succeed")
	}
	for i := range 3 {
		if st.reserveHeavyAction() {
			t.Fatalf("reservation %d must fail once the slot is taken", i+2)
		}
	}
}

// CON-213: genkit dispatches a turn's tool calls in parallel goroutines, so the
// reservation must admit exactly one winner even under simultaneous contention
// (guards against the TOCTOU/data race a completion-based check had). Run under
// -race to also catch unsynchronised access.
func TestReserveHeavyAction_Concurrent(t *testing.T) {
	st := &requestState{}
	const goroutines = 64

	var start sync.WaitGroup
	var done sync.WaitGroup
	start.Add(1)
	done.Add(goroutines)

	var wins int64
	var mu sync.Mutex
	for range goroutines {
		go func() {
			defer done.Done()
			start.Wait() // release all goroutines at once to maximise contention
			if st.reserveHeavyAction() {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}()
	}
	start.Done()
	done.Wait()

	if wins != 1 {
		t.Fatalf("exactly one reservation must win, got %d", wins)
	}
}

// CON-213: the max-tool-iterations abort is detected by its stable message
// substring so the turn degrades gracefully instead of 502ing.
func TestIsMaxTurnsExceeded(t *testing.T) {
	if isMaxTurnsExceeded(nil) {
		t.Fatal("nil error is not a max-turns abort")
	}
	if isMaxTurnsExceeded(errors.New("some other failure")) {
		t.Fatal("unrelated error must not match")
	}
	// The genkit message form (ai/generate.go), verbatim and wrapped.
	if !isMaxTurnsExceeded(errors.New("exceeded maximum tool call iterations (2)")) {
		t.Fatal("genkit abort message must match")
	}
	if !isMaxTurnsExceeded(fmtWrap(errors.New("exceeded maximum tool call iterations (4)"))) {
		t.Fatal("wrapped genkit abort message must match")
	}
}

func fmtWrap(err error) error { return errors.Join(errors.New("model call failed"), err) }

// CON-114: a single post with only a start date is pinned to that day, so
// "generate 1 for Jul 22" lands on Jul 22 instead of the midpoint of the
// derived 14-day window. Explicit ends and multi-post requests keep their range.
func TestSinglePostWindowEnd(t *testing.T) {
	cases := []struct {
		name               string
		start, end, rawEnd string
		count              int
		want               string
	}{
		{"one post, derived end -> pinned to start", "2026-07-22", "2026-08-05", "", 1, "2026-07-22"},
		{"one post, explicit end -> range kept", "2026-07-22", "2026-07-29", "2026-07-29", 1, "2026-07-29"},
		{"multi post, derived end -> range kept", "2026-07-22", "2026-08-05", "", 3, "2026-08-05"},
	}
	for _, c := range cases {
		if got := singlePostWindowEnd(c.start, c.end, c.rawEnd, c.count); got != c.want {
			t.Errorf("%s: singlePostWindowEnd(%q, %q, %q, %d) = %q, want %q",
				c.name, c.start, c.end, c.rawEnd, c.count, got, c.want)
		}
	}
}
