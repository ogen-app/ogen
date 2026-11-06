package ideas

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
)

func TestTokenize(t *testing.T) {
	got := tokenize("The 3 Pricing Myths, and why stories matter — for CFOs!")
	want := []string{"3", "cfo", "matter", "myth", "pricing", "story"}
	if !slices.Equal(got, want) {
		t.Fatalf("tokens = %v, want %v", got, want)
	}
	for in, want := range map[string]string{"bus": "bus", "class": "class", "analysis": "analysis", "ties": "tie", "posts": "post", "companies": "company"} {
		if got := singular(in); got != want {
			t.Errorf("singular(%q) = %q, want %q", in, got, want)
		}
	}
}

// The threshold is tuned on these pairs: rewordings of one idea match,
// neighbouring ideas on the same topic do not.
func TestSimilarity_Threshold(t *testing.T) {
	tok := func(title, note string) ideaTokens {
		tt := tokenize(title)
		return ideaTokens{title: tt, all: union(tt, tokenize(note))}
	}
	same := [][2]ideaTokens{
		{tok("5 pricing myths that cost you deals", ""), tok("Pricing myths that cost you deals", "")},
		{tok("Behind the scenes: how we build our roadmap", ""), tok("How we build the roadmap — behind the scenes", "")},
		{tok("Customer story: Acme cut churn by 30%", "Interview the Acme team about churn"), tok("Acme customer story: churn down 30%", "Interview Acme about the churn drop")},
		{tok("Founder story", "Why we started the company"), tok("The founder's story", "Why we started the company and what we learned")},
		{tok("Launch week recap", ""), tok("Recap of launch week", "")},
	}
	for _, p := range same {
		if s := similarity(p[0], p[1]); s < DuplicateThreshold {
			t.Errorf("%v ~ %v = %.2f, want a match", p[0].title, p[1].title, s)
		}
	}
	different := [][2]ideaTokens{
		{tok("Pricing myths", ""), tok("Pricing tips for startups", "")},
		{tok("Customer story: Acme", "churn"), tok("Customer story: Globex", "onboarding")},
		{tok("How we hire engineers", ""), tok("How we onboard engineers", "")},
		{tok("Q3 product update", "New dashboard and exports"), tok("Q4 product update", "Billing and SSO")},
		{tok("The", ""), tok("A", "")},
	}
	for _, p := range different {
		if s := similarity(p[0], p[1]); s >= DuplicateThreshold {
			t.Errorf("%v ~ %v = %.2f, want no match", p[0].title, p[1].title, s)
		}
	}
}

func TestGroupDuplicates(t *testing.T) {
	base := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	yes := models.IdeaVerdictYes
	idea := func(id, title string, age int) models.Idea {
		return models.Idea{ID: id, Title: title, CreatedAt: base.Add(time.Duration(age) * time.Hour)}
	}
	a1 := idea("a1", "Pricing myths that cost you deals", 0)
	a2 := idea("a2", "5 pricing myths that cost you deals", 1)
	a2.AssetIDs = []string{"x", "y"}
	a3 := idea("a3", "Pricing myths that cost deals", 2)
	b1 := idea("b1", "Launch week recap", 3)
	b2 := idea("b2", "Recap of launch week", 4)
	b2.Verdict = &yes
	lone := idea("lone", "Hiring our first designer", 5)

	groups := groupDuplicates([]models.Idea{lone, b2, a3, b1, a2, a1})
	if len(groups) != 2 {
		t.Fatalf("groups = %+v", groups)
	}
	ids := func(g DuplicateGroup) []string {
		var out []string
		for _, m := range g.Ideas {
			out = append(out, m.Idea.ID)
		}
		return out
	}
	// Largest first; most assets wins the keep, the rest oldest first.
	if g := groups[0]; g.KeepID != "a2" || !slices.Equal(ids(g), []string{"a2", "a1", "a3"}) || g.Ideas[0].AssetCount != 2 {
		t.Fatalf("pricing group = %+v", g)
	}
	// A yes wins the keep over an older idea.
	if g := groups[1]; g.KeepID != "b2" || !slices.Equal(ids(g), []string{"b2", "b1"}) || g.Similarity != 1 {
		t.Fatalf("launch group = %+v", g)
	}
	if s := groups[0].Similarity; s < DuplicateThreshold || s > 1 {
		t.Fatalf("similarity = %v", s)
	}
}

type openRepo struct {
	repository.IdeaRepository
	open  []models.Idea
	more  bool
	scope string
}

func (r *openRepo) OpenIdeas(_ context.Context, campaignID string, limit int) ([]models.Idea, bool, error) {
	r.scope = campaignID
	return r.open[:min(len(r.open), limit)], r.more, nil
}

func (r *openRepo) LiveCampaignExists(_ context.Context, id string) (bool, error) {
	return id == "c1", nil
}

func TestDuplicates_Scope(t *testing.T) {
	repo := &openRepo{open: []models.Idea{{ID: "1", Title: "Launch recap"}, {ID: "2", Title: "launch recaps"}}, more: true}
	svc := New(repo, nil)
	res, err := svc.Duplicates(t.Context(), nil)
	if err != nil || repo.scope != "" || len(res.Groups) != 1 || res.Scanned != 2 || !res.Truncated {
		t.Fatalf("workspace = %+v, %v", res, err)
	}
	if _, err := svc.Duplicates(t.Context(), new("c1")); err != nil || repo.scope != "c1" {
		t.Fatalf("campaign scope = %q, %v", repo.scope, err)
	}
	if _, err := svc.Duplicates(t.Context(), new("gone")); !errors.Is(err, ErrScopeNotFound) {
		t.Fatalf("gone campaign = %v", err)
	}
}
