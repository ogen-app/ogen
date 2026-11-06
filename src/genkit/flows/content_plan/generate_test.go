package content_plan

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
)

// The streaming persist path stops at the batch's requested count, so
// an over-producing model can't turn "generate exactly 1" into 3 persisted
// posts. expectedCount <= 0 is the uncapped fallback (model decides the count).
func TestWithinCount(t *testing.T) {
	cases := []struct {
		name                string
		persisted, expected int
		want                bool
	}{
		{"first post for a 1-post batch", 0, 1, true},
		{"second post exceeds a 1-post batch", 1, 1, false},
		{"under a 3-post batch", 2, 3, true},
		{"at a 3-post batch cap", 3, 3, false},
		{"uncapped (zero)", 5, 0, true},
		{"uncapped (negative)", 5, -1, true},
	}
	for _, c := range cases {
		if got := withinCount(c.persisted, c.expected); got != c.want {
			t.Errorf("%s: withinCount(%d, %d) = %v, want %v", c.name, c.persisted, c.expected, got, c.want)
		}
	}
}

// stubIdeaRepo embeds the interface so it satisfies the type; only Create is
// implemented (the rest panic if unexpectedly called).
type stubIdeaRepo struct {
	repository.IdeaRepository
	created *models.Idea
}

func (s *stubIdeaRepo) Create(_ context.Context, i *models.Idea) error {
	s.created = i
	return nil
}

// A planned item is saved as the campaign's idea: its thesis is the note,
// its platform, post type, phase and date are the plan, and nothing becomes
// a post.
func TestPersistIdea_SavesThePlan(t *testing.T) {
	ideas := &stubIdeaRepo{}
	campaign := &models.Campaign{ID: "camp1", CreatedBy: "user1"}
	dp := DraftPost{
		Title: "Why pricing pages fail", Body: "- point 1\n- point 2", ToneNotes: "dry humour",
		PlatformID: "linkedin", ContentType: "article", PhaseID: "ph1", PublishDate: "2026-08-12",
	}
	id, err := persistIdea(t.Context(), &dp, campaign, nil, nil, ideas, []string{"asset-1", "asset-2"})
	if err != nil {
		t.Fatal(err)
	}
	i := ideas.created
	if i == nil || i.ID != id || i.Title != dp.Title || i.Origin != models.IdeaOriginContentPlan || *i.CampaignID != "camp1" || *i.CreatedBy != "user1" {
		t.Fatalf("idea = %+v", i)
	}
	if i.Note != "- point 1\n- point 2\n\nTone: dry humour" {
		t.Errorf("note = %q", i.Note)
	}
	if p := i.Plan; *p.PlatformID != "linkedin" || *p.PostType != "article" || *p.PhaseID != "ph1" || p.TargetDate.String() != "2026-08-12" {
		t.Errorf("plan = %+v", p)
	}
	if len(i.AssetIDs) != 2 || *i.SourceAssetID != "asset-1" || *i.GroundingMode != models.GroundingGrounded {
		t.Errorf("sources = %v / %v, grounding %v", i.AssetIDs, i.SourceAssetID, i.GroundingMode)
	}
}

// An idea that drew on no source is exploratory, with no plan fields the
// model left out.
func TestPersistIdea_Exploratory(t *testing.T) {
	ideas := &stubIdeaRepo{}
	dp := DraftPost{Title: "T", Body: "  "}
	if _, err := persistIdea(t.Context(), &dp, &models.Campaign{ID: "c", CreatedBy: "u"}, nil, nil, ideas, nil); err != nil {
		t.Fatal(err)
	}
	i := ideas.created
	if *i.GroundingMode != models.GroundingExploratory || i.SourceAssetID != nil || len(i.AssetIDs) != 0 || i.Note != "" {
		t.Fatalf("idea = %+v", i)
	}
	if p := i.Plan; p.PlatformID != nil || p.PostType != nil || p.PhaseID != nil || p.TargetDate != nil {
		t.Fatalf("plan = %+v; a malformed date and empty fields set nothing", p)
	}
}

// Each idea records whether it drew on the campaign's sources. It goes
// through the persist step generation uses, which binds each idea to the
// retrieved assets it cites.
func TestPersistCited_RecordsGrounding(t *testing.T) {
	cases := map[string]struct {
		cites    []string
		wantUsed []string
		want     models.GroundingMode
	}{
		"cites a retrieved asset": {[]string{"asset-1"}, []string{"asset-1"}, models.GroundingGrounded},
		"cites none":              {nil, nil, models.GroundingExploratory},
		"cites an unknown id":     {[]string{"made-up"}, nil, models.GroundingExploratory},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			ideas := &stubIdeaRepo{}
			campaign := &models.Campaign{ID: "camp1", CreatedBy: "user1"}
			persist := persistCited(campaign, planScope{}, ContentPlanRepos{Ideas: ideas}, []resolvedPiece{{ID: "asset-1", Title: "Report"}}, nil)
			dp := DraftPost{Title: "T", PlatformID: "linkedin", ContentType: "article", AssetRefs: tc.cites}
			if _, err := persist(t.Context(), &dp); err != nil {
				t.Fatal(err)
			}
			i := ideas.created
			if !slices.Equal(i.AssetIDs, tc.wantUsed) {
				t.Errorf("used assets = %v, want %v", i.AssetIDs, tc.wantUsed)
			}
			if g := i.GroundingMode; g == nil || *g != tc.want {
				t.Fatalf("grounding = %v, want %s", g, tc.want)
			}
		})
	}
}

// Day-snapping is bounded by the window passed in (the targeting window),
// not the full campaign window — so a targeted run can't move an idea onto an
// enabled day outside the requested window.
func TestPersistIdea_SnapsWithinWindow(t *testing.T) {
	campStart := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	campEnd := time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC)
	date := time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC)
	tok := map[time.Weekday]string{
		time.Sunday: "sun", time.Monday: "mon", time.Tuesday: "tue", time.Wednesday: "wed",
		time.Thursday: "thu", time.Friday: "fri", time.Saturday: "sat",
	}
	// Only the day after the model's date is enabled.
	campaign := &models.Campaign{
		ID: "camp1", CreatedBy: "user1", PublishingTime: "09:00",
		PublishingDays: models.StringSlice{tok[(date.Weekday()+1)%7]},
		StartDate:      &campStart, EndDate: &campEnd,
	}

	ideas := &stubIdeaRepo{}
	dp := DraftPost{Title: "T", PublishDate: "2026-08-12"}
	if _, err := persistIdea(t.Context(), &dp, campaign, &campStart, &campEnd, ideas, nil); err != nil {
		t.Fatal(err)
	}
	if got := ideas.created.Plan.TargetDate.String(); got != "2026-08-13" || dp.PublishDate != "2026-08-13" {
		t.Fatalf("campaign window: date = %s / %s, want snapped to 2026-08-13", got, dp.PublishDate)
	}

	// A single-day targeting window on the disabled day keeps the date.
	dp = DraftPost{Title: "T", PublishDate: "2026-08-12"}
	win := date
	if _, err := persistIdea(t.Context(), &dp, campaign, &win, &win, ideas, nil); err != nil {
		t.Fatal(err)
	}
	if got := ideas.created.Plan.TargetDate.String(); got != "2026-08-12" {
		t.Fatalf("targeting window: date = %s, want kept at 2026-08-12", got)
	}
}
