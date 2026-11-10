package repository_test

import (
	"context"
	"errors"
	"maps"
	"strings"
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/domain/viewcontext"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
)

func TestEntityTitles(t *testing.T) {
	// Seeded without the campaign type and platform rows the foreign keys
	// would want.
	db := openMigratedDB(t)
	ctx := tenantCtx()
	campaigns := repository.NewCampaignRepository(db, nil, nil, nil)
	posts := repository.NewPostRepository(db)
	ideas := repository.NewIdeaRepository(db)
	titles := repository.NewEntityTitleRepository(db)

	live := seedCampaign(t, campaigns, ctx, "Spring launch")
	gone := seedCampaign(t, campaigns, ctx, "Old")
	if _, err := campaigns.Delete(ctx, gone); err != nil {
		t.Fatal(err)
	}
	titled := seedSubmissionPost(t, posts, live, models.PostStatusDraft, "")
	untitled := seedSubmissionPost(t, posts, live, models.PostStatusDraft, "")
	untitled.Title, untitled.Content = "", strings.Repeat("word ", 40)
	if err := posts.Update(ctx, untitled); err != nil {
		t.Fatal(err)
	}
	ideaID, err := models.NewID()
	if err != nil {
		t.Fatal(err)
	}
	if err := ideas.Create(ctx, &models.Idea{ID: ideaID, Title: "Pricing myths"}); err != nil {
		t.Fatal(err)
	}
	assetID, err := models.NewID()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	asset := &models.Asset{ID: assetID, Title: "Q3 deck", Content: "body", Status: models.AssetStatusReady,
		TagIDs: models.StringSlice{}, CreatedBy: "user-1", CreatedAt: now, UpdatedAt: now}
	if err := repository.NewAssetRepository(db, nil, nil).Create(ctx, asset); err != nil {
		t.Fatal(err)
	}

	got, err := titles.Titles(ctx, viewcontext.KindCampaign, []string{live, gone, "missing"})
	if err != nil || !maps.Equal(got, map[string]string{live: "Spring launch"}) {
		t.Fatalf("campaigns = %v, %v; want only the live one", got, err)
	}
	got, err = titles.Titles(ctx, viewcontext.KindPost, []string{titled.ID, untitled.ID})
	if err != nil || got[titled.ID] != "t" || len(got[untitled.ID]) != 80 {
		t.Fatalf("posts = %v, %v; want the title, else the opening 80 characters", got, err)
	}
	got, err = titles.Titles(ctx, viewcontext.KindIdea, []string{ideaID})
	if err != nil || got[ideaID] != "Pricing myths" {
		t.Fatalf("ideas = %v, %v", got, err)
	}
	got, err = titles.Titles(ctx, viewcontext.KindAsset, []string{assetID})
	if err != nil || got[assetID] != "Q3 deck" {
		t.Fatalf("assets = %v, %v", got, err)
	}
	brand := repository.NewBrandRepository(db)
	voiceID, audienceID, seriesID, goneSeriesID := mustID(t), mustID(t), mustID(t), mustID(t)
	if err := brand.CreateVoice(ctx, &models.BrandVoice{ID: voiceID, Name: "Plain"}); err != nil {
		t.Fatal(err)
	}
	if err := brand.CreateAudience(ctx, &models.BrandAudience{ID: audienceID, Name: "Founders"}); err != nil {
		t.Fatal(err)
	}
	series := repository.NewSeriesRepository(db)
	for id, name := range map[string]string{seriesID: "Monday tips", goneSeriesID: "Retired"} {
		if err := series.Create(ctx, &models.Series{ID: id, Name: name, Supply: models.SeriesSupplySelf}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := series.SoftDelete(ctx, goneSeriesID); err != nil {
		t.Fatal(err)
	}
	for kind, want := range map[viewcontext.EntityKind]map[string]string{
		viewcontext.KindVoice:    {voiceID: "Plain"},
		viewcontext.KindAudience: {audienceID: "Founders"},
		viewcontext.KindSeries:   {seriesID: "Monday tips"},
	} {
		got, err := titles.Titles(ctx, kind, []string{voiceID, audienceID, seriesID, goneSeriesID, "missing"})
		if err != nil || !maps.Equal(got, want) {
			t.Fatalf("%s titles = %v, %v; want %v", kind, got, err, want)
		}
	}
	if got, err := titles.Titles(tenantctx.With(context.Background(), "tenant-other"), viewcontext.KindVoice, []string{voiceID}); err != nil || len(got) != 0 {
		t.Fatalf("another tenant read voice %v (err %v)", got, err)
	}
	factID := mustID(t)
	statement := "Founded in 2019 " + strings.Repeat("and growing ", 10)
	if err := brand.CreateFact(ctx, &models.BrandFact{ID: factID, Statement: statement, Subject: "us", Kind: "documented", Source: "site"}); err != nil {
		t.Fatal(err)
	}
	if got, err := titles.Titles(ctx, viewcontext.KindFact, []string{factID, "missing"}); err != nil || got[factID] != statement[:80] || len(got) != 1 {
		t.Fatalf("facts = %v, %v; want the statement's opening 80 characters", got, err)
	}
	// Every kind a view context can name is checked.
	for _, kind := range viewcontext.Kinds() {
		if _, err := titles.Titles(ctx, kind, nil); err != nil {
			t.Errorf("%s has no title source: %v", kind, err)
		}
	}
	// A campaign's posts only.
	otherCampaign := seedCampaign(t, campaigns, ctx, "Other")
	elsewhere := seedSubmissionPost(t, posts, otherCampaign, models.PostStatusDraft, "")
	got, err = titles.CampaignTitles(ctx, viewcontext.KindPost, live, []string{titled.ID, elsewhere.ID})
	if err != nil || len(got) != 1 || got[titled.ID] != "t" {
		t.Fatalf("campaign posts = %v, %v; want only the live campaign's", got, err)
	}
	if _, err := titles.CampaignTitles(ctx, viewcontext.KindVoice, live, []string{voiceID}); !errors.Is(err, repository.ErrUntitledKind) {
		t.Fatal("a kind outside campaigns read campaign titles")
	}
	if _, err := titles.CampaignTitles(ctx, viewcontext.KindPost, "", []string{titled.ID, elsewhere.ID}); !errors.Is(err, repository.ErrUntitledKind) {
		t.Fatal("no campaign read the whole tenant's posts")
	}

	other := tenantctx.With(context.Background(), "tenant-other")
	if got, err := titles.Titles(other, viewcontext.KindIdea, []string{ideaID}); err != nil || len(got) != 0 {
		t.Fatalf("another tenant read %v (err %v)", got, err)
	}
	if got, err := titles.Titles(other, viewcontext.KindAsset, []string{assetID}); err != nil || len(got) != 0 {
		t.Fatalf("another tenant read asset %v (err %v)", got, err)
	}
}
