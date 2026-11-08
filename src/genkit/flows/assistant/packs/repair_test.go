package packs

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/genkit/flows/assistant"
	"github.com/ogen-app/ogen/src/genkit/flows/campaign_assistant"
	"github.com/ogen-app/ogen/src/genkit/flows/consistency"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/assistantkit"
	"github.com/ogen-app/ogen/src/infra/repository"
)

// offers records what tools offered.
type offers struct{ got []assistantkit.Proposal }

func (o *offers) Propose(_ context.Context, p assistantkit.Proposal) (string, error) {
	o.got = append(o.got, p)
	return fmt.Sprint("offer-", len(o.got)), nil
}

type memPosts struct {
	repository.PostRepository
	posts []models.Post
	moved []repository.ScheduleMove
	// raced are posts edited between the read and the write.
	raced map[string]bool
}

func (r *memPosts) ListByCampaign(context.Context, string) ([]models.Post, error) {
	return slices.Clone(r.posts), nil
}

func (r *memPosts) MoveScheduledAt(_ context.Context, ms []repository.ScheduleMove, _ time.Time) ([]string, error) {
	var ids []string
	for _, m := range ms {
		if r.raced[m.PostID] {
			continue
		}
		r.moved = append(r.moved, m)
		ids = append(ids, m.PostID)
	}
	return ids, nil
}

type memDismissals struct {
	repository.AssistantDismissalRepository
	keys map[string]bool
}

func (r *memDismissals) Dismiss(_ context.Context, _ models.AssistantScopeKind, _, key, _ string) error {
	r.keys[key] = true
	return nil
}

func (r *memDismissals) Keys(context.Context, models.AssistantScopeKind, string) (map[string]bool, error) {
	return r.keys, nil
}

func day(s string) *time.Time {
	t, _ := time.Parse(time.DateOnly, s)
	return &t
}

func repairDeps() (Deps, *memPosts, *memDismissals) {
	start, end := day("2027-03-01"), day("2027-03-31")
	camp := &models.Campaign{ID: "c1", Name: "Spring", StartDate: start, EndDate: end, PublishingDays: models.StringSlice{"mon", "tue", "wed", "thu", "fri"}}
	posts := &memPosts{posts: []models.Post{
		{ID: "p1", Title: "Early", Status: models.PostStatusDraft, PlatformID: "li", ScheduledAt: day("2027-02-20")},
		{ID: "p2", Title: "Mid", Status: models.PostStatusReadyForPublish, PlatformID: "li", ScheduledAt: day("2027-03-03")},
		{ID: "p3", Title: "Out", Status: models.PostStatusScheduled, PlatformID: "li", ScheduledAt: day("2027-03-04")},
	}}
	dismissals := &memDismissals{keys: map[string]bool{}}
	d := Deps{Campaigns: &memCampaigns{c: camp}, Posts: posts, Dismissals: dismissals,
		Campaign: &assistant.Pack{Name: "campaign"}}
	return d, posts, dismissals
}

func TestCheckCalendar_OffersFixes(t *testing.T) {
	d, _, dismissals := repairDeps()
	sink := &offers{}
	ctx, _ := turn(t, campaign1, nil)
	ctx = assistantkit.WithProposalSink(ctx, sink)

	out, err := checkCalendar(ctx, d, CampaignRef{})
	if err != nil {
		t.Fatal(err)
	}
	var outside, gap *FindingOut
	for i := range out.Findings {
		switch out.Findings[i].Code {
		case "outside_window":
			outside = &out.Findings[i]
		case "gap":
			gap = &out.Findings[i]
		}
	}
	if outside == nil || gap == nil {
		t.Fatalf("findings = %+v", out.Findings)
	}
	// One redistribution offer, with the exact moves; one plan-ideas offer.
	if outside.Fixes[0].ProposalID == "" || gap.Fixes[0].ProposalID == "" || len(sink.got) != 2 {
		t.Fatalf("offers = %+v; fixes = %+v / %+v", sink.got, outside.Fixes, gap.Fixes)
	}
	moves := sink.got[0].Args.(ApplyMovesInput)
	if sink.got[0].Tool != "applyCalendarMoves" || moves.CampaignID != "c1" || len(moves.Moves) == 0 {
		t.Fatalf("moves offer = %+v", sink.got[0])
	}
	// Filling a silence plans ideas for its dates, through the campaign's
	// own tool, naming the campaign so it can run from any screen.
	plan, _ := sink.got[1].Args.(campaign_assistant.GeneratePostsInput)
	if sink.got[1].Tool != "generatePosts" || plan.CampaignID != "c1" || plan.WindowStart != gap.Fixes[0].From || plan.WindowEnd != gap.Fixes[0].To {
		t.Fatalf("gap offer = %+v", sink.got[1])
	}

	// Dismissed findings stay out.
	if _, err := dismissFinding(ctx, d, DismissFindingInput{Key: gap.Key}); err != nil {
		t.Fatal(err)
	}
	if !dismissals.keys[gap.Key] {
		t.Fatal("not dismissed")
	}
	out, _ = checkCalendar(ctx, d, CampaignRef{})
	if out.Dismissed != 1 || slices.ContainsFunc(out.Findings, func(f FindingOut) bool { return f.Code == "gap" }) {
		t.Fatalf("after dismissing: %+v", out)
	}
}

func TestApplyMoves_SkipsPostsChangedSince(t *testing.T) {
	d, posts, _ := repairDeps()
	posts.posts = append(posts.posts,
		models.Post{ID: "p4", Status: models.PostStatusDraft, ScheduledAt: day("2027-03-05")},
		models.Post{ID: "p5", Status: models.PostStatusDraft, ScheduledAt: day("2027-03-05")},
		models.Post{ID: "p6", Status: models.PostStatusDraft, ScheduledAt: day("2027-03-05")})
	posts.raced = map[string]bool{"p6": true}
	retimed := day("2027-03-03").Add(10 * time.Hour)
	ctx, touched := turn(t, campaign1, nil)
	out, err := applyMoves(ctx, d, ApplyMovesInput{CampaignID: "c1", Moves: []Move{
		{PostID: "p1", From: "2027-02-20", FromAt: day("2027-02-20"), To: "2027-03-02"},
		{PostID: "p2", From: "2027-03-03", FromAt: &retimed, To: "2027-03-08"},          // retimed the same day since
		{PostID: "p3", From: "2027-03-04", FromAt: day("2027-03-04"), To: "2027-03-09"}, // scheduled: committed
		{PostID: "gone", To: "2027-03-10"},                                              // deleted
		{PostID: "p4", From: "2027-03-05", FromAt: day("2027-03-05"), To: "2027-03-06"}, // a Saturday
		{PostID: "p5", From: "2027-03-05", FromAt: day("2027-03-05"), To: "2027-04-01"}, // after the campaign
		{PostID: "p6", From: "2027-03-05", FromAt: day("2027-03-05"), To: "2027-03-10"}, // edited during the write
	}})
	if err != nil {
		t.Fatal(err)
	}
	if out.Moved != 1 || !slices.Equal(out.Skipped, []string{"p2", "p3", "gone", "p4", "p5", "p6"}) {
		t.Fatalf("out = %+v", out)
	}
	// The write is conditional on the offered schedule, and lands at the
	// campaign's publishing time.
	want := repository.ScheduleMove{PostID: "p1", From: day("2027-02-20"), To: time.Date(2027, 3, 2, 9, 0, 0, 0, time.UTC)}
	if got := posts.moved[0]; got.PostID != want.PostID || !got.From.Equal(*want.From) || !got.To.Equal(want.To) {
		t.Fatalf("moved = %+v, want %+v", got, want)
	}
	if !slices.Equal(touched(), []assistantkit.Touched{{Kind: "post"}}) {
		t.Fatalf("touched = %v", touched())
	}
}

func TestOptimiseCalendar(t *testing.T) {
	d, _, _ := repairDeps()
	sink := &offers{}
	ctx, _ := turn(t, campaign1, nil)
	out, err := optimiseCalendar(assistantkit.WithProposalSink(ctx, sink), d, CampaignRef{})
	if err != nil || out.ProposalID == "" || len(out.Moves) == 0 {
		t.Fatalf("out = %+v, %v", out, err)
	}
	for _, m := range out.Moves {
		if m.PostID == "p3" {
			t.Fatal("a scheduled post was moved")
		}
		if wd := day(m.To).Weekday(); wd == time.Saturday || wd == time.Sunday {
			t.Fatalf("%s moved to a %s, a day the campaign doesn't publish", m.PostID, wd)
		}
	}
	undated := Deps{Campaigns: &memCampaigns{c: &models.Campaign{ID: "c1"}}, Posts: &memPosts{}}
	_, err = optimiseCalendar(ctx, undated, CampaignRef{})
	refused(t, err)
}

// Moves are local days in the campaign's timezone, offered with the exact
// schedule they move from, and applied at its publishing time there.
func TestCalendarMoves_CampaignTimezone(t *testing.T) {
	d, posts, _ := repairDeps()
	camp := d.Campaigns.(*memCampaigns).c
	camp.Timezone, camp.PublishingTime = "America/Los_Angeles", "10:00"
	// 03:00 UTC on 20 February is the 19th in Los Angeles.
	early := day("2027-02-20").Add(3 * time.Hour)
	posts.posts[0].ScheduledAt = &early

	ctx, _ := turn(t, campaign1, nil)
	out, err := optimiseCalendar(assistantkit.WithProposalSink(ctx, &offers{}), d, CampaignRef{})
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(out.Moves, func(m Move) bool { return m.PostID == "p1" })
	if i < 0 || out.Moves[i].From != "2027-02-19" || !out.Moves[i].FromAt.Equal(early) {
		t.Fatalf("moves = %+v", out.Moves)
	}

	if _, err := applyMoves(ctx, d, ApplyMovesInput{CampaignID: "c1", Moves: []Move{out.Moves[i]}}); err != nil {
		t.Fatal(err)
	}
	la, _ := time.LoadLocation("America/Los_Angeles")
	if got := posts.moved[0].To.In(la); got.Format(time.DateOnly) != out.Moves[i].To || got.Hour() != 10 {
		t.Fatalf("moved to %v, want %s 10:00 in Los Angeles", got, out.Moves[i].To)
	}
}

// Dismissing one of two findings of a code leaves the other raised.
func TestCheckCalendar_DismissesOneFindingOfACode(t *testing.T) {
	d, posts, _ := repairDeps()
	posts.posts = append(posts.posts, models.Post{ID: "p4", Status: models.PostStatusDraft, PlatformID: "li", ScheduledAt: day("2027-03-15")})
	ctx, _ := turn(t, campaign1, nil)
	ctx = assistantkit.WithProposalSink(ctx, &offers{})
	gaps := func() []FindingOut {
		out, err := checkCalendar(ctx, d, CampaignRef{})
		if err != nil {
			t.Fatal(err)
		}
		var gs []FindingOut
		for _, f := range out.Findings {
			if f.Code == "gap" {
				gs = append(gs, f)
			}
		}
		return gs
	}
	before := gaps()
	if len(before) != 2 || before[0].Key == before[1].Key {
		t.Fatalf("gaps = %+v", before)
	}
	if _, err := dismissFinding(ctx, d, DismissFindingInput{Key: before[0].Key}); err != nil {
		t.Fatal(err)
	}
	if after := gaps(); len(after) != 1 || after[0].Key != before[1].Key {
		t.Fatalf("after dismissing one: %+v", after)
	}
}

func TestReviewCampaign_FindingsCarryFixes(t *testing.T) {
	d, _, _ := repairDeps()
	d.CheckBrief = func(context.Context, string, consistency.OnEventFunc) (*consistency.BriefReview, error) {
		return &consistency.BriefReview{Findings: []consistency.Finding{
			{Aspect: "persona", Severity: "high", Issue: "no persona", Suggestion: "name one"},
			{Aspect: "tone", Severity: "low", Issue: "vague", Suggestion: "pick two words"},
		}}, nil
	}
	d.CheckPosts = func(context.Context, consistency.PostsCheckRequest, consistency.OnEventFunc) (*consistency.PostsReview, error) {
		return &consistency.PostsReview{Checked: 2, Findings: []consistency.PostFinding{{PostID: "p2", Issue: "off-brief"}}}, nil
	}
	sink := &offers{}
	ctx, _ := turn(t, campaign1, nil)
	out, err := reviewCampaign(assistantkit.WithProposalSink(ctx, sink), d, CampaignRef{})
	if err != nil {
		t.Fatal(err)
	}
	// One brief edit for both brief findings; the post finding opens the post.
	if len(out.Findings) != 3 || len(sink.got) != 1 || out.Findings[0].Fix.ProposalID != "offer-1" || out.Findings[1].Fix.ProposalID != "offer-1" ||
		out.Findings[2].Fix.PostID != "p2" || out.Findings[2].Fix.ProposalID != "" {
		t.Fatalf("findings = %+v, offers = %d", out.Findings, len(sink.got))
	}
}
