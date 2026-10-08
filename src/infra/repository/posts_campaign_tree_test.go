package repository_test

import (
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
)

func TestListCampaignPostTree(t *testing.T) {
	db := openMigratedDB(t)
	repo := repository.NewPostRepository(db)
	base := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	at := func(d time.Duration) *time.Time { v := base.Add(d); return &v }

	type campaignSeed struct {
		tenantID, id, name string
		status             models.CampaignStatus
		start              *time.Time
		archived, deleted  bool
	}
	seedCampaign := func(s campaignSeed) {
		t.Helper()
		c := &models.Campaign{
			ID: s.id, Name: s.name, TargetPlatforms: models.CampaignPlatforms{}, PublishingDays: models.StringSlice{},
			TagIDs: models.StringSlice{}, AssetIDs: models.StringSlice{}, Status: s.status, Timezone: "Europe/Kyiv",
			StartDate: s.start, CreatedBy: "user-1", CreatedAt: base, UpdatedAt: base,
		}
		if s.archived {
			c.ArchivedAt = &base
		}
		if s.deleted {
			c.DeletedAt = &base
		}
		if _, err := db.NewInsert().Model(c).Exec(tenantctx.With(t.Context(), s.tenantID)); err != nil {
			t.Fatalf("seed campaign %s: %v", s.id, err)
		}
	}
	seedPost := func(tenantID, id, campaignID, platformID string, status models.PostStatus, scheduled *time.Time, created time.Duration) {
		t.Helper()
		p := &models.Post{
			ID: id, CampaignID: campaignID, PlatformID: platformID, Title: "Title " + id, Content: "secret body",
			MediaURLs: models.StringSlice{}, Status: status, ScheduledAt: scheduled, CTAType: models.CTATypeNone,
			CreatedBy: "user-1", CreatedAt: base.Add(created), UpdatedAt: base,
		}
		if _, err := db.NewInsert().Model(p).Exec(tenantctx.With(t.Context(), tenantID)); err != nil {
			t.Fatalf("seed post %s: %v", id, err)
		}
	}

	own := models.DefaultTenantID
	seedCampaign(campaignSeed{tenantID: own, id: "c-draft", name: "Draft", status: models.StatusDraft, start: at(0)})
	seedCampaign(campaignSeed{tenantID: own, id: "c-active-undated", name: "Active A", status: models.StatusActive})
	seedCampaign(campaignSeed{tenantID: own, id: "c-active-old", name: "Active B", status: models.StatusActive, start: at(-48 * time.Hour)})
	seedCampaign(campaignSeed{tenantID: own, id: "c-active-new", name: "Active C", status: models.StatusActive, start: at(24 * time.Hour)})
	seedCampaign(campaignSeed{tenantID: own, id: "c-completed", name: "Done", status: models.StatusCompleted})
	seedCampaign(campaignSeed{tenantID: own, id: "c-scheduled", name: "Soon", status: models.StatusScheduled})
	seedCampaign(campaignSeed{tenantID: own, id: "c-paused", name: "Paused", status: models.StatusPaused})
	seedCampaign(campaignSeed{tenantID: own, id: "c-status-archived", name: "Old", status: models.StatusArchived})
	seedCampaign(campaignSeed{tenantID: own, id: "c-archived-at", name: "Shelved", status: models.StatusActive, archived: true})
	seedCampaign(campaignSeed{tenantID: own, id: "c-deleted", name: "Gone", status: models.StatusActive, deleted: true})
	seedCampaign(campaignSeed{tenantID: "t-other", id: "c-theirs", name: "Theirs", status: models.StatusActive})

	const linkedIn = "AXqWG7U2qnpt"
	seedPost(own, "p-unscheduled", "c-active-new", "", models.PostStatusDraft, nil, 0)
	seedPost(own, "p-late", "c-active-new", linkedIn, models.PostStatusPublished, at(72*time.Hour), 0)
	seedPost(own, "p-early", "c-active-new", linkedIn, models.PostStatusScheduled, at(time.Hour), 0)
	seedPost(own, "p-tie-b", "c-active-new", "", models.PostStatusReadyForPublish, at(2*time.Hour), time.Minute)
	seedPost(own, "p-tie-a", "c-active-new", "", models.PostStatusFailed, at(2*time.Hour), 0)
	seedPost(own, "p-shelved", "c-archived-at", "", models.PostStatusDraft, nil, 0)
	seedPost("t-other", "p-theirs", "c-theirs", "", models.PostStatusDraft, nil, 0)
	for i, att := range []models.PostAttachment{
		{ID: "att-1", PostID: "p-early", MimeType: "image/png", S3Key: "k"},
		{ID: "att-2", PostID: "p-late", MimeType: "video/mp4", S3Key: "k2"},
		{ID: "att-3", PostID: "p-late", MimeType: "image/png", S3Key: "k3"},
	} {
		att.Position, att.CreatedBy = i, "user-1"
		if _, err := db.NewInsert().Model(&att).Exec(tenantCtx()); err != nil {
			t.Fatalf("seed attachment: %v", err)
		}
	}
	if _, err := db.NewUpdate().Model((*models.Post)(nil)).Set("platform_post_type = ?", "video").
		Where("id = ?", "p-late").Exec(tenantCtx()); err != nil {
		t.Fatalf("set post type: %v", err)
	}

	got, err := repo.ListCampaignPostTree(tenantCtx(), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, c := range got {
		ids = append(ids, c.ID)
	}
	want := []string{"c-active-new", "c-active-old", "c-active-undated", "c-scheduled", "c-draft", "c-paused", "c-completed"}
	if !slices.Equal(ids, want) {
		t.Fatalf("campaigns = %v, want %v", ids, want)
	}
	for _, c := range got[1:] {
		if c.Posts == nil || len(c.Posts) != 0 {
			t.Fatalf("campaign %s posts = %#v, want empty non-nil", c.ID, c.Posts)
		}
	}

	top := got[0]
	if top.Name != "Active C" || top.Status != models.StatusActive || top.Timezone != "Europe/Kyiv" ||
		top.StartDate == nil || !top.StartDate.Equal(*at(24 * time.Hour)) || top.EndDate != nil {
		t.Fatalf("campaign row = %+v", top)
	}
	var postIDs []string
	for _, p := range top.Posts {
		postIDs = append(postIDs, p.ID)
	}
	wantPosts := []string{"p-early", "p-tie-a", "p-tie-b", "p-late", "p-unscheduled"}
	if !slices.Equal(postIDs, wantPosts) {
		t.Fatalf("posts = %v, want %v (scheduled_at asc nulls last, then created_at)", postIDs, wantPosts)
	}
	early := top.Posts[0]
	if early.PlatformID != linkedIn || early.PlatformName != "LinkedIn" || early.AttachmentCount != 1 || early.VideoCount != 0 ||
		early.PlatformPostType != "" || early.Status != models.PostStatusScheduled || early.Title != "Title p-early" || early.ScheduledAt == nil {
		t.Fatalf("early post = %+v", early)
	}
	if late := top.Posts[3]; late.PlatformPostType != "video" || late.AttachmentCount != 2 || late.VideoCount != 1 {
		t.Fatalf("late post = %+v", late)
	}
	if u := top.Posts[4]; u.PlatformID != "" || u.PlatformName != "" || u.ScheduledAt != nil || u.AttachmentCount != 0 {
		t.Fatalf("unscheduled post = %+v", u)
	}

	// Caps: campaigns keep their order; posts keep the earliest per campaign.
	got, err = repo.ListCampaignPostTree(tenantCtx(), 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != "c-active-new" || got[1].ID != "c-active-old" {
		t.Fatalf("capped campaigns = %+v", got)
	}
	if len(got[0].Posts) != 2 || got[0].Posts[0].ID != "p-early" || got[0].Posts[1].ID != "p-tie-a" {
		t.Fatalf("capped posts = %+v", got[0].Posts)
	}

	// The other tenant sees only its own campaign and post.
	got, err = repo.ListCampaignPostTree(tenantctx.With(t.Context(), "t-other"), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "c-theirs" || len(got[0].Posts) != 1 || got[0].Posts[0].ID != "p-theirs" {
		t.Fatalf("other tenant tree = %+v", got)
	}
}

func TestGetCampaignPostTree(t *testing.T) {
	db := openMigratedDB(t)
	repo := repository.NewPostRepository(db)
	base := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)

	seedCampaign := func(tenantID, id string, status models.CampaignStatus, archived, deleted bool) {
		t.Helper()
		c := &models.Campaign{
			ID: id, Name: "Campaign " + id, TargetPlatforms: models.CampaignPlatforms{}, PublishingDays: models.StringSlice{},
			TagIDs: models.StringSlice{}, AssetIDs: models.StringSlice{}, Status: status, Timezone: "Europe/Kyiv",
			CreatedBy: "user-1", CreatedAt: base, UpdatedAt: base,
		}
		if archived {
			c.ArchivedAt = &base
		}
		if deleted {
			c.DeletedAt = &base
		}
		if _, err := db.NewInsert().Model(c).Exec(tenantctx.With(t.Context(), tenantID)); err != nil {
			t.Fatalf("seed campaign %s: %v", id, err)
		}
	}
	own := models.DefaultTenantID
	seedCampaign(own, "c-big", models.StatusActive, false, false)
	seedCampaign(own, "c-archived", models.StatusArchived, true, false)
	seedCampaign(own, "c-empty", models.StatusDraft, false, false)
	seedCampaign(own, "c-deleted", models.StatusActive, false, true)
	seedCampaign("t-other", "c-theirs", models.StatusActive, false, false)

	// More posts than the listing's per-campaign cap, scheduled in reverse
	// creation order so the result must be re-sorted.
	const n = repository.MaxTreePostsPerCampaign + 5
	posts := make([]*models.Post, 0, n)
	for i := range n {
		at := base.Add(time.Duration(n-i) * time.Hour)
		posts = append(posts, &models.Post{
			ID: fmt.Sprintf("p-%03d", i), CampaignID: "c-big", Title: "t", Content: "body",
			MediaURLs: models.StringSlice{}, Status: models.PostStatusDraft, ScheduledAt: &at, CTAType: models.CTATypeNone,
			CreatedBy: "user-1", CreatedAt: base, UpdatedAt: base,
		})
	}
	if _, err := db.NewInsert().Model(&posts).Exec(tenantCtx()); err != nil {
		t.Fatalf("seed posts: %v", err)
	}
	shelved := &models.Post{
		ID: "p-shelved", CampaignID: "c-archived", Title: "t", MediaURLs: models.StringSlice{}, Status: models.PostStatusPublished,
		CTAType: models.CTATypeNone, CreatedBy: "user-1", CreatedAt: base, UpdatedAt: base,
	}
	if _, err := db.NewInsert().Model(shelved).Exec(tenantCtx()); err != nil {
		t.Fatalf("seed post: %v", err)
	}
	changed := base.Add(time.Minute)
	if _, err := db.NewUpdate().Model((*models.Campaign)(nil)).Set("posts_changed_at = ?", changed).
		Where("id = ?", "c-big").Exec(tenantCtx()); err != nil {
		t.Fatalf("set posts_changed_at: %v", err)
	}

	got, err := repo.GetCampaignPostTree(tenantCtx(), "c-big")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "c-big" || got.Status != models.StatusActive || got.Timezone != "Europe/Kyiv" ||
		got.PostsChangedAt == nil || !got.PostsChangedAt.Equal(changed) {
		t.Fatalf("campaign row = %+v", got)
	}
	if len(got.Posts) != n {
		t.Fatalf("posts = %d, want all %d", len(got.Posts), n)
	}
	if got.Posts[0].ID != fmt.Sprintf("p-%03d", n-1) || got.Posts[n-1].ID != "p-000" {
		t.Fatalf("posts not in scheduled order: first %s, last %s", got.Posts[0].ID, got.Posts[n-1].ID)
	}

	got, err = repo.GetCampaignPostTree(tenantCtx(), "c-archived")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != models.StatusArchived || len(got.Posts) != 1 || got.Posts[0].ID != "p-shelved" || got.PostsChangedAt != nil {
		t.Fatalf("archived campaign = %+v", got)
	}

	got, err = repo.GetCampaignPostTree(tenantCtx(), "c-empty")
	if err != nil {
		t.Fatal(err)
	}
	if got.Posts == nil || len(got.Posts) != 0 {
		t.Fatalf("empty campaign posts = %#v, want empty non-nil", got.Posts)
	}

	for _, id := range []string{"c-deleted", "c-theirs", "no-such-campaign"} {
		if _, err := repo.GetCampaignPostTree(tenantCtx(), id); !errors.Is(err, sql.ErrNoRows) {
			t.Errorf("%s: err = %v, want sql.ErrNoRows", id, err)
		}
	}
}
