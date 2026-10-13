package repository_test

import (
	"slices"
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
)

// setIdeaCreated backdates ideas so the oldest one is known.
func (fx *assetQueryFixture) setIdeaCreated(t *testing.T, ages map[string]time.Duration) {
	t.Helper()
	for id, age := range ages {
		if _, err := fx.db.NewUpdate().TableExpr("ideas").Set("created_at = ?", fx.now.Add(-age)).
			Where("id = ?", id).Exec(fx.ctx); err != nil {
			t.Fatal(err)
		}
	}
}

// addIdea files one more analysis idea against a lens.
func (fx *assetQueryFixture) addIdea(t *testing.T, id, lens string, verdict models.IdeaVerdict) {
	t.Helper()
	i := &models.Idea{ID: id, Title: id, Verdict: &verdict, DecidedAt: &fx.now,
		Origin: models.IdeaOriginAssetAnalysis, AssetAnalysisID: &lens, CreatedByName: "u"}
	if _, err := fx.db.NewInsert().Model(i).Exec(fx.ctx); err != nil {
		t.Fatal(err)
	}
}

func TestAssetQuery_NextIdea(t *testing.T) {
	fx := seedAssetQueryFixture(t)
	// The later idea is oldest but still snoozed; the woken one is the oldest
	// undecided, older than the idea without a verdict.
	fx.setIdeaCreated(t, map[string]time.Duration{
		"i-later": 4 * time.Hour, "i-yes": 3 * time.Hour, "i-woken": 2 * time.Hour, "i-undecided": time.Hour,
	})
	fx.addIdea(t, "i-web-yes", "l-web", models.IdeaVerdictYes)

	res := fx.run(t, models.AssetQuery{})
	for _, a := range res.Assets {
		if (a.NextIdea == nil) != (a.IdeaCounts.Undecided == 0) {
			t.Errorf("%s: next_idea %+v disagrees with %d undecided", a.ID, a.NextIdea, a.IdeaCounts.Undecided)
		}
		switch a.ID {
		case fxPDF:
			if a.NextIdea == nil || *a.NextIdea != (models.AssetNextIdea{ID: "i-woken", Title: "i-woken"}) {
				t.Errorf("pdf next_idea = %+v, want the woken idea", a.NextIdea)
			}
		case fxWeb:
			if a.IdeaCounts.Yes != 1 || a.NextIdea != nil {
				t.Errorf("web row = %+v / %+v, want ideas but none undecided", a.IdeaCounts, a.NextIdea)
			}
		}
	}

	// Once the woken idea is decided, the next one is the idea without a verdict.
	if _, err := fx.db.NewUpdate().TableExpr("ideas").Set("verdict = 'no', remind_at = NULL").Where("id = 'i-woken'").Exec(fx.ctx); err != nil {
		t.Fatal(err)
	}
	pdf := fx.run(t, models.AssetQuery{Kind: anyOf("pdf")}).Assets[0]
	if pdf.NextIdea == nil || pdf.NextIdea.ID != "i-undecided" || pdf.IdeaCounts.Undecided != 1 {
		t.Errorf("pdf = %+v / %+v, want i-undecided next", pdf.NextIdea, pdf.IdeaCounts)
	}
}

func TestAssetQuery_IdeasUndecidedTotal(t *testing.T) {
	fx := seedAssetQueryFixture(t)
	// A second asset with an undecided idea, so the sum spans assets.
	fx.addIdea(t, "i-web-yes", "l-web", models.IdeaVerdictYes)
	if _, err := fx.db.NewInsert().Model(&models.Idea{ID: "i-web-open", Title: "open",
		Origin: models.IdeaOriginAssetAnalysis, AssetAnalysisID: new("l-web"), CreatedByName: "u"}).Exec(fx.ctx); err != nil {
		t.Fatal(err)
	}
	want := 3 // the pdf's undecided and woken ideas, and the web page's open one
	q := models.AssetQuery{Limit: 1}
	for range 20 {
		res := fx.run(t, q)
		if res.IdeasUndecided != want {
			t.Errorf("page after %q: ideas_undecided = %d, want %d on every page", q.Cursor, res.IdeasUndecided, want)
		}
		if res.NextCursor == "" {
			break
		}
		q.Cursor = res.NextCursor
	}
	if got := fx.run(t, models.AssetQuery{Kind: anyOf("web")}).IdeasUndecided; got != 1 {
		t.Errorf("filtered ideas_undecided = %d, want the web page's 1", got)
	}
}

// Created and updated are separate clocks: an edit moves updated only.
func TestAssetQuery_CreatedIgnoresEdits(t *testing.T) {
	fx := seedAssetQueryFixture(t)
	if _, err := fx.db.NewUpdate().TableExpr("assets").Set("updated_at = ?", fx.now).Where("id = ?", fxAudio).Exec(fx.ctx); err != nil {
		t.Fatal(err)
	}
	since := fx.now.Add(-5 * 24 * time.Hour)
	if got := listedIDs(fx.run(t, models.AssetQuery{CreatedAfter: &since})); !slices.Equal(got, []string{fxPDF, fxWeb}) {
		t.Errorf("created window = %v, want the edited recording left out", got)
	}
	if got := listedIDs(fx.run(t, models.AssetQuery{UpdatedAfter: &since})); !slices.Contains(got, fxAudio) {
		t.Errorf("updated window = %v, want the edited recording in", got)
	}
}

// groupedRow is a listed row with the group it is under.
type groupedRow struct{ id, key string }

func groupedRows(res *models.AssetQueryResult) []groupedRow {
	out := make([]groupedRow, len(res.Assets))
	for i, a := range res.Assets {
		out[i] = groupedRow{a.ID, a.GroupKey}
	}
	return out
}

// checkGroups checks one whole grouped read: groups are contiguous, in the
// order and with the counts groups gives.
func checkGroups(t *testing.T, res *models.AssetQueryResult) {
	t.Helper()
	var order []string
	counts := map[string]int{}
	for _, a := range res.Assets {
		if len(order) == 0 || order[len(order)-1] != a.GroupKey {
			if slices.Contains(order, a.GroupKey) {
				t.Errorf("group %s is split", a.GroupKey)
			}
			order = append(order, a.GroupKey)
		}
		counts[a.GroupKey]++
	}
	if len(order) != len(res.Groups) {
		t.Fatalf("rows list groups %v, groups = %+v", order, res.Groups)
	}
	for i, g := range res.Groups {
		if order[i] != g.Key || counts[g.Key] != g.Count {
			t.Errorf("group %d = %+v, rows have %s with %d", i, g, order[i], counts[order[i]])
		}
	}
}

func TestAssetQuery_GroupByIdeas(t *testing.T) {
	fx := seedAssetQueryFixture(t)
	fx.addIdea(t, "i-web-yes", "l-web", models.IdeaVerdictYes)
	res := fx.run(t, models.AssetQuery{Group: models.AssetGroupIdeas})
	checkGroups(t, res)
	want := []models.AssetGroupCount{{Key: "undecided", Count: 1}, {Key: "answered", Count: 1}, {Key: "none", Count: 7}}
	if !slices.Equal(res.Groups, want) {
		t.Errorf("groups = %+v, want %+v", res.Groups, want)
	}
	if got := groupedRows(res)[:2]; !slices.Equal(got, []groupedRow{{fxPDF, "undecided"}, {fxWeb, "answered"}}) {
		t.Errorf("first rows = %+v", got)
	}
	if res.Total != len(fxAllAssets) {
		t.Errorf("total = %d, want every asset once", res.Total)
	}
}

func TestAssetQuery_GroupByTag(t *testing.T) {
	fx := seedAssetQueryFixture(t)
	res := fx.run(t, models.AssetQuery{Group: models.AssetGroupTag})
	checkGroups(t, res)
	// By name: Finance before Gartner; the page has a row per tag, so the
	// web page shows twice, and total still counts assets.
	want := []models.AssetGroupCount{{Key: "tg-finance", Count: 1}, {Key: "tg-gartner", Count: 2}, {Key: "none", Count: 7}}
	if !slices.Equal(res.Groups, want) {
		t.Errorf("groups = %+v, want %+v", res.Groups, want)
	}
	if len(res.Assets) != 10 || res.Total != len(fxAllAssets) {
		t.Errorf("%d rows, total %d; want 10 rows of 9 assets", len(res.Assets), res.Total)
	}
	if got := groupedRows(res)[:3]; !slices.Equal(got, []groupedRow{{fxWeb, "tg-finance"}, {fxPDF, "tg-gartner"}, {fxWeb, "tg-gartner"}}) {
		t.Errorf("first rows = %+v", got)
	}

	// A tag filter keeps an asset under the tags it asks for only.
	filtered := fx.run(t, models.AssetQuery{Group: models.AssetGroupTag, Tag: anyOf("tg-finance")})
	if got := groupedRows(filtered); !slices.Equal(got, []groupedRow{{fxWeb, "tg-finance"}}) {
		t.Errorf("tag=tg-finance rows = %+v, want the web page under finance only", got)
	}
}

func TestAssetQuery_GroupByCampaignAndKind(t *testing.T) {
	fx := seedAssetQueryFixture(t)
	res := fx.run(t, models.AssetQuery{Group: models.AssetGroupCampaign})
	checkGroups(t, res)
	want := []models.AssetGroupCount{{Key: "c-explicit", Count: 2}, {Key: "c-off", Count: 1}, {Key: "none", Count: 6}}
	if !slices.Equal(res.Groups, want) {
		t.Errorf("campaign groups = %+v, want %+v", res.Groups, want)
	}
	kinds := fx.run(t, models.AssetQuery{Group: models.AssetGroupKind})
	checkGroups(t, kinds)
	if kinds.Groups[0].Key != "pdf" || kinds.Groups[len(kinds.Groups)-1].Key != "presentation" {
		t.Errorf("kind groups = %+v, want the kind order", kinds.Groups)
	}
}

func TestAssetQuery_GroupByCreated(t *testing.T) {
	fx := seedAssetQueryFixture(t)
	loc, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatal(err)
	}
	res := fx.run(t, models.AssetQuery{Group: models.AssetGroupCreated, Location: loc})
	checkGroups(t, res)
	band := func(created time.Time) string {
		for _, key := range models.AssetCreatedGroupKeys[:3] {
			if since, _ := models.AssetDateSince(key, fx.now, loc); !created.Before(since) {
				return key
			}
		}
		return models.AssetCreatedEarlier
	}
	for _, a := range res.Assets {
		if want := band(a.CreatedAt); a.GroupKey != want {
			t.Errorf("%s created %v is under %s, want %s", a.ID, a.CreatedAt, a.GroupKey, want)
		}
	}
}

// Paging a grouped read, a page boundary inside a group or between two, gives
// the rows of one whole read in the same order.
func TestAssetQuery_GroupedPagination(t *testing.T) {
	fx := seedAssetQueryFixture(t)
	fx.addIdea(t, "i-web-yes", "l-web", models.IdeaVerdictYes)
	sorts := []string{models.AssetSortUpdatedDesc, models.AssetSortTitleAsc, models.AssetSortCreatedAsc, models.AssetSortIdeasUndecided}
	for _, group := range models.AssetGroups {
		for _, sort := range sorts {
			t.Run(group+"/"+sort, func(t *testing.T) {
				base := models.AssetQuery{Group: group, Sort: sort}
				whole := fx.run(t, base)
				var got []groupedRow
				q := base
				q.Limit = 2
				for range 20 {
					res := fx.run(t, q)
					got = append(got, groupedRows(res)...)
					if !slices.Equal(res.Groups, whole.Groups) {
						t.Errorf("groups on a page = %+v, want the whole match's %+v", res.Groups, whole.Groups)
					}
					if res.NextCursor == "" {
						break
					}
					q.Cursor = res.NextCursor
				}
				if want := groupedRows(whole); !slices.Equal(got, want) {
					t.Errorf("paged %+v\nwant  %+v", got, want)
				}
			})
		}
	}

	first := fx.run(t, models.AssetQuery{Group: models.AssetGroupTag, Limit: 2})
	for _, other := range []models.AssetQuery{{Group: models.AssetGroupKind}, {}} {
		other.Cursor = first.NextCursor
		if _, err := fx.repo.Query(fx.ctx, fx.query(other)); err == nil {
			t.Errorf("a tag-grouped cursor is accepted by group %q", other.Group)
		}
	}
}
