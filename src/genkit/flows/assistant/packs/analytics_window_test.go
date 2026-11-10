package packs

import (
	"context"
	"testing"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/domain/viewcontext"
	"github.com/ogen-app/ogen/src/genkit/flows/assistant"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
	"github.com/ogen-app/ogen/src/usecase/analytics"
)

// Setting the window builds on the screen's, checks platforms and the
// campaign, and the turn's later reads use it.
func TestSetAnalyticsWindow(t *testing.T) {
	d := Deps{Analytics: &analytics.Service{Platforms: slugPlatforms{}, Campaigns: &memCampaigns{c: &models.Campaign{ID: "c1", Name: "Spring"}}}}
	ctx := analyticsTurn(t, &viewcontext.Window{From: "2026-10-01", To: "2026-10-14", Platforms: []string{"instagram"}})

	out, err := setAnalyticsWindow(ctx, d, SetWindowInput{Platforms: []string{"LinkedIn"}, CampaignID: "c1"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Applied.From != "2026-10-01" || out.Applied.Platforms[0] != "linkedin" || out.Applied.Campaign.Name != "Spring" {
		t.Fatalf("out = %+v", out)
	}
	if w := assistant.Window(ctx); w.CampaignID != "c1" || w.Platforms[0] != "linkedin" || w.To != "2026-10-14" {
		t.Fatalf("the turn reads %+v", w)
	}
	rng, sc, err := rangeAndScope(ctx, d, AnalyticsQuery{}, "")
	if err != nil || rng.Days != 14 || !sc.AllowsSlug("linkedin") || sc.Campaign() != "c1" {
		t.Fatalf("a later read = %+v %+v %v", rng, sc, err)
	}

	if out, err = setAnalyticsWindow(ctx, d, SetWindowInput{From: "2026-03-01", To: "2026-03-31", Platforms: []string{"all"}, CampaignID: "all"}); err != nil ||
		out.Applied.Campaign != nil || len(out.Applied.Platforms) != 0 || out.Applied.To != "2026-03-31" {
		t.Fatalf("widened = %+v, %v", out, err)
	}
	_, err = setAnalyticsWindow(ctx, d, SetWindowInput{Platforms: []string{"myspace"}})
	refused(t, err)

	home := tenantctx.With(context.WithValue(t.Context(), logging.UserIDKey, "u1"), "tenant-1")
	home, _ = assistant.NewTurnContext(home, workspace, nil)
	_, err = setAnalyticsWindow(home, d, SetWindowInput{Window: "7d"})
	refused(t, err)
}
