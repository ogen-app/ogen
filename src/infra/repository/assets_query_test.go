package repository_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/pgvector/pgvector-go"
	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
)

// assetQueryFixture is a small workspace that touches every filter value.
type assetQueryFixture struct {
	db   *bun.DB
	repo repository.AssetRepository
	ctx  context.Context
	now  time.Time
}

const (
	fxTenant      = "tenant-asset-query"
	fxOtherTenant = "tenant-asset-query-other"
	fxChecksum    = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
)

// Assets of the fixture, by what makes each one distinct.
const (
	fxPDF   = "q-pdf"   // ready, tag gartner, in c-explicit, woken + later + yes ideas, matches text once
	fxWeb   = "q-web"   // processing, tags gartner+finance, in c-explicit, analysed without ideas, matches text twice
	fxNote  = "q-note"  // failed, only a deleted tag, in the deleted campaign only, analysis pending
	fxSheet = "q-sheet" // partial, untagged, listed by a campaign with use_assets off
	fxCSV   = "q-csv"
	fxSlide = "q-slide"
	fxDoc   = "q-doc"
	fxAudio = "q-audio"
	fxImage = "q-image"
)

var fxAllAssets = []string{fxPDF, fxWeb, fxNote, fxSheet, fxCSV, fxSlide, fxDoc, fxAudio, fxImage}

func seedAssetQueryFixture(t *testing.T) *assetQueryFixture {
	t.Helper()
	db := openMigratedDB(t)
	fx := &assetQueryFixture{
		db:   db,
		repo: repository.NewAssetRepository(db, repository.NewTagRepository(db), repository.NewAssetFileRepository(db)),
		ctx:  tenantctx.With(t.Context(), fxTenant),
		now:  time.Now().UTC().Truncate(time.Microsecond),
	}
	t.Cleanup(func() {
		ctx := context.Background()
		tenants := bun.List([]string{fxTenant, fxOtherTenant})
		for _, table := range []string{"ideas", "asset_analyses", "assets_chunks", "asset_files", "assets", "tags", "campaigns"} {
			_, _ = db.NewDelete().TableExpr(table).Where("tenant_id IN (?)", tenants).Exec(ctx)
		}
	})

	insert := func(ctx context.Context, model any) {
		t.Helper()
		if _, err := db.NewInsert().Model(model).Exec(ctx); err != nil {
			t.Fatalf("seed %T: %v", model, err)
		}
	}
	ctx := fx.ctx
	insert(ctx, &models.Tag{ID: "tg-gartner", Name: "Gartner"})
	insert(ctx, &models.Tag{ID: "tg-finance", Name: "Finance"})

	type assetSeed struct {
		id, title, status string
		typ               *string
		mime              string
		tags              []string
		age               time.Duration
	}
	seeds := []assetSeed{
		{fxPDF, "Gartner Hype Cycle 2026", models.AssetStatusReady, new(models.AssetTypePDF), "application/pdf", []string{"tg-gartner"}, time.Hour},
		{fxWeb, "Blog: 100% honest bees", models.AssetStatusProcessing, new(models.AssetTypeURL), "", []string{"tg-gartner", "tg-finance"}, 3 * 24 * time.Hour},
		{fxNote, "meeting notes", models.AssetStatusFailed, nil, "", []string{"tg-gone"}, 10 * 24 * time.Hour},
		{fxSheet, "Q3 budget", models.AssetStatusPartial, new(models.AssetTypeDocument), "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", nil, 40 * 24 * time.Hour},
		{fxCSV, "Leads export", models.AssetStatusReady, new(models.AssetTypeDocument), "text/csv", nil, 50 * 24 * time.Hour},
		{fxSlide, "Board deck", models.AssetStatusReady, new(models.AssetTypeDocument), "application/vnd.oasis.opendocument.presentation", nil, 60 * 24 * time.Hour},
		{fxDoc, "Press release", models.AssetStatusReady, new(models.AssetTypeDocument), "application/vnd.openxmlformats-officedocument.wordprocessingml.document", nil, 70 * 24 * time.Hour},
		{fxAudio, "Customer call", models.AssetStatusReady, new(models.AssetTypeAudio), "audio/mpeg", nil, 80 * 24 * time.Hour},
		{fxImage, "Hero shot", models.AssetStatusPending, new(models.AssetTypeImage), "image/png", nil, 90 * 24 * time.Hour},
	}
	for _, s := range seeds {
		at := fx.now.Add(-s.age)
		insert(ctx, &models.Asset{
			ID: s.id, Title: s.title, Status: s.status, Type: s.typ,
			TagIDs: models.StringSlice(s.tags), CreatedBy: "u", CreatedAt: at, UpdatedAt: at,
		})
		if s.mime != "" {
			f := &models.AssetFile{
				ID: "f-" + s.id, AssetID: s.id, OriginalName: s.id, MimeType: s.mime,
				S3Key: "k/" + s.id, CreatedAt: at, UpdatedAt: at,
			}
			if s.id == fxPDF {
				f.ChecksumSHA256, f.PageCount = fxChecksum, new(12)
			}
			insert(ctx, f)
		}
	}
	// Another tenant's asset, tag and campaign must never show up.
	other := tenantctx.With(t.Context(), fxOtherTenant)
	insert(other, &models.Asset{ID: "q-other", Title: "Gartner elsewhere", Status: models.AssetStatusReady,
		TagIDs: models.StringSlice{"tg-other"}, CreatedBy: "u", CreatedAt: fx.now, UpdatedAt: fx.now})
	insert(other, &models.Tag{ID: "tg-other", Name: "Other"})
	insert(other, &models.Campaign{ID: "c-other", Name: "Other", UseAssets: true, AssetIDs: models.StringSlice{},
		TargetPlatforms: models.CampaignPlatforms{}, PublishingDays: models.StringSlice{}})

	campaign := func(id string, use bool, assets []string, deleted bool) {
		c := &models.Campaign{ID: id, Name: id, UseAssets: use, AssetIDs: models.StringSlice(assets),
			TargetPlatforms: models.CampaignPlatforms{}, PublishingDays: models.StringSlice{}}
		if deleted {
			c.DeletedAt = &fx.now
		}
		insert(ctx, c)
	}
	campaign("c-explicit", true, []string{fxPDF, fxWeb}, false)
	campaign("c-off", false, []string{fxSheet}, false)
	campaign("c-deleted", true, []string{fxNote}, true)

	lens := func(id, asset string, campaignID *string, status models.AssetAnalysisStatus, summary string) {
		insert(ctx, &models.AssetAnalysis{ID: id, AssetID: asset, CampaignID: campaignID, Status: status, Summary: summary})
	}
	lens("l-pdf", fxPDF, nil, models.AssetAnalysisStatusReady, "## Summary\n\nThe hype cycle maps **AI agents** at the peak. More detail follows.")
	lens("l-pdf-c", fxPDF, new("c-explicit"), models.AssetAnalysisStatusReady, "")
	lens("l-web", fxWeb, nil, models.AssetAnalysisStatusReady, "Bees.")
	lens("l-note", fxNote, nil, models.AssetAnalysisStatusPending, "")
	lens("l-note-del", fxNote, new("c-deleted"), models.AssetAnalysisStatusReady, "")

	idea := func(id, analysis string, verdict *models.IdeaVerdict, remind *time.Time) {
		i := &models.Idea{ID: id, Title: id, Verdict: verdict, RemindAt: remind,
			Origin: models.IdeaOriginAssetAnalysis, AssetAnalysisID: &analysis, CreatedByName: "u"}
		if verdict != nil {
			i.DecidedAt = &fx.now
		}
		insert(ctx, i)
	}
	later := models.IdeaVerdictLater
	idea("i-undecided", "l-pdf", nil, nil)
	idea("i-woken", "l-pdf", &later, new(fx.now.Add(-time.Hour)))
	idea("i-later", "l-pdf", &later, new(fx.now.Add(24*time.Hour)))
	idea("i-yes", "l-pdf", new(models.IdeaVerdictYes), nil)
	idea("i-no-campaign-lens", "l-pdf-c", new(models.IdeaVerdictNo), nil)
	idea("i-deleted-campaign", "l-note-del", new(models.IdeaVerdictYes), nil)
	// A manual idea that happens to point at an analysis never counts.
	insert(ctx, &models.Idea{ID: "i-manual", Title: "manual", AssetAnalysisID: new("l-web"), CreatedByName: "u"})

	if err := repository.NewAssetFileRepository(db).SetDuration(ctx, fxAudio, 65000); err != nil {
		t.Fatalf("seed audio duration: %v", err)
	}

	chunks := repository.NewAssetChunksRepository(db)
	chunk := func(ctx context.Context, asset string, contents ...string) {
		cs := make([]models.AssetChunk, len(contents))
		for i, c := range contents {
			cs[i] = models.AssetChunk{ID: fmt.Sprintf("%s:%d", asset, i), AssetID: asset, ChunkIndex: i, Content: c,
				Model: "m", Embedding: pgvector.NewHalfVector(make([]float32, 3072))}
		}
		if err := chunks.UpsertChunks(ctx, asset, cs); err != nil {
			t.Fatalf("seed chunks %s: %v", asset, err)
		}
	}
	chunk(ctx, fxPDF, "Overwintering is on the slope of enlightenment.", "Nothing else here.")
	chunk(ctx, fxWeb, "Overwintering bees need dry hives.", "More on overwintering colonies.")
	chunk(ctx, fxAudio, "We talked about pricing.")
	chunk(other, "q-other", "Overwintering in another workspace.")
	return fx
}

func (fx *assetQueryFixture) query(q models.AssetQuery) models.AssetQuery {
	q.Now = fx.now
	if q.Location == nil {
		q.Location = time.UTC
	}
	if q.Sort == "" {
		q.Sort = models.AssetSortUpdatedDesc
	}
	if q.Limit == 0 {
		q.Limit = 200
	}
	return q
}

func (fx *assetQueryFixture) run(t *testing.T, q models.AssetQuery) *models.AssetQueryResult {
	t.Helper()
	res, err := fx.repo.Query(fx.ctx, fx.query(q))
	if err != nil {
		t.Fatalf("query %+v: %v", q, err)
	}
	return res
}

func listedIDs(res *models.AssetQueryResult) []string {
	out := make([]string, len(res.Assets))
	for i, a := range res.Assets {
		out[i] = a.ID
	}
	return out
}

func anyOf(vs ...string) models.AssetValueFilter  { return models.AssetValueFilter{Any: vs} }
func noneOf(vs ...string) models.AssetValueFilter { return models.AssetValueFilter{Not: vs} }

func TestAssetQuery_Filters(t *testing.T) {
	fx := seedAssetQueryFixture(t)
	since := fx.now.Add(-5 * 24 * time.Hour)
	cases := []struct {
		name string
		q    models.AssetQuery
		want []string
	}{
		{"everything, newest first", models.AssetQuery{}, fxAllAssets},
		{"title substring, case-insensitive", models.AssetQuery{Title: anyOf("GARTNER", "deck")}, []string{fxPDF, fxSlide}},
		{"title wildcard is literal", models.AssetQuery{Title: anyOf("100%")}, []string{fxWeb}},
		{"title_not", models.AssetQuery{Title: noneOf("BUDGET", "deck")}, []string{fxPDF, fxWeb, fxNote, fxCSV, fxDoc, fxAudio, fxImage}},
		{"kind", models.AssetQuery{Kind: anyOf("pdf", "note")}, []string{fxPDF, fxNote}},
		{"kind of documents by mime", models.AssetQuery{Kind: anyOf("spreadsheet", "csv", "presentation", "document")}, []string{fxSheet, fxCSV, fxSlide, fxDoc}},
		{"kind_not", models.AssetQuery{Kind: noneOf("document", "spreadsheet", "csv", "presentation", "audio", "image")}, []string{fxPDF, fxWeb, fxNote}},
		{"pending includes processing", models.AssetQuery{Status: anyOf("pending")}, []string{fxWeb, fxImage}},
		{"status_not", models.AssetQuery{Status: noneOf("ready", "pending")}, []string{fxNote, fxSheet}},
		{"explicit campaign", models.AssetQuery{Campaign: anyOf("c-explicit")}, []string{fxPDF, fxWeb}},
		{"campaign listed with use_assets off", models.AssetQuery{Campaign: anyOf("c-off")}, []string{fxSheet}},
		{"deleted campaign has no members", models.AssetQuery{Campaign: anyOf("c-deleted")}, nil},
		{"campaign none", models.AssetQuery{Campaign: anyOf("none")}, []string{fxNote, fxCSV, fxSlide, fxDoc, fxAudio, fxImage}},
		{"campaign_not", models.AssetQuery{Campaign: noneOf("c-explicit", "c-off")}, []string{fxNote, fxCSV, fxSlide, fxDoc, fxAudio, fxImage}},
		{"tag", models.AssetQuery{Tag: anyOf("tg-finance")}, []string{fxWeb}},
		{"tag none counts deleted tags as none", models.AssetQuery{Tag: anyOf("none")}, []string{fxNote, fxSheet, fxCSV, fxSlide, fxDoc, fxAudio, fxImage}},
		{"tag or none", models.AssetQuery{Tag: anyOf("tg-finance", "none"), Kind: anyOf("pdf", "web", "note")}, []string{fxWeb, fxNote}},
		{"tag_not", models.AssetQuery{Tag: noneOf("tg-gartner", "none")}, nil},
		{"ideas undecided includes woken", models.AssetQuery{Ideas: anyOf("undecided")}, []string{fxPDF}},
		{"ideas no from campaign lens", models.AssetQuery{Ideas: anyOf("no")}, []string{fxPDF}},
		{"ideas none", models.AssetQuery{Ideas: anyOf("none")}, []string{fxWeb}},
		{"ideas unread", models.AssetQuery{Ideas: anyOf("unread"), Kind: anyOf("pdf", "web", "note")}, []string{fxNote}},
		{"ideas_not", models.AssetQuery{Ideas: noneOf("unread")}, []string{fxPDF, fxWeb}},
		{"updated window", models.AssetQuery{UpdatedAfter: &since}, []string{fxPDF, fxWeb}},
		{"updated before", models.AssetQuery{UpdatedBefore: new(fx.now.Add(-75 * 24 * time.Hour))}, []string{fxAudio, fxImage}},
		{"created window", models.AssetQuery{CreatedAfter: &since}, []string{fxPDF, fxWeb}},
		{"created before", models.AssetQuery{CreatedBefore: new(fx.now.Add(-75 * 24 * time.Hour))}, []string{fxAudio, fxImage}},
		{"created, oldest first", models.AssetQuery{Sort: models.AssetSortCreatedAsc, Kind: anyOf("pdf", "web", "note")}, []string{fxNote, fxWeb, fxPDF}},
		{"checksum", models.AssetQuery{Checksums: []string{fxChecksum}}, []string{fxPDF}},
		{"text", models.AssetQuery{Text: "overwinter"}, []string{fxPDF, fxWeb}},
		{"text of stopwords only", models.AssetQuery{Text: "the and"}, nil},
		{"combined", models.AssetQuery{Tag: anyOf("tg-gartner"), Status: noneOf("pending")}, []string{fxPDF}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := listedIDs(fx.run(t, tc.q))
			if !slices.Equal(got, tc.want) {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestAssetQuery_WholeBankCampaign(t *testing.T) {
	fx := seedAssetQueryFixture(t)
	insert := func(c *models.Campaign) {
		if _, err := fx.db.NewInsert().Model(c).Exec(fx.ctx); err != nil {
			t.Fatal(err)
		}
	}
	if got := listedIDs(fx.run(t, models.AssetQuery{Campaign: anyOf("none")})); len(got) != 6 {
		t.Fatalf("without a whole-bank campaign, campaign=none = %v, want the 6 assets in none", got)
	}
	insert(&models.Campaign{ID: "c-whole", Name: "whole", UseAssets: true, AssetIDs: models.StringSlice{},
		TargetPlatforms: models.CampaignPlatforms{}, PublishingDays: models.StringSlice{}})
	if got := listedIDs(fx.run(t, models.AssetQuery{Campaign: anyOf("c-whole")})); !slices.Equal(got, fxAllAssets) {
		t.Errorf("campaign=c-whole = %v, want every asset", got)
	}
	if got := listedIDs(fx.run(t, models.AssetQuery{Campaign: anyOf("none")})); len(got) != 0 {
		t.Errorf("campaign=none = %v, want none: every asset is in the whole-bank campaign", got)
	}
	res := fx.run(t, models.AssetQuery{Title: anyOf("Gartner")})
	if got := res.Assets[0].CampaignIDs; !slices.Equal(got, []string{"c-whole", "c-explicit"}) {
		t.Errorf("campaign_ids = %v, want the whole-bank campaign and the explicit one", got)
	}
	scoped := fx.run(t, models.AssetQuery{ScopeCampaign: "c-whole"})
	if scoped.TotalUnfiltered != len(fxAllAssets) {
		t.Errorf("scope of a whole-bank campaign holds %d assets, want %d", scoped.TotalUnfiltered, len(fxAllAssets))
	}
}

// An archived campaign drops out of membership and the campaign facet, like
// the campaign list, but its own scope still reads.
func TestAssetQuery_ArchivedCampaign(t *testing.T) {
	fx := seedAssetQueryFixture(t)
	if _, err := fx.db.NewInsert().Model(&models.Campaign{
		ID: "c-arch", Name: "archived", UseAssets: true, AssetIDs: models.StringSlice{fxCSV}, ArchivedAt: &fx.now,
		TargetPlatforms: models.CampaignPlatforms{}, PublishingDays: models.StringSlice{},
	}).Exec(fx.ctx); err != nil {
		t.Fatal(err)
	}
	if got := listedIDs(fx.run(t, models.AssetQuery{Campaign: anyOf("c-arch")})); len(got) != 0 {
		t.Errorf("campaign=c-arch = %v, want none: archived campaigns have no members", got)
	}
	res := fx.run(t, models.AssetQuery{Kind: anyOf("csv"), Campaign: anyOf("none"), Facets: []string{models.AssetFacetCampaign}})
	if got := listedIDs(res); !slices.Equal(got, []string{fxCSV}) {
		t.Errorf("campaign=none = %v, want the csv only listed by an archived campaign", got)
	}
	for _, c := range res.Facets[models.AssetFacetCampaign] {
		if c.Value == "c-arch" {
			t.Error("the campaign facet lists an archived campaign")
		}
	}
	scoped := fx.run(t, models.AssetQuery{ScopeCampaign: "c-arch"})
	if got := listedIDs(scoped); !slices.Equal(got, []string{fxCSV}) || !slices.Equal(scoped.Assets[0].CampaignIDs, []string{"c-arch"}) {
		t.Errorf("scope c-arch = %v / %v, want the csv in its archived campaign", got, scoped.Assets)
	}
}

func TestAssetQuery_RowShape(t *testing.T) {
	fx := seedAssetQueryFixture(t)
	res := fx.run(t, models.AssetQuery{})
	byID := map[string]models.AssetListItem{}
	for _, a := range res.Assets {
		byID[a.ID] = a
	}
	pdf := byID[fxPDF]
	if pdf.Kind != models.AssetKindPDF || pdf.File == nil || *pdf.File.PageCount != 12 || pdf.File.ChecksumSHA256 != fxChecksum {
		t.Errorf("pdf row = %+v / %+v", pdf, pdf.File)
	}
	if want := (models.AssetIdeaCounts{Undecided: 2, Yes: 1, Later: 1, No: 1}); pdf.IdeaCounts != want {
		t.Errorf("idea_counts = %+v, want %+v (woken idea undecided only, campaign-lens idea counted)", pdf.IdeaCounts, want)
	}
	if pdf.Gist == nil || *pdf.Gist != "The hype cycle maps AI agents at the peak." {
		t.Errorf("gist = %v, want the summary's lead sentence", pdf.Gist)
	}
	if pdf.AnalysisStatus == nil || *pdf.AnalysisStatus != models.AssetAnalysisStatusReady {
		t.Errorf("analysis_status = %v", pdf.AnalysisStatus)
	}
	if len(pdf.Tags) != 1 || pdf.Tags[0].Name != "Gartner" {
		t.Errorf("tags = %+v", pdf.Tags)
	}
	if note := byID[fxNote]; len(note.Tags) != 0 || len(note.CampaignIDs) != 0 || note.Gist != nil || note.Kind != models.AssetKindNote {
		t.Errorf("note row = %+v: a deleted tag and a deleted campaign must not show", note)
	}
	if web := byID[fxWeb]; web.Status != models.AssetStatusProcessing || web.IdeaCounts != (models.AssetIdeaCounts{}) {
		t.Errorf("web row = %+v: stored status, manual ideas not counted", web)
	}
	if audio := byID[fxAudio]; audio.File == nil || audio.File.DurationMS != 65000 {
		t.Errorf("audio duration = %+v, want the file's 65000", audio.File)
	}
	for _, a := range res.Assets {
		if a.Match != nil {
			t.Errorf("%s has a match without text", a.ID)
		}
	}
	if res.Total != len(fxAllAssets) || res.TotalUnfiltered != len(fxAllAssets) {
		t.Errorf("totals %d/%d, want %d (other tenant excluded)", res.Total, res.TotalUnfiltered, len(fxAllAssets))
	}
}

func TestAssetQuery_TextMatch(t *testing.T) {
	fx := seedAssetQueryFixture(t)
	res := fx.run(t, models.AssetQuery{Text: "overwintering", Sort: models.AssetSortRelevance})
	if got := listedIDs(res); !slices.Equal(got, []string{fxWeb, fxPDF}) {
		t.Fatalf("relevance order = %v, want the asset with more matching chunks first", got)
	}
	web := res.Assets[0].Match
	if web == nil || web.Matches != 2 || !strings.Contains(web.Snippet, "<mark>Overwintering</mark>") {
		t.Errorf("web match = %+v", web)
	}
	if res.Total != 2 || res.TotalUnfiltered != len(fxAllAssets) {
		t.Errorf("totals %d/%d", res.Total, res.TotalUnfiltered)
	}
}

func TestAssetQuery_Scope(t *testing.T) {
	fx := seedAssetQueryFixture(t)
	res := fx.run(t, models.AssetQuery{ScopeCampaign: "c-explicit", Facets: models.AssetFacets, Kind: anyOf("pdf")})
	if res.Total != 1 || res.TotalUnfiltered != 2 {
		t.Errorf("scoped totals %d/%d, want 1/2", res.Total, res.TotalUnfiltered)
	}
	if _, ok := res.Facets[models.AssetFacetCampaign]; ok {
		t.Error("the campaign facet is returned inside a campaign scope")
	}
	for _, scope := range []string{"c-deleted", "c-other", "missing"} {
		if _, err := fx.repo.Query(fx.ctx, fx.query(models.AssetQuery{ScopeCampaign: scope})); !errors.Is(err, sql.ErrNoRows) {
			t.Errorf("scope %s: err = %v, want sql.ErrNoRows", scope, err)
		}
	}
	// Inside c-explicit the deleted campaign's lens never counted anyway; the
	// PDF keeps both its workspace and its c-explicit ideas.
	pdf := fx.run(t, models.AssetQuery{ScopeCampaign: "c-explicit", Kind: anyOf("pdf")}).Assets[0]
	if pdf.IdeaCounts.No != 1 || pdf.IdeaCounts.Undecided != 2 {
		t.Errorf("scoped idea counts = %+v", pdf.IdeaCounts)
	}
}

// TestAssetQuery_FacetsAgreeWithFilters is the facet property: for every base
// query and every value of every facet, the count equals the total of the base
// query with that field's filter replaced by the value alone.
func TestAssetQuery_FacetsAgreeWithFilters(t *testing.T) {
	fx := seedAssetQueryFixture(t)
	bases := []models.AssetQuery{
		{},
		{Tag: anyOf("tg-gartner")},
		{Kind: noneOf("pdf"), Status: anyOf("ready", "pending")},
		{Ideas: anyOf("undecided", "unread"), Campaign: noneOf("c-off")},
		{Text: "overwintering", UpdatedAfter: new(fx.now.Add(-24 * time.Hour))},
		{Title: anyOf("e"), Tag: anyOf("none")},
		{ScopeCampaign: "c-explicit", Tag: anyOf("tg-finance")},
	}
	loc, err := time.LoadLocation("Pacific/Auckland")
	if err != nil {
		t.Fatal(err)
	}
	for bi, base := range bases {
		base.Facets = models.AssetFacets
		base.Location = loc
		res := fx.run(t, base)
		for field, counts := range res.Facets {
			for _, c := range counts {
				q := base
				q.Facets = nil
				switch field {
				case models.AssetFacetKind:
					q.Kind = anyOf(c.Value)
				case models.AssetFacetStatus:
					q.Status = anyOf(c.Value)
				case models.AssetFacetCampaign:
					q.Campaign = anyOf(c.Value)
				case models.AssetFacetTag:
					q.Tag = anyOf(c.Value)
				case models.AssetFacetIdeas:
					q.Ideas = anyOf(c.Value)
				case models.AssetFacetUpdated:
					since, _ := models.AssetDateSince(c.Value, fx.now, loc)
					q.UpdatedAfter, q.UpdatedBefore = &since, nil
				case models.AssetFacetCreated:
					since, _ := models.AssetDateSince(c.Value, fx.now, loc)
					q.CreatedAfter, q.CreatedBefore = &since, nil
				}
				if total := fx.run(t, q).Total; total != c.Count {
					t.Errorf("base %d: facet %s=%s counts %d, filtering by it gives %d", bi, field, c.Value, c.Count, total)
				}
			}
		}
	}
	res := fx.run(t, models.AssetQuery{Facets: models.AssetFacets})
	if got := len(res.Facets[models.AssetFacetKind]); got != len(models.AssetKinds) {
		t.Errorf("kind facet has %d values, want every kind", got)
	}
	tags := res.Facets[models.AssetFacetTag]
	if want := []string{"tg-gartner", "tg-finance", "none"}; len(tags) != 3 || tags[0].Value != want[0] || tags[1].Value != want[1] || tags[2].Value != want[2] {
		t.Errorf("tag facet = %+v, want %v by count with none last", tags, want)
	}
	for _, c := range res.Facets[models.AssetFacetCampaign] {
		if c.Value == "c-other" || c.Value == "c-deleted" {
			t.Errorf("campaign facet lists %s", c.Value)
		}
	}
}

func TestAssetQuery_Pagination(t *testing.T) {
	fx := seedAssetQueryFixture(t)
	for _, sort := range models.AssetSorts {
		t.Run(sort, func(t *testing.T) {
			base := models.AssetQuery{Sort: sort}
			if sort == models.AssetSortRelevance {
				base.Text = "overwintering"
			}
			want, err := fx.repo.QueryIDs(fx.ctx, fx.query(base))
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			q := base
			q.Limit = 2
			for range 10 {
				res := fx.run(t, q)
				got = append(got, listedIDs(res)...)
				if res.NextCursor == "" {
					break
				}
				q.Cursor = res.NextCursor
			}
			if !slices.Equal(got, want) {
				t.Errorf("paged %v, want %v", got, want)
			}
		})
	}
	byTitle, err := fx.repo.QueryIDs(fx.ctx, fx.query(models.AssetQuery{Sort: models.AssetSortTitleAsc, Kind: anyOf("pdf", "web", "note")}))
	if err != nil || !slices.Equal(byTitle, []string{fxWeb, fxPDF, fxNote}) {
		t.Errorf("title order = %v (%v), want case-insensitive", byTitle, err)
	}
	undecided, err := fx.repo.QueryIDs(fx.ctx, fx.query(models.AssetQuery{Sort: models.AssetSortIdeasUndecided}))
	if err != nil || undecided[0] != fxPDF {
		t.Errorf("-ideas_undecided order = %v (%v)", undecided, err)
	}

	first := fx.run(t, models.AssetQuery{Limit: 2})
	for _, cursor := range []string{"garbage!", first.NextCursor} {
		_, err := fx.repo.Query(fx.ctx, fx.query(models.AssetQuery{Sort: models.AssetSortTitleAsc, Cursor: cursor}))
		if !errors.Is(err, repository.ErrInvalidAssetCursor) {
			t.Errorf("cursor %q: err = %v, want ErrInvalidAssetCursor", cursor, err)
		}
	}
}
