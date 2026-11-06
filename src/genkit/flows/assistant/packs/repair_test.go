package packs

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
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
	moved []*models.Post
}

func (r *memPosts) ListByCampaign(context.Context, string) ([]models.Post, error) {
	return slices.Clone(r.posts), nil
}

func (r *memPosts) UpdateScheduledAtBatch(_ context.Context, ps []*models.Post) error {
	r.moved = append(r.moved, ps...)
	return nil
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
		Campaign: func(context.Context, campaignRequest, campaignEvents) (*campaignResponse, error) { return nil, nil }}
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
	if sink.got[1].Tool != "workOnCampaign" {
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
	ctx, touched := turn(t, campaign1, nil)
	out, err := applyMoves(ctx, d, ApplyMovesInput{CampaignID: "c1", Moves: []Move{
		{PostID: "p1", From: "2027-02-20", To: "2027-03-02"},
		{PostID: "p2", From: "2027-03-01", To: "2027-03-08"}, // moved since the offer
		{PostID: "p3", From: "2027-03-04", To: "2027-03-09"}, // scheduled: committed
		{PostID: "gone", To: "2027-03-10"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if out.Moved != 1 || !slices.Equal(out.Skipped, []string{"p2", "p3", "gone"}) || posts.moved[0].ScheduledAt.Format(time.DateOnly) != "2027-03-02" {
		t.Fatalf("out = %+v", out)
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
	}
	undated := Deps{Campaigns: &memCampaigns{c: &models.Campaign{ID: "c1"}}, Posts: &memPosts{}}
	_, err = optimiseCalendar(ctx, undated, CampaignRef{})
	refused(t, err)
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
	if len(out.Findings) != 3 || len(sink.got) != 1 || out.Findings[0].Fix.ProposalID != out.Findings[1].Fix.ProposalID ||
		out.Findings[2].Fix.PostID != "p2" || out.Findings[2].Fix.ProposalID != "" {
		t.Fatalf("findings = %+v, offers = %d", out.Findings, len(sink.got))
	}
}
