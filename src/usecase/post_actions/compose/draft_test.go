package compose

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
)

type memIdeas struct {
	repository.IdeaRepository
	byID map[string]models.Idea
}

func (r memIdeas) GetByID(_ context.Context, id string) (*models.Idea, error) {
	if i, ok := r.byID[id]; ok {
		return &i, nil
	}
	return nil, sql.ErrNoRows
}

type memLinks struct {
	repository.PostIdeaRepository
	links map[string][]string
}

func (r *memLinks) Link(_ context.Context, _ bun.IDB, postID string, ideaIDs []string) error {
	r.links[postID] = ideaIDs
	return nil
}

func draftService(ideas ...models.Idea) (*Service, *memLinks) {
	byID := map[string]models.Idea{}
	for _, i := range ideas {
		byID[i.ID] = i
	}
	links := &memLinks{links: map[string][]string{}}
	c := spring()
	c.CampaignType = &models.CampaignType{Phases: []models.CampaignTypePhase{{ID: "ph1"}}}
	s := New(nil, memIdeas{byID: byID}, oneCampaign{byID: map[string]*models.Campaign{"c1": c}}, links, nil, nil)
	return s, links
}

func TestPlanDraft(t *testing.T) {
	day, _ := models.ParseCalendarDate("2026-11-20")
	angle, why := "Most buyers overpay", "It's counter-intuitive"
	a := idea("a", "c1", models.IdeaPlan{PlatformID: new("linkedin"), PostType: new("carousel"), TargetDate: &day})
	a.Angle, a.Rationale, a.Sources = &angle, &why, []string{"p. 3"}
	asset := "asset-1"
	a.SourceAssetID = &asset
	b := idea("b", "c1", models.IdeaPlan{})
	b.Note = "A manual note"
	s, _ := draftService(a, b)

	// One post per idea on the suggested platform, at the suggested date.
	plan, err := s.PlanDraft(t.Context(), DraftInput{IdeaIDs: []string{"a", "b"}, PhaseID: "ph1", Actor: "user-2"}, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Jobs) != 2 || !slices.Equal(plan.Jobs[1].IdeaIDs, []string{"b"}) {
		t.Fatalf("jobs = %+v", plan.Jobs)
	}
	// The first idea's plan dates both posts; the second idea has no date of
	// its own, but the draft is dated by the first plan that sets one.
	if plan.Jobs[0].DateFrom != DateFromPlan || plan.Jobs[1].DateFrom != DateFromPlan {
		t.Fatalf("dates from %q / %q; want the plan", plan.Jobs[0].DateFrom, plan.Jobs[1].DateFrom)
	}
	r := plan.Jobs[0].Request
	if r.PlatformID != "linkedin" || r.PostType != "carousel" || r.WindowStart != "2026-11-20" || r.Count != 1 ||
		r.PhaseID != "ph1" || r.CreatedBy != "user-2" || !slices.Equal(r.UsedAssetIDs, []string{"asset-1"}) {
		t.Fatalf("request = %+v", r)
	}
	want := "Idea: Idea a\nAngle: Most buyers overpay\nWhy it works: It's counter-intuitive\nFrom the source: p. 3"
	if r.SourceMaterial != want || plan.Jobs[1].Request.SourceMaterial != "Idea: Idea b\nA manual note" {
		t.Fatalf("source material = %q / %q", r.SourceMaterial, plan.Jobs[1].Request.SourceMaterial)
	}

	// Together: one post per named platform from both ideas; naming
	// platforms drops the suggested post type.
	plan, err = s.PlanDraft(t.Context(), DraftInput{IdeaIDs: []string{"a", "b"}, PlatformIDs: []string{"linkedin", "threads"}, Together: true}, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Jobs) != 2 || plan.Jobs[1].Request.PlatformID != "threads" || plan.Jobs[0].Request.PostType != "" ||
		!slices.Equal(plan.Jobs[0].IdeaIDs, []string{"a", "b"}) || !strings.Contains(plan.Jobs[0].Request.SourceMaterial, "Idea: Idea b") {
		t.Fatalf("together = %+v", plan.Jobs)
	}

	// A day named wins over the plan's; no date at all says nothing.
	asked, _ := models.ParseCalendarDate("2026-12-05")
	if plan, err := s.PlanDraft(t.Context(), DraftInput{IdeaIDs: []string{"a"}, TargetDate: &asked}, 5); err != nil ||
		plan.Jobs[0].DateFrom != DateFromAsked || plan.Jobs[0].Request.WindowStart != "2026-12-05" {
		t.Fatalf("asked = %+v, %v", plan, err)
	}
	if plan, err := s.PlanDraft(t.Context(), DraftInput{IdeaIDs: []string{"b"}, PlatformIDs: []string{"linkedin"}}, 5); err != nil || plan.Jobs[0].DateFrom != "" {
		t.Fatalf("undated = %+v, %v", plan, err)
	}

	for name, tc := range map[string]struct {
		in   DraftInput
		max  int
		want error
	}{
		"no suggested platform": {DraftInput{IdeaIDs: []string{"b"}}, 5, ErrNoPlatform},
		"not a target":          {DraftInput{IdeaIDs: []string{"a"}, PlatformIDs: []string{"myspace"}}, 5, ErrNotTarget},
		"type for two":          {DraftInput{IdeaIDs: []string{"a"}, PlatformIDs: []string{"linkedin", "threads"}, PostType: "carousel"}, 5, ErrPostTypeOneFor},
		"over the cap":          {DraftInput{IdeaIDs: []string{"a", "b"}, PlatformIDs: []string{"linkedin", "threads"}}, 3, ErrTooManyDrafts},
		"unknown idea":          {DraftInput{IdeaIDs: []string{"zz"}}, 5, ErrIdeaNotFound},
		"not a phase":           {DraftInput{IdeaIDs: []string{"a"}, PhaseID: "elsewhere"}, 5, ErrInvalidPhase},
		"targets and one more":  {DraftInput{IdeaIDs: []string{"a"}, PlatformIDs: []string{"linkedin", "threads", "linkedin", "myspace"}}, 5, ErrNotTarget},
	} {
		if _, err := s.PlanDraft(t.Context(), tc.in, tc.max); !errors.Is(err, tc.want) || !IsValidation(err) {
			t.Errorf("%s: err = %v, want %v", name, err, tc.want)
		}
	}
}

func TestRunDraft_LinksEachPostAndKeepsWhatWasWritten(t *testing.T) {
	s, links := draftService(idea("a", "c1", models.IdeaPlan{PlatformID: new("linkedin")}), idea("b", "c1", models.IdeaPlan{}))
	calls := 0
	s.WithDrafter(func(_ context.Context, req DraftRequest) ([]Written, []string, error) {
		calls++
		if calls == 2 {
			return nil, nil, errors.New("writer down")
		}
		return []Written{{PostID: "p1", Title: "T", PlatformID: req.PlatformID, PlatformPostType: "carousel", PublishDate: "2026-11-20"}}, []string{"one draft dropped"}, nil
	})
	plan, err := s.PlanDraft(t.Context(), DraftInput{IdeaIDs: []string{"a", "b"}}, 5)
	if err != nil {
		t.Fatal(err)
	}
	plan.Jobs[0].DateFrom = DateFromPlan
	var streamed []string
	res, err := s.RunDraft(t.Context(), plan, func(p DraftedPost) { streamed = append(streamed, p.PostID) })
	if err == nil || len(res.Posts) != 1 || !slices.Equal(streamed, []string{"p1"}) || len(res.Warnings) != 1 {
		t.Fatalf("res = %+v, err = %v", res, err)
	}
	if !slices.Equal(links.links["p1"], []string{"a"}) {
		t.Fatalf("links = %v", links.links)
	}
	// A dated post says where its date came from, and keeps its type.
	if p := res.Posts[0]; p.DateFrom != DateFromPlan || p.PlatformPostType != "carousel" {
		t.Fatalf("post = %+v", p)
	}
}

// A draft with no window stays undated; a lone bound is the single day.
func TestRunDraft_CompletesAOneSidedWindow(t *testing.T) {
	cases := map[string]struct{ start, end, wantStart, wantEnd string }{
		"none, undated": {"", "", "", ""},
		"start only":    {"2026-12-01", "", "2026-12-01", "2026-12-01"},
		"end only":      {"", "2026-11-10", "2026-11-10", "2026-11-10"},
		"both, as is":   {"2026-11-08", "2026-11-09", "2026-11-08", "2026-11-09"},
	}
	for name, tc := range cases {
		got := withWindow(DraftRequest{WindowStart: tc.start, WindowEnd: tc.end})
		if got.WindowStart != tc.wantStart || got.WindowEnd != tc.wantEnd {
			t.Errorf("%s: window = %s..%s, want %s..%s", name, got.WindowStart, got.WindowEnd, tc.wantStart, tc.wantEnd)
		}
	}
}

// A post written outside a plan is dated by the request's window, so a
// dated one says it was asked for; an undated one says nothing.
func TestWrite_DateFrom(t *testing.T) {
	s, _ := draftService(idea("a", "c1", models.IdeaPlan{}))
	s.WithDrafter(func(_ context.Context, req DraftRequest) ([]Written, []string, error) {
		return []Written{{PostID: "p-" + req.WindowStart, PublishDate: req.WindowStart}}, nil, nil
	})
	res, err := s.Write(t.Context(), DraftRequest{CampaignID: "c1", WindowStart: "2026-12-01"}, []string{"a"}, nil)
	if err != nil || res.Posts[0].DateFrom != DateFromAsked {
		t.Fatalf("dated = %+v, %v", res, err)
	}
	res, err = s.Write(t.Context(), DraftRequest{CampaignID: "c1"}, []string{"a"}, nil)
	if err != nil || res.Posts[0].DateFrom != "" {
		t.Fatalf("undated = %+v, %v", res, err)
	}
}
