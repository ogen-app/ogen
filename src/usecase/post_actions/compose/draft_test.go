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
	s := New(nil, memIdeas{byID: byID}, oneCampaign{byID: map[string]*models.Campaign{"c1": spring()}}, links, nil, nil)
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
	plan, err := s.PlanDraft(t.Context(), DraftInput{IdeaIDs: []string{"a", "b"}}, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Jobs) != 2 || !slices.Equal(plan.Jobs[1].IdeaIDs, []string{"b"}) {
		t.Fatalf("jobs = %+v", plan.Jobs)
	}
	r := plan.Jobs[0].Request
	if r.PlatformID != "linkedin" || r.PostType != "carousel" || r.WindowStart != "2026-11-20" || r.Count != 1 ||
		!slices.Equal(r.UsedAssetIDs, []string{"asset-1"}) {
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
		return []Written{{PostID: "p1", Title: "T", PlatformID: req.PlatformID}}, []string{"one draft dropped"}, nil
	})
	plan, err := s.PlanDraft(t.Context(), DraftInput{IdeaIDs: []string{"a", "b"}}, 5)
	if err != nil {
		t.Fatal(err)
	}
	var streamed []string
	res, err := s.RunDraft(t.Context(), plan, func(p DraftedPost) { streamed = append(streamed, p.PostID) })
	if err == nil || len(res.Posts) != 1 || !slices.Equal(streamed, []string{"p1"}) || len(res.Warnings) != 1 {
		t.Fatalf("res = %+v, err = %v", res, err)
	}
	if !slices.Equal(links.links["p1"], []string{"a"}) {
		t.Fatalf("links = %v", links.links)
	}
}
