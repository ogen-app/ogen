package repository_test

import (
	"net/url"
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
	"github.com/ogen-app/ogen/src/usecase/ideas"
)

// An idea's plan round-trips and can be cleared, and the new origins read
// back as themselves in the origin filter and facet, beside manual ideas.
func TestIdeas_PlanAndOrigins(t *testing.T) {
	db := openMigratedDB(t)
	repo := repository.NewIdeaRepository(db)
	ctx := tenantctx.With(t.Context(), "tenant-idea-plan")

	day, err := models.ParseCalendarDate("2026-12-01")
	if err != nil {
		t.Fatal(err)
	}
	planned := &models.Idea{
		ID: "ip-plan", Title: "Launch teaser", Origin: models.IdeaOriginContentPlan,
		Plan:     models.IdeaPlan{PlatformID: new("linkedin"), PostType: new("carousel"), PhaseID: new("ph1"), TargetDate: &day},
		AssetIDs: []string{"a1", "a2"}, GroundingMode: models.GroundingFor(2),
	}
	for _, i := range []*models.Idea{
		planned,
		{ID: "ip-assistant", Title: "Founder story", Origin: models.IdeaOriginAssistant},
		{ID: "ip-manual", Title: "Pricing myths", CreatedBy: new("u1"), CreatedByName: "Ann"},
	} {
		if err := repo.Create(ctx, i); err != nil {
			t.Fatalf("create %s: %v", i.ID, err)
		}
	}

	got, err := repo.GetByID(ctx, "ip-plan")
	if err != nil {
		t.Fatal(err)
	}
	if p := got.Plan; *p.PlatformID != "linkedin" || *p.PostType != "carousel" || *p.PhaseID != "ph1" || p.TargetDate.String() != "2026-12-01" {
		t.Fatalf("plan = %+v", p)
	}
	if len(got.AssetIDs) != 2 || got.AssetIDs[1] != "a2" || got.GroundingMode == nil || *got.GroundingMode != models.GroundingGrounded {
		t.Fatalf("sources = %v, grounding = %v", got.AssetIDs, got.GroundingMode)
	}
	manual, err := repo.GetByID(ctx, "ip-manual")
	if err != nil || manual.AssetIDs == nil || len(manual.AssetIDs) != 0 || manual.GroundingMode != nil {
		t.Fatalf("manual idea sources = %#v, grounding = %v (%v); want [] and nil", manual.AssetIDs, manual.GroundingMode, err)
	}

	got.Plan = models.IdeaPlan{}
	if ok, err := repo.Update(ctx, got, "plan_platform_id", "plan_post_type", "plan_phase_id", "plan_target_date"); err != nil || !ok {
		t.Fatalf("clear plan: %v, %v", ok, err)
	}
	if got, err = repo.GetByID(ctx, "ip-plan"); err != nil || !got.Plan.IsZero() {
		t.Fatalf("cleared plan = %+v (%v)", got.Plan, err)
	}

	q, err := ideas.ParseQuery(url.Values{"origin": {"content_plan,assistant"}, "facets": {"origin"}}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	res, err := repo.Query(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	if res.Total != 2 {
		t.Fatalf("content_plan + assistant = %d ideas, want 2", res.Total)
	}
	counts := map[string]int{}
	for _, c := range res.Facets[models.IdeaFacetOrigin] {
		counts[c.Value] = c.Count
	}
	if counts["content_plan"] != 1 || counts["assistant"] != 1 || counts["manual"] != 1 || counts["asset_analysis"] != 0 {
		t.Fatalf("origin facet = %v", counts)
	}
}
