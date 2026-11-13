package repository_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
)

// Each filter of the posts list narrows the match; QueryIDs pages it and
// counts all of it, and Query returns the same posts hydrated.
func TestPostRepository_Query(t *testing.T) {
	db := openMigratedDB(t)
	ctx := tenantCtx()
	repo := repository.NewPostRepository(db)

	t0 := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	seed := []struct {
		id, campaign, platform, postType string
		status                           models.PostStatus
		scheduled                        *time.Time
	}{
		{"p1", "c1", "linkedin", "text", models.PostStatusDraft, nil},
		{"p2", "c1", "linkedin", "image", models.PostStatusScheduled, new(t0.Add(24 * time.Hour))},
		{"p3", "c1", "x", "thread", models.PostStatusFailed, new(t0.Add(72 * time.Hour))},
		{"p4", "c2", "x", "text", models.PostStatusPublished, new(t0.Add(48 * time.Hour))},
		{"p5", "c1", "", "", models.PostStatusPlanned, nil},
	}
	for i, s := range seed {
		post := &models.Post{
			ID: s.id, CampaignID: s.campaign, PlatformID: s.platform, PlatformPostType: s.postType,
			Content: "Body", MediaURLs: models.StringSlice{}, UsedAssetIDs: models.StringSlice{},
			Status: s.status, ScheduledAt: s.scheduled, CTAType: models.CTATypeNone, CreatedBy: "user-1",
			CreatedAt: t0.Add(time.Duration(i) * time.Minute), UpdatedAt: t0,
		}
		if _, err := db.NewInsert().Model(post).Exec(ctx); err != nil {
			t.Fatalf("seed %s: %v", s.id, err)
		}
	}

	cases := map[string]struct {
		q    models.PostQuery
		want []string
	}{
		"everything":        {models.PostQuery{}, []string{"p1", "p2", "p3", "p4", "p5"}},
		"status":            {models.PostQuery{Status: models.AssetValueFilter{Any: []string{"draft", "failed"}}}, []string{"p1", "p3"}},
		"platform not":      {models.PostQuery{Platform: models.AssetValueFilter{Not: []string{"x"}}}, []string{"p1", "p2", "p5"}},
		"post type":         {models.PostQuery{PostType: models.AssetValueFilter{Any: []string{"text"}}}, []string{"p1", "p4"}},
		"campaign scope":    {models.PostQuery{ScopeCampaign: "c2"}, []string{"p4"}},
		"campaign filter":   {models.PostQuery{Campaign: models.AssetValueFilter{Not: []string{"c1"}}}, []string{"p4"}},
		"unscheduled":       {models.PostQuery{Scheduled: models.PostScheduledNo}, []string{"p1", "p5"}},
		"scheduled":         {models.PostQuery{Scheduled: models.PostScheduledYes, ScopeCampaign: "c1"}, []string{"p2", "p3"}},
		"days":              {models.PostQuery{ScheduledAfter: new(t0.Add(48 * time.Hour)), ScheduledBefore: new(t0.Add(72 * time.Hour))}, []string{"p4"}},
		"from a day onward": {models.PostQuery{ScheduledAfter: new(t0.Add(48 * time.Hour))}, []string{"p3", "p4"}},
		"these ids":         {models.PostQuery{IDs: []string{"p2", "p3", "gone"}, Scheduled: models.PostScheduledYes, ScopeCampaign: "c1"}, []string{"p2", "p3"}},
		"no ids":            {models.PostQuery{IDs: []string{}}, nil},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			ids, total, err := repo.QueryIDs(ctx, tc.q, 100)
			if err != nil || !slices.Equal(ids, tc.want) || total != len(tc.want) {
				t.Fatalf("ids = %v (total %d), %v; want %v", ids, total, err, tc.want)
			}
		})
	}

	// Another tenant's posts neither match nor count.
	other := &models.Post{
		ID: "p-other", CampaignID: "c1", Content: "Body", MediaURLs: models.StringSlice{}, UsedAssetIDs: models.StringSlice{},
		Status: models.PostStatusDraft, CTAType: models.CTATypeNone, CreatedBy: "user-2", CreatedAt: t0, UpdatedAt: t0,
	}
	if _, err := db.NewInsert().Model(other).Exec(tenantctx.With(context.Background(), "other-tenant")); err != nil {
		t.Fatalf("seed other tenant: %v", err)
	}

	ids, total, err := repo.QueryIDs(ctx, models.PostQuery{}, 2)
	if err != nil || !slices.Equal(ids, []string{"p1", "p2"}) || total != 5 {
		t.Fatalf("paged = %v (total %d), %v", ids, total, err)
	}
	posts, err := repo.Query(ctx, models.PostQuery{Status: models.AssetValueFilter{Any: []string{"scheduled"}}})
	if err != nil || len(posts) != 1 || posts[0].ID != "p2" || posts[0].ScheduledAt == nil {
		t.Fatalf("query = %+v, %v", posts, err)
	}
}
