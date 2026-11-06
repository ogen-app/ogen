package repository_test

import (
	"context"
	"errors"
	"maps"
	"strings"
	"testing"

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
	if _, err := titles.Titles(ctx, viewcontext.KindVoice, []string{"v1"}); !errors.Is(err, repository.ErrUntitledKind) {
		t.Fatal("an unsupported kind read titles")
	}

	other := tenantctx.With(context.Background(), "tenant-other")
	if got, err := titles.Titles(other, viewcontext.KindIdea, []string{ideaID}); err != nil || len(got) != 0 {
		t.Fatalf("another tenant read %v (err %v)", got, err)
	}
}
