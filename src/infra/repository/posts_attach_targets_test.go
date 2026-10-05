package repository_test

import (
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
)

func TestListAttachTargets(t *testing.T) {
	db := openMigratedDB(t)
	repo := repository.NewPostRepository(db)
	base := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)

	seedCampaign := func(tenantID, id, name string, deleted bool) {
		t.Helper()
		c := &models.Campaign{
			ID: id, Name: name, TargetPlatforms: models.CampaignPlatforms{}, PublishingDays: models.StringSlice{},
			TagIDs: models.StringSlice{}, AssetIDs: models.StringSlice{}, Status: models.StatusActive,
			CreatedBy: "user-1", CreatedAt: base, UpdatedAt: base,
		}
		if deleted {
			c.DeletedAt = &base
		}
		if _, err := db.NewInsert().Model(c).Exec(tenantctx.With(t.Context(), tenantID)); err != nil {
			t.Fatalf("seed campaign %s: %v", id, err)
		}
	}
	seedPost := func(tenantID, id, campaignID, title string, status models.PostStatus, age time.Duration) {
		t.Helper()
		p := &models.Post{
			ID: id, CampaignID: campaignID, Title: title, Content: "body", MediaURLs: models.StringSlice{},
			Status: status, CTAType: models.CTATypeNone, CreatedBy: "user-1",
			CreatedAt: base, UpdatedAt: base.Add(-age),
		}
		if _, err := db.NewInsert().Model(p).Exec(tenantctx.With(t.Context(), tenantID)); err != nil {
			t.Fatalf("seed post %s: %v", id, err)
		}
	}

	seedCampaign(models.DefaultTenantID, "camp-live", "Launch", false)
	seedCampaign(models.DefaultTenantID, "camp-gone", "Archived", true)
	seedCampaign("t-other", "camp-other", "Theirs", false)
	seedPost(models.DefaultTenantID, "p-draft", "camp-live", "Hero 100% draft", models.PostStatusDraft, time.Hour)
	seedPost(models.DefaultTenantID, "p-failed", "camp-live", "Retry me", models.PostStatusFailed, time.Minute)
	seedPost(models.DefaultTenantID, "p-ready", "camp-live", "Ready", models.PostStatusReadyForPublish, 2*time.Hour)
	seedPost(models.DefaultTenantID, "p-scheduled", "camp-live", "Queued", models.PostStatusScheduled, 0)
	seedPost(models.DefaultTenantID, "p-published", "camp-live", "Out", models.PostStatusPublished, 0)
	seedPost(models.DefaultTenantID, "p-orphan", "camp-gone", "Archived draft", models.PostStatusDraft, 0)
	seedPost("t-other", "p-theirs", "camp-other", "Hero theirs", models.PostStatusDraft, 0)
	if _, err := db.NewInsert().Model(&models.PostAttachment{ID: "att-1", PostID: "p-draft", MimeType: "image/png", S3Key: "k", CreatedBy: "user-1"}).
		Exec(tenantCtx()); err != nil {
		t.Fatalf("seed attachment: %v", err)
	}

	got, err := repo.ListAttachTargets(tenantCtx(), "", 0)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, p := range got {
		ids = append(ids, p.ID)
	}
	want := []string{"p-failed", "p-draft", "p-ready"}
	if len(ids) != len(want) {
		t.Fatalf("ids = %v, want %v (newest edit first, unsubmitted, live campaigns, own tenant)", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("ids = %v, want %v", ids, want)
		}
	}
	if d := got[1]; d.CampaignName != "Launch" || d.AttachmentCount != 1 || d.Title != "Hero 100% draft" {
		t.Fatalf("draft row = %+v", d)
	}

	// The query is a literal substring: % and _ don't act as wildcards.
	got, err = repo.ListAttachTargets(tenantCtx(), "100%", 10)
	if err != nil || len(got) != 1 || got[0].ID != "p-draft" {
		t.Fatalf("q=100%%: %+v err=%v", got, err)
	}
	got, err = repo.ListAttachTargets(tenantCtx(), "hero", 10)
	if err != nil || len(got) != 1 || got[0].ID != "p-draft" {
		t.Fatalf("q=hero must match case-insensitively and stay in the tenant: %+v err=%v", got, err)
	}
	if got, _ = repo.ListAttachTargets(tenantCtx(), "", 1); len(got) != 1 {
		t.Fatalf("limit 1 returned %d rows", len(got))
	}
}
