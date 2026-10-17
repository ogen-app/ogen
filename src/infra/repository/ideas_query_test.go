package repository_test

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"slices"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/ideafit"
	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
	"github.com/ogen-app/ogen/src/usecase/ideas"
)

const (
	iqTenant      = "tenant-idea-query"
	iqOtherTenant = "tenant-idea-query-other"
)

// Ideas of the fixture, oldest first, by what makes each one distinct.
const (
	iqOrphan  = "iq-orphan"  // its analysis is gone; source a web page, later (snoozed), fit low, breakdown names c-launch
	iqAudio   = "iq-audio"   // from the audio asset, later with its reminder passed, unscored
	iqBob     = "iq-bob"     // manual, its author removed, attached to a deleted campaign, undecided
	iqPDF2    = "iq-pdf-2"   // from the PDF's c-launch lens, in c-launch, yes, fit medium
	iqAnn     = "iq-ann"     // manual by Ann, in c-launch, no, its note mentions bees
	iqPDF1    = "iq-pdf-1"   // from the PDF's workspace lens, undecided, fit high, phase Launch
	iqAssetPD = "iq-a-pdf"   // "Annual report", 12 pages
	iqAssetAu = "iq-a-audio" // "Customer call", one minute
	iqAssetWb = "iq-a-web"   // "Blog post" on www.example.com
)

var iqAllByCreated = []string{iqOrphan, iqAudio, iqBob, iqPDF2, iqAnn, iqPDF1}

type ideaQueryFixture struct {
	repo repository.IdeaRepository
	ctx  context.Context
	now  time.Time
}

func seedIdeaQueryFixture(t *testing.T) *ideaQueryFixture {
	t.Helper()
	db := openMigratedDB(t)
	fx := &ideaQueryFixture{
		repo: repository.NewIdeaRepository(db),
		ctx:  tenantctx.With(t.Context(), iqTenant),
		now:  time.Now().UTC().Truncate(time.Microsecond),
	}
	t.Cleanup(func() {
		ctx := context.Background()
		tenants := bun.List([]string{iqTenant, iqOtherTenant})
		for _, table := range []string{"ideas", "asset_analyses", "asset_files", "assets", "campaigns"} {
			_, _ = db.NewDelete().TableExpr(table).Where("tenant_id IN (?)", tenants).Exec(ctx)
		}
	})
	insert := func(ctx context.Context, model any) {
		t.Helper()
		if _, err := db.NewInsert().Model(model).Exec(ctx); err != nil {
			t.Fatalf("seed %T: %v", model, err)
		}
	}
	ctx, now := fx.ctx, fx.now
	ago := func(d time.Duration) time.Time { return now.Add(-d) }

	for _, a := range []struct {
		id, title, mime string
		typ             string
		url             *string
		pages           *int
		durationMS      int64
	}{
		{iqAssetPD, "Annual report", "application/pdf", models.AssetTypePDF, nil, new(12), 0},
		{iqAssetAu, "Customer call", "audio/mpeg", models.AssetTypeAudio, nil, nil, 60000},
		{iqAssetWb, "Blog post", "", models.AssetTypeURL, new("https://www.Example.com/post"), nil, 0},
	} {
		insert(ctx, &models.Asset{ID: a.id, Title: a.title, Status: models.AssetStatusReady, Type: new(a.typ),
			SourceURL: a.url, TagIDs: models.StringSlice{}, CreatedBy: "u", CreatedAt: now, UpdatedAt: now})
		if a.mime != "" {
			insert(ctx, &models.AssetFile{ID: "f-" + a.id, AssetID: a.id, OriginalName: a.id, MimeType: a.mime,
				S3Key: "k/" + a.id, PageCount: a.pages, DurationMs: a.durationMS, CreatedAt: now, UpdatedAt: now})
		}
	}
	for _, c := range []struct {
		id, name string
		deleted  bool
	}{{"iq-c-launch", "Launch", false}, {"iq-c-gone", "Gone", true}} {
		row := &models.Campaign{ID: c.id, Name: c.name, AssetIDs: models.StringSlice{},
			TargetPlatforms: models.CampaignPlatforms{}, PublishingDays: models.StringSlice{}}
		if c.deleted {
			row.DeletedAt = &now
		}
		insert(ctx, row)
	}
	insert(ctx, &models.AssetAnalysis{ID: "iq-l-pdf", AssetID: iqAssetPD, Status: models.AssetAnalysisStatusReady})
	insert(ctx, &models.AssetAnalysis{ID: "iq-l-pdf-c", AssetID: iqAssetPD, CampaignID: new("iq-c-launch"), Status: models.AssetAnalysisStatusReady})
	insert(ctx, &models.AssetAnalysis{ID: "iq-l-audio", AssetID: iqAssetAu, Status: models.AssetAnalysisStatusReady})

	later, yes, no := new(models.IdeaVerdictLater), new(models.IdeaVerdictYes), new(models.IdeaVerdictNo)
	breakdown := func(lens ideafit.LensKind, campaignID *string, phase string) *ideafit.Breakdown {
		b := &ideafit.Breakdown{Lens: lens, CampaignID: campaignID, Criteria: map[string]ideafit.Rating{}, Caps: []ideafit.Cap{}}
		if phase != "" {
			b.Criteria["phase_fit"] = ideafit.Rating{Level: 4, Phase: phase}
		}
		return b
	}
	analysed := func(id, title, analysis, asset string, campaignID *string, score *int, b *ideafit.Breakdown, created time.Time) *models.Idea {
		i := &models.Idea{ID: id, Title: title, Origin: models.IdeaOriginAssetAnalysis, SourceAssetID: new(asset),
			CampaignID: campaignID, FitScore: score, FitBreakdown: b, CreatedByName: "Ogen", CreatedAt: created, UpdatedAt: created}
		if analysis != "" {
			i.AssetAnalysisID = new(analysis)
		}
		return i
	}
	orphan := analysed(iqOrphan, "Bee keeping tips", "", iqAssetWb, nil, new(20), breakdown(ideafit.LensCampaign, new("iq-c-launch"), ""), ago(40*24*time.Hour))
	orphan.Verdict, orphan.RemindAt, orphan.DecidedAt = later, new(now.Add(48*time.Hour)), &now
	audio := analysed(iqAudio, "Customer quotes", "iq-l-audio", iqAssetAu, nil, nil, nil, ago(10*24*time.Hour))
	audio.Verdict, audio.RemindAt, audio.DecidedAt = later, new(ago(24*time.Hour)), &now
	pdf2 := analysed(iqPDF2, "Launch angle", "iq-l-pdf-c", iqAssetPD, new("iq-c-launch"), new(50),
		breakdown(ideafit.LensCampaign, new("iq-c-launch"), ""), ago(2*24*time.Hour))
	pdf2.Verdict, pdf2.DecidedAt = yes, &now
	pdf1 := analysed(iqPDF1, "Agents at the peak", "iq-l-pdf", iqAssetPD, nil, new(85),
		breakdown(ideafit.LensWorkspace, nil, "Launch"), ago(time.Hour))
	ann := &models.Idea{ID: iqAnn, Title: "Our hive", Note: "Bees are great", CampaignID: new("iq-c-launch"),
		Verdict: no, DecidedAt: &now, CreatedBy: new("iq-u-ann"), CreatedByName: "Ann", CreatedAt: ago(3 * time.Hour), UpdatedAt: now}
	bob := &models.Idea{ID: iqBob, Title: "Old thought", CampaignID: new("iq-c-gone"),
		CreatedByName: "Former Bob", CreatedAt: ago(5 * 24 * time.Hour), UpdatedAt: now}
	for _, i := range []*models.Idea{orphan, audio, bob, pdf2, ann, pdf1} {
		insert(ctx, i)
	}
	other := tenantctx.With(t.Context(), iqOtherTenant)
	insert(other, &models.Idea{ID: "iq-other", Title: "Agents elsewhere", CreatedByName: "X", CreatedAt: now, UpdatedAt: now})
	return fx
}

func (fx *ideaQueryFixture) query(t *testing.T, raw string) *models.IdeaQueryResult {
	t.Helper()
	v, err := url.ParseQuery(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	q, err := ideas.ParseQuery(v, fx.now)
	if err != nil {
		t.Fatalf("ParseQuery(%q): %v", raw, err)
	}
	res, err := fx.repo.Query(fx.ctx, q)
	if err != nil {
		t.Fatalf("Query(%q): %v", raw, err)
	}
	return res
}

func ideaIDs(list []models.Idea) []string {
	ids := make([]string, len(list))
	for i, idea := range list {
		ids[i] = idea.ID
	}
	return ids
}

func TestIdeaQueryFilters(t *testing.T) {
	fx := seedIdeaQueryFixture(t)
	created4hAgo := url.QueryEscape(fx.now.Add(-4 * time.Hour).Format(time.RFC3339Nano))
	for _, tc := range []struct {
		query string
		want  []string
	}{
		{"", iqAllByCreated},
		{"pile=waiting", []string{iqAudio, iqBob, iqPDF1}},
		{"pile=later", []string{iqOrphan}},
		{"pile_not=waiting,later", []string{iqPDF2, iqAnn}},
		{"origin=manual", []string{iqBob, iqAnn}},
		{"source_asset=none", []string{iqBob, iqAnn}},
		{"source_asset=" + iqAssetPD, []string{iqPDF2, iqPDF1}},
		{"asset_id=" + iqAssetPD, []string{iqPDF2, iqPDF1}},
		{"source_asset_not=none", []string{iqOrphan, iqAudio, iqPDF2, iqPDF1}},
		{"kind=audio,web", []string{iqOrphan, iqAudio}},
		{"kind_not=pdf", []string{iqOrphan, iqAudio, iqBob, iqAnn}},
		{"campaign_id=iq-c-launch", []string{iqPDF2, iqAnn}},
		{"campaign_id=none", []string{iqOrphan, iqAudio, iqBob, iqPDF1}},
		{"fit=high", []string{iqPDF1}},
		{"fit=medium,low", []string{iqOrphan, iqPDF2}},
		{"fit=unscored", []string{iqAudio, iqBob, iqAnn}},
		{"phase=Launch", []string{iqPDF1}},
		{"phase_not=Launch", []string{iqOrphan, iqAudio, iqBob, iqPDF2, iqAnn}},
		{"author=iq-u-ann", []string{iqAnn}},
		{"author_not=iq-u-ann", []string{iqOrphan, iqAudio, iqBob, iqPDF2, iqPDF1}},
		{"text=BEES", []string{iqAnn}},
		{"text=the+PEAK", []string{iqPDF1}},
		{"text=%25", nil},
		{"created_after=" + created4hAgo, []string{iqAnn, iqPDF1}},
		{"origin=asset_analysis&fit_not=unscored&pile_not=yes", []string{iqOrphan, iqPDF1}},
	} {
		res := fx.query(t, tc.query)
		if got := ideaIDs(res.Ideas); !slices.Equal(got, tc.want) && (len(got) > 0 || len(tc.want) > 0) {
			t.Errorf("%q: ideas = %v, want %v", tc.query, got, tc.want)
		}
		if res.Total != len(tc.want) || res.TotalUnfiltered != len(iqAllByCreated) {
			t.Errorf("%q: total %d of %d, want %d of %d", tc.query, res.Total, res.TotalUnfiltered, len(tc.want), len(iqAllByCreated))
		}
	}
}

func TestIdeaQueryPilesPartitionTheMatch(t *testing.T) {
	fx := seedIdeaQueryFixture(t)
	for _, raw := range []string{"", "pile=waiting", "pile=no&origin=manual", "kind=pdf"} {
		res := fx.query(t, raw)
		sum := 0
		for _, n := range res.Piles {
			sum += n
		}
		withoutPile := fx.query(t, removePile(raw))
		if sum != withoutPile.Total {
			t.Errorf("%q: piles %v sum to %d, want %d", raw, res.Piles, sum, withoutPile.Total)
		}
	}
	res := fx.query(t, "")
	want := map[string]int{models.IdeaPileWaiting: 3, models.IdeaPileYes: 1, models.IdeaPileLater: 1, models.IdeaPileNo: 1}
	for k, n := range want {
		if res.Piles[k] != n {
			t.Errorf("piles = %v, want %v (a woken reminder is waiting only)", res.Piles, want)
			break
		}
	}
}

func removePile(raw string) string {
	v, _ := url.ParseQuery(raw)
	v.Del("pile")
	return v.Encode()
}

func TestIdeaQueryFacetsExcludeTheirOwnField(t *testing.T) {
	fx := seedIdeaQueryFixture(t)
	res := fx.query(t, "origin=manual&facets=origin,source_asset,kind,campaign_id,fit,author,phase,created")
	counts := func(field string) map[string]int {
		out := map[string]int{}
		for _, c := range res.Facets[field] {
			out[c.Value] = c.Count
		}
		return out
	}
	if got := counts("origin"); got["asset_analysis"] != 4 || got["manual"] != 2 {
		t.Errorf("origin facet = %v, want its own filter ignored", res.Facets["origin"])
	}
	src := res.Facets["source_asset"]
	if len(src) != 4 || src[len(src)-1] != (models.AssetFacetCount{Value: "none", Count: 2}) || src[0].Count != 0 {
		t.Errorf("source_asset facet = %v, want every source at 0 and none last at 2", src)
	}
	if len(res.SourceAssetLabels) != 3 || res.SourceAssetLabels[0] != (models.IdeaFacetAsset{ID: iqAssetPD, Title: "Annual report", Kind: models.AssetKindPDF}) {
		t.Errorf("source labels = %v, want every source asset by title", res.SourceAssetLabels)
	}
	if got := counts("kind"); len(res.Facets["kind"]) != len(models.AssetKinds) || got["pdf"] != 0 {
		t.Errorf("kind facet = %v, want every kind, all 0 under origin=manual", res.Facets["kind"])
	}
	if got := counts("campaign_id"); got["iq-c-launch"] != 1 || got["none"] != 1 {
		t.Errorf("campaign facet = %v, want c-launch 1 and none 1 (the deleted campaign reads as none)", res.Facets["campaign_id"])
	}
	if got := counts("author"); len(got) != 1 || got["iq-u-ann"] != 1 {
		t.Errorf("author facet = %v, want only the current member", res.Facets["author"])
	}
	if got := counts("fit"); got["unscored"] != 2 || len(res.Facets["fit"]) != 4 {
		t.Errorf("fit facet = %v", res.Facets["fit"])
	}
	if got := counts("phase"); got["Launch"] != 0 || len(got) != 1 {
		t.Errorf("phase facet = %v, want Launch at 0", res.Facets["phase"])
	}
	if got := counts("created"); got["month"] > 2 || len(res.Facets["created"]) != 4 {
		t.Errorf("created facet = %v", res.Facets["created"])
	}

	scoped := fx.query(t, "scope_campaign=iq-c-launch&facets=campaign_id,origin")
	if _, ok := scoped.Facets["campaign_id"]; ok {
		t.Error("campaign facet returned inside a campaign scope")
	}
	if scoped.Total != 2 || scoped.TotalUnfiltered != 2 {
		t.Errorf("scoped total %d of %d, want 2 of 2", scoped.Total, scoped.TotalUnfiltered)
	}
}

func TestIdeaQueryGroupsAndSorts(t *testing.T) {
	fx := seedIdeaQueryFixture(t)
	res := fx.query(t, "group=source&sort=fit")
	if got, want := ideaIDs(res.Ideas), []string{iqPDF1, iqPDF2, iqOrphan, iqAudio, iqBob, iqAnn}; !slices.Equal(got, want) {
		t.Errorf("grouped by source = %v, want %v (groups by title, none last, fit within)", got, want)
	}
	wantGroups := []models.AssetGroupCount{{Key: iqAssetPD, Count: 2}, {Key: iqAssetWb, Count: 1}, {Key: iqAssetAu, Count: 1}, {Key: "none", Count: 2}}
	if !slices.Equal(res.Groups, wantGroups) {
		t.Errorf("groups = %v, want %v", res.Groups, wantGroups)
	}
	if res.Ideas[0].GroupKey != iqAssetPD || res.Ideas[5].GroupKey != "none" {
		t.Errorf("group keys = %q … %q", res.Ideas[0].GroupKey, res.Ideas[5].GroupKey)
	}

	for _, tc := range []struct {
		query string
		want  []string
	}{
		{"sort=created_desc", []string{iqPDF1, iqAnn, iqPDF2, iqBob, iqAudio, iqOrphan}},
		{"sort=fit", []string{iqPDF1, iqPDF2, iqOrphan, iqAudio, iqBob, iqAnn}},
		{"pile=waiting&sort=waiting", []string{iqBob, iqAudio, iqPDF1}},
		{"group=fit&sort=created", []string{iqPDF1, iqPDF2, iqOrphan, iqAudio, iqBob, iqAnn}},
		{"group=author", []string{iqAnn, iqOrphan, iqAudio, iqBob, iqPDF2, iqPDF1}},
		{"group=campaign&sort=created_desc", []string{iqAnn, iqPDF2, iqPDF1, iqBob, iqAudio, iqOrphan}},
		{"group=origin", []string{iqOrphan, iqAudio, iqPDF2, iqPDF1, iqBob, iqAnn}},
	} {
		if got := ideaIDs(fx.query(t, tc.query).Ideas); !slices.Equal(got, tc.want) {
			t.Errorf("%q = %v, want %v", tc.query, got, tc.want)
		}
	}
}

func TestIdeaQueryCursorPaging(t *testing.T) {
	fx := seedIdeaQueryFixture(t)
	for _, base := range []string{"sort=created_desc", "sort=fit", "group=source&sort=fit", "group=created&sort=waiting"} {
		want := ideaIDs(fx.query(t, base).Ideas)
		var got []string
		cursor := ""
		for range 10 {
			raw := base + "&limit=2"
			if cursor != "" {
				raw += "&cursor=" + cursor
			}
			res := fx.query(t, raw)
			if len(res.Ideas) > 2 {
				t.Fatalf("%q: page of %d", raw, len(res.Ideas))
			}
			got = append(got, ideaIDs(res.Ideas)...)
			if cursor = res.NextCursor; cursor == "" {
				break
			}
		}
		if !slices.Equal(got, want) {
			t.Errorf("%q paged = %v, want %v", base, got, want)
		}
	}

	first := fx.query(t, "sort=fit&limit=2")
	v := url.Values{"sort": {"created"}, "cursor": {first.NextCursor}}
	q, err := ideas.ParseQuery(v, fx.now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fx.repo.Query(fx.ctx, q); !errors.Is(err, repository.ErrInvalidCursor) {
		t.Errorf("cursor of another sort: err = %v, want ErrInvalidCursor", err)
	}
	q.Cursor, q.ScopeCampaign = "", "iq-c-gone"
	if _, err := fx.repo.Query(fx.ctx, q); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("scope of a deleted campaign: err = %v, want sql.ErrNoRows", err)
	}
}

func TestIdeaQueryPayload(t *testing.T) {
	fx := seedIdeaQueryFixture(t)
	byID := map[string]models.Idea{}
	for _, idea := range fx.query(t, "").Ideas {
		byID[idea.ID] = idea
	}
	pdf2 := byID[iqPDF2].SourceAsset
	if pdf2 == nil || pdf2.Kind != models.AssetKindPDF || pdf2.PageCount == nil || *pdf2.PageCount != 12 ||
		pdf2.Reading == nil || *pdf2.Reading != (models.IdeaReading{CampaignID: "iq-c-launch", CampaignName: "Launch"}) {
		t.Errorf("pdf-2 source = %+v, want a 12-page PDF read in Launch", pdf2)
	}
	if s := byID[iqPDF1].SourceAsset; s == nil || s.Reading != nil || s.DurationMS != nil {
		t.Errorf("pdf-1 source = %+v, want the workspace lens", s)
	}
	if s := byID[iqAudio].SourceAsset; s == nil || s.DurationMS == nil || *s.DurationMS != 60000 || s.PageCount != nil {
		t.Errorf("audio source = %+v, want one minute", s)
	}
	orphan := byID[iqOrphan].SourceAsset
	if orphan == nil || orphan.Domain == nil || *orphan.Domain != "example.com" || orphan.Reading == nil ||
		orphan.Reading.CampaignID != "iq-c-launch" {
		t.Errorf("orphan source = %+v, want the web page, read in Launch per its breakdown", orphan)
	}
	if a := byID[iqAnn].Author; a == nil || a.ID == nil || *a.ID != "iq-u-ann" || a.Name != "Ann" {
		t.Errorf("ann author = %+v", a)
	}
	if a := byID[iqBob].Author; a == nil || a.ID != nil || a.Name != "Former Bob" {
		t.Errorf("bob author = %+v, want a former member's name without id", a)
	}
	if byID[iqPDF1].Author != nil || byID[iqBob].SourceAsset != nil {
		t.Error("analysis ideas have no author and manual ideas no source")
	}
}

func TestIdeaQueryIDs(t *testing.T) {
	fx := seedIdeaQueryFixture(t)
	v, _ := url.ParseQuery("pile=waiting&limit=1&sort=fit")
	q, err := ideas.ParseQuery(v, fx.now)
	if err != nil {
		t.Fatal(err)
	}
	ids, total, err := fx.repo.QueryIDs(fx.ctx, q, 10)
	slices.Sort(ids)
	if err != nil || total != 3 || !slices.Equal(ids, []string{iqAudio, iqBob, iqPDF1}) {
		t.Errorf("ids = %v, total %d, %v; want every waiting idea, ignoring the page size", ids, total, err)
	}
	if ids, total, _ := fx.repo.QueryIDs(fx.ctx, q, 2); len(ids) != 2 || total != 3 {
		t.Errorf("capped: %d ids, total %d; want 2 of 3", len(ids), total)
	}
	q.ScopeCampaign = "iq-c-gone"
	if _, _, err := fx.repo.QueryIDs(fx.ctx, q, 10); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("deleted scope: err = %v, want sql.ErrNoRows", err)
	}
}

func TestIdeaCountViews(t *testing.T) {
	fx := seedIdeaQueryFixture(t)
	var qs []models.IdeaQuery
	for _, raw := range []string{"", "origin=manual", "scope_campaign=iq-c-gone", "kind=pdf"} {
		v, _ := url.ParseQuery(raw)
		q, err := ideas.ParseQuery(v, fx.now)
		if err != nil {
			t.Fatal(err)
		}
		qs = append(qs, q)
	}
	got, err := fx.repo.CountViews(fx.ctx, qs)
	if err != nil {
		t.Fatal(err)
	}
	want := []models.IdeaViewCounts{{Total: 6, Waiting: 3}, {Total: 2, Waiting: 1}, {}, {Total: 2, Waiting: 1}}
	if !slices.Equal(got, want) {
		t.Errorf("counts = %v, want %v", got, want)
	}
}
