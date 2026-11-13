package packs

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/genkit/flows/assistant"
	"github.com/ogen-app/ogen/src/genkit/flows/campaign_assistant"
	"github.com/ogen-app/ogen/src/genkit/flows/consistency"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/assistantkit"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/usecase/campaign_actions/calendarcheck"
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

func (r *memPosts) GetByID(_ context.Context, id string) (*models.Post, error) {
	if i := slices.IndexFunc(r.posts, func(p models.Post) bool { return p.ID == id }); i >= 0 {
		return new(r.posts[i]), nil
	}
	return nil, sql.ErrNoRows
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

// memDismissals keeps campaign c1's dismissals in keys, and any other
// scope's in others, by "kind|id".
type memDismissals struct {
	repository.AssistantDismissalRepository
	keys   map[string]bool
	others map[string]map[string]bool
}

func (r *memDismissals) scope(kind models.AssistantScopeKind, id string) map[string]bool {
	if kind == models.AssistantScopeCampaign && id == "c1" {
		return r.keys
	}
	if r.others == nil {
		r.others = map[string]map[string]bool{}
	}
	k := string(kind) + "|" + id
	if r.others[k] == nil {
		r.others[k] = map[string]bool{}
	}
	return r.others[k]
}

func (r *memDismissals) Dismiss(_ context.Context, kind models.AssistantScopeKind, id, key, _ string) error {
	r.scope(kind, id)[key] = true
	return nil
}

func (r *memDismissals) Keys(_ context.Context, kind models.AssistantScopeKind, id string) (map[string]bool, error) {
	return r.scope(kind, id), nil
}

func day(s string) *time.Time {
	t, _ := time.Parse(time.DateOnly, s)
	return &t
}

func repairDeps() (Deps, *memPosts, *memDismissals) {
	start, end := day("2027-03-01"), day("2027-03-31")
	camp := &models.Campaign{ID: "c1", Name: "Spring", StartDate: start, EndDate: end, PublishingDays: models.StringSlice{"mon", "tue", "wed", "thu", "fri"},
		Platforms: []models.Platform{{ID: "li", Name: "LinkedIn"}}}
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
	if sink.got[1].Tool != "generatePosts" || plan.CampaignID != "c1" || plan.WindowStart != gap.Fixes[0].From || plan.WindowEnd != gap.Fixes[0].To ||
		!slices.Equal(plan.Platforms, []string{"LinkedIn"}) || plan.Exploratory || len(sink.got[1].Options) != 0 {
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

// gapOffers runs a check and returns the ideas each silence's fix offers.
func gapOffers(t *testing.T, d Deps) ([]FindingOut, []assistantkit.Proposal) {
	t.Helper()
	sink := &offers{}
	ctx, _ := turn(t, campaign1, nil)
	out, err := checkCalendar(assistantkit.WithProposalSink(ctx, sink), d, CampaignRef{})
	if err != nil {
		t.Fatal(err)
	}
	var gaps []FindingOut
	for _, f := range out.Findings {
		if f.Code == "gap" {
			gaps = append(gaps, f)
		}
	}
	var plans []assistantkit.Proposal
	for _, p := range sink.got {
		if p.Tool == "generatePosts" {
			plans = append(plans, p)
		}
	}
	return gaps, plans
}

// A silence's ideas belong to the phase it starts in, and stop where that
// phase ends.
func TestCheckCalendar_GapOfferPhase(t *testing.T) {
	d, posts, _ := repairDeps()
	d.Campaigns.(*memCampaigns).c.CampaignType = &models.CampaignType{Phases: []models.CampaignTypePhase{
		{ID: "ph1", Name: "Tease", Sequence: 1}, {ID: "ph2", Name: "Launch", Sequence: 2}}}
	// Phases split March at the 16th; this post leaves a silence on each side.
	posts.posts = append(posts.posts, models.Post{ID: "p4", Status: models.PostStatusScheduled, PlatformID: "li", ScheduledAt: day("2027-03-18")})

	_, plans := gapOffers(t, d)
	if len(plans) != 2 {
		t.Fatalf("plans = %+v", plans)
	}
	type window struct{ phase, from, to string }
	var got []window
	for _, p := range plans {
		in := p.Args.(campaign_assistant.GeneratePostsInput)
		got = append(got, window{in.Phase, in.WindowStart, in.WindowEnd})
	}
	want := []window{{"ph1", "2027-03-05", "2027-03-16"}, {"ph2", "2027-03-19", "2027-03-31"}}
	if !slices.Equal(got, want) {
		t.Fatalf("plans = %+v, want %+v", got, want)
	}
}

// With no sources, a silence's fix is the choice to go ahead from general
// knowledge, not a plan that runs without asking.
func TestCheckCalendar_GapOfferWithoutSources(t *testing.T) {
	d, _, _ := repairDeps()
	d.Assets = memAssets{}
	_, plans := gapOffers(t, d)
	if len(plans) != 1 || len(plans[0].Options) != 1 {
		t.Fatalf("plans = %+v", plans)
	}
	if base := plans[0].Args.(campaign_assistant.GeneratePostsInput); base.Exploratory {
		t.Fatal("the offer itself plans without sources")
	}
	if o := plans[0].Options[0]; o.Tool != "generatePosts" || !o.Args.(campaign_assistant.GeneratePostsInput).Exploratory {
		t.Fatalf("option = %+v", o)
	}

	// A ready source makes it a plain offer.
	d.Assets = memAssets{all: []models.Asset{{ID: "a1", Status: "ready"}}}
	if _, plans = gapOffers(t, d); len(plans) != 1 || len(plans[0].Options) != 0 {
		t.Fatalf("plans = %+v", plans)
	}
}

// A campaign without a target platform can't have ideas planned, so its
// silence's fix asks for one instead of offering a plan that would refuse.
func TestCheckCalendar_GapOfferNeedsPlatform(t *testing.T) {
	d, _, _ := repairDeps()
	d.Campaigns.(*memCampaigns).c.Platforms = nil
	gaps, plans := gapOffers(t, d)
	if len(plans) != 0 || len(gaps) != 1 {
		t.Fatalf("gaps = %+v, plans = %+v", gaps, plans)
	}
	if fix := gaps[0].Fixes[0]; fix.ProposalID != "" || !strings.Contains(fix.Title, "target platform") {
		t.Fatalf("fix = %+v", fix)
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
	// The scheduled post is the one kept in place, for the before and after.
	if len(out.Locked) != 1 || out.Locked[0].PostID != "p3" || out.Locked[0].Date != "2027-03-04" {
		t.Fatalf("locked = %+v", out.Locked)
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

// Each brief finding gets its own brief edit and one shared by all of them;
// a post finding gets a rewrite and an open; a dismissed finding is left out.
func TestReviewCampaign_FindingsCarryFixes(t *testing.T) {
	d, posts, dismissals := repairDeps()
	posts.posts[1].Content = "twelve chars"
	fix := func(aspect string) []consistency.Fix {
		return []consistency.Fix{{Kind: consistency.FixImproveBrief, Title: "Improve the brief's " + aspect, Instruction: "fix " + aspect}}
	}
	d.CheckBrief = func(context.Context, string, consistency.OnEventFunc) (*consistency.BriefReview, error) {
		return &consistency.BriefReview{Findings: []consistency.Finding{
			{Key: "b1", Aspect: "persona", Severity: "high", Issue: "no persona", Suggestion: "name one", Fixes: fix("persona")},
			{Key: "b2", Aspect: "tone", Severity: "low", Issue: "vague", Suggestion: "pick two words", Fixes: fix("tone")},
			{Key: "b3", Aspect: "messages", Severity: "low", Issue: "dismissed", Fixes: fix("messages")},
		}}, nil
	}
	d.CheckPosts = func(context.Context, consistency.PostsCheckRequest, consistency.OnEventFunc) (*consistency.PostsReview, error) {
		return &consistency.PostsReview{Checked: 2, Findings: []consistency.PostFinding{{Key: "p", PostID: "p2", Issue: "off-brief", Fixes: []consistency.Fix{
			{Kind: consistency.FixRevisePost, Title: "Rewrite it", PostID: "p2", Instruction: "follow the brief"},
			{Kind: consistency.FixOpenPost, Title: "Open the post", PostID: "p2"},
		}}}}, nil
	}
	var revised []string
	versioned := true
	d.Revise = func(_ context.Context, id, instruction, _ string) (Revised, error) {
		revised = append(revised, id+": "+instruction)
		return Revised{Content: "short", Versioned: versioned}, nil
	}
	dismissals.keys["b3"] = true
	sink := &offers{}
	ctx, _ := turn(t, campaign1, nil)
	out, err := reviewCampaign(assistantkit.WithProposalSink(ctx, sink), d, CampaignRef{})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Findings) != 3 || out.Dismissed != 1 {
		t.Fatalf("findings = %+v, dismissed %d", out.Findings, out.Dismissed)
	}
	persona, tone, post := out.Findings[0], out.Findings[1], out.Findings[2]
	if persona.Key != "b1" || len(persona.Fixes) != 2 || len(tone.Fixes) != 2 || persona.Fixes[1].ProposalID != tone.Fixes[1].ProposalID ||
		persona.Fixes[0].ProposalID == tone.Fixes[0].ProposalID {
		t.Fatalf("brief fixes = %+v / %+v", persona.Fixes, tone.Fixes)
	}
	own := sink.got[slices.IndexFunc(sink.got, func(p assistantkit.Proposal) bool { return p.Summary == "Improve the brief's persona" })]
	if in := own.Args.(campaign_assistant.EnrichBriefInput); in.Instruction != "fix persona" || in.CampaignID != "c1" {
		t.Fatalf("persona offer = %+v", in)
	}
	if len(post.Fixes) != 2 || post.Fixes[0].Kind != consistency.FixRevisePost || post.Fixes[0].ProposalID == "" ||
		post.Fixes[1].Kind != calendarcheck.FixOpenPost || post.Fixes[1].ProposalID != "" {
		t.Fatalf("post fixes = %+v", post.Fixes)
	}
	rewrite := sink.got[slices.IndexFunc(sink.got, func(p assistantkit.Proposal) bool { return p.Tool == "revisePost" })]
	// The rewrite says how long the copy was and is.
	if out, err := revisePost(ctx, d, rewrite.Args.(RevisePostInput)); err != nil || !slices.Equal(revised, []string{"p2: follow the brief"}) ||
		out.Length == nil || *out.Length != (Lengths{Before: 12, After: 5}) {
		t.Fatalf("revise = %v, %+v, %v", revised, out, err)
	}
	// A rewrite whose version couldn't be saved says so.
	versioned = false
	if out, err := revisePost(ctx, d, rewrite.Args.(RevisePostInput)); err != nil || !out.Revised || out.Versioned {
		t.Fatalf("unversioned rewrite = %+v, %v", out, err)
	}
	// A scheduled post's copy is locked.
	if _, err := revisePost(ctx, d, RevisePostInput{PostID: "p3", Instruction: "x"}); !isRefusal(err) {
		t.Fatalf("scheduled post = %v", err)
	}
}

// A finding is dismissed for the campaign checked, whichever thread checked
// it, so the Overview leaves it out too; one dismissed in a workspace thread
// before dismissals followed the campaign is still left out there.
func TestDismissFinding_FollowsTheCampaign(t *testing.T) {
	d, _, dismissals := repairDeps()
	ctx, _ := turn(t, workspace, nil)
	ctx = assistantkit.WithProposalSink(ctx, &offers{})
	out, err := checkCalendar(ctx, d, CampaignRef{CampaignID: "c1"})
	if err != nil || len(out.Findings) < 2 {
		t.Fatalf("out = %+v, %v", out, err)
	}
	if _, err := dismissFinding(ctx, d, DismissFindingInput{}); !isRefusal(err) {
		t.Fatalf("no key = %v", err)
	}
	if _, err := dismissFinding(ctx, d, DismissFindingInput{Key: "k"}); !isRefusal(err) {
		t.Fatalf("a workspace thread named no campaign = %v", err)
	}
	if _, err := dismissFinding(ctx, d, DismissFindingInput{Key: out.Findings[0].Key, CampaignID: "c1"}); err != nil {
		t.Fatal(err)
	}
	if !dismissals.keys[out.Findings[0].Key] {
		t.Fatalf("dismissed in %v, not for the campaign", dismissals.others)
	}
	dismissals.scope(models.AssistantScopeWorkspace, "")[out.Findings[1].Key] = true
	after, err := checkCalendar(ctx, d, CampaignRef{CampaignID: "c1"})
	if err != nil || after.Dismissed != 2 {
		t.Fatalf("after = %+v, %v", after, err)
	}
	// The campaign's own thread sees only the campaign's.
	cctx, _ := turn(t, campaign1, nil)
	in, err := checkCalendar(assistantkit.WithProposalSink(cctx, &offers{}), d, CampaignRef{})
	if err != nil || in.Dismissed != 1 {
		t.Fatalf("campaign thread = %+v, %v", in, err)
	}
}

// A rewrite keeps the post's grounding: its sources with the campaign's
// analysis of each, exploratory as exploratory, and none when it was never
// known.
func TestRevisePost_KeepsThePostsGrounding(t *testing.T) {
	d, posts, _ := repairDeps()
	d.Revise = func(context.Context, string, string, string) (Revised, error) { return Revised{Versioned: true}, nil }
	d.Assets = memAssets{all: []models.Asset{{ID: "a1", Title: "Brochure"}}}
	d.Analyses = gistAnalyses{}
	posts.posts[0].GroundingMode, posts.posts[0].UsedAssetIDs = new(models.GroundingGrounded), models.StringSlice{"a1"}
	posts.posts[1].GroundingMode = new(models.GroundingExploratory)
	ctx, _ := turn(t, campaign1, nil)

	out, err := revisePost(ctx, d, RevisePostInput{PostID: "p1", Instruction: "tighter"})
	if err != nil || out.Grounding == nil || !slices.Equal(out.Grounding.Sources, []assistantkit.Source{{ID: "a1", Title: "Brochure", AnalysisID: "an-a1"}}) {
		t.Fatalf("grounded = %+v, %v", out, err)
	}
	if out, err = revisePost(ctx, d, RevisePostInput{PostID: "p2", Instruction: "tighter"}); err != nil || out.Grounding == nil || out.Grounding.Mode != assistantkit.Exploratory {
		t.Fatalf("exploratory = %+v, %v", out, err)
	}
	posts.posts[1].GroundingMode = nil
	if out, err = revisePost(ctx, d, RevisePostInput{PostID: "p2", Instruction: "tighter"}); err != nil || out.Grounding != nil {
		t.Fatalf("unknown = %+v, %v", out, err)
	}
}
