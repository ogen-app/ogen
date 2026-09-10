package repository_test

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
)

// seedCampaignMessage inserts one message and returns its id. FK enforcement is
// disabled by openMigratedDB, so a synthetic campaign_id needs no campaign row.
func seedCampaignMessage(t *testing.T, repo repository.CampaignAssistantMessageRepository, ctx context.Context, campaignID, role, content string, createdAt time.Time) string {
	t.Helper()
	id, err := models.NewID()
	if err != nil {
		t.Fatalf("mint id: %v", err)
	}
	if err := repo.Create(ctx, &models.CampaignAssistantMessage{
		ID:         id,
		CampaignID: campaignID,
		Role:       role,
		Content:    content,
		CreatedAt:  createdAt,
	}); err != nil {
		t.Fatalf("create message: %v", err)
	}
	return id
}

// TestCampaignAssistantMessagesChronological verifies ListRecentByCampaignID
// returns a campaign's turns oldest-first (the repo queries DESC then reverses)
// and scopes to the requested campaign.
func TestCampaignAssistantMessagesChronological(t *testing.T) {
	db := openMigratedDB(t)
	ctx := tenantCtx()
	repo := repository.NewCampaignAssistantMessageRepository(db)

	base := time.Now().UTC().Truncate(time.Second)
	seedCampaignMessage(t, repo, ctx, "camp-1", "user", "first", base)
	seedCampaignMessage(t, repo, ctx, "camp-1", "model", "second", base.Add(time.Second))
	seedCampaignMessage(t, repo, ctx, "camp-1", "user", "third", base.Add(2*time.Second))
	// A different campaign's message must not leak in.
	seedCampaignMessage(t, repo, ctx, "camp-2", "user", "other", base.Add(3*time.Second))

	msgs, err := repo.ListRecentByCampaignID(ctx, "camp-1", 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(msgs) != 3 {
		t.Fatalf("want 3 messages for camp-1, got %d", len(msgs))
	}
	want := []string{"first", "second", "third"}
	for i, w := range want {
		if msgs[i].Content != w {
			t.Fatalf("msg[%d].Content = %q, want %q", i, msgs[i].Content, w)
		}
	}
	if msgs[0].Role != "user" || msgs[1].Role != "model" {
		t.Fatalf("roles not preserved: %q, %q", msgs[0].Role, msgs[1].Role)
	}
}

// TestCampaignAssistantMessagesLimit verifies the limit returns the most-recent
// N turns, still oldest-first.
func TestCampaignAssistantMessagesLimit(t *testing.T) {
	db := openMigratedDB(t)
	ctx := tenantCtx()
	repo := repository.NewCampaignAssistantMessageRepository(db)

	base := time.Now().UTC().Truncate(time.Second)
	for i := range 5 {
		seedCampaignMessage(t, repo, ctx, "camp-lim", "user", "m"+strconv.Itoa(i), base.Add(time.Duration(i)*time.Second))
	}

	msgs, err := repo.ListRecentByCampaignID(ctx, "camp-lim", 2)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("want 2, got %d", len(msgs))
	}
	// The two most-recent (m3, m4), returned oldest-first.
	if msgs[0].Content != "m3" || msgs[1].Content != "m4" {
		t.Fatalf("limit window wrong: %q, %q", msgs[0].Content, msgs[1].Content)
	}
}

// TestCampaignAssistantMessagesCreateBatch verifies both turns of a conversation
// are written atomically by CreateBatch and are tenant-stamped from context.
func TestCampaignAssistantMessagesCreateBatch(t *testing.T) {
	db := openMigratedDB(t)
	ctx := tenantCtx()
	repo := repository.NewCampaignAssistantMessageRepository(db)

	uID, err := models.NewID()
	if err != nil {
		t.Fatalf("mint id: %v", err)
	}
	mID, err := models.NewID()
	if err != nil {
		t.Fatalf("mint id: %v", err)
	}
	base := time.Now().UTC().Truncate(time.Second)
	if err := repo.CreateBatch(ctx, []*models.CampaignAssistantMessage{
		{ID: uID, CampaignID: "camp-batch", Role: "user", Content: "hi", CreatedAt: base},
		{ID: mID, CampaignID: "camp-batch", Role: "model", Content: `{"action":"answered"}`, CreatedAt: base.Add(time.Second)},
	}); err != nil {
		t.Fatalf("create batch: %v", err)
	}

	msgs, err := repo.ListRecentByCampaignID(ctx, "camp-batch", 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("want 2 messages, got %d", len(msgs))
	}
	if msgs[0].Role != "user" || msgs[1].Role != "model" {
		t.Fatalf("unexpected roles: %q, %q", msgs[0].Role, msgs[1].Role)
	}
}

// TestCampaignAssistantMessagesTenantIsolation verifies the TenantScoped hooks
// keep one tenant's conversation invisible to another, even on the same
// campaign id.
func TestCampaignAssistantMessagesTenantIsolation(t *testing.T) {
	db := openMigratedDB(t)
	repo := repository.NewCampaignAssistantMessageRepository(db)

	ctxA := tenantctx.With(context.Background(), "tenant-a")
	ctxB := tenantctx.With(context.Background(), "tenant-b")

	base := time.Now().UTC().Truncate(time.Second)
	seedCampaignMessage(t, repo, ctxA, "camp-shared", "user", "a-msg", base)
	seedCampaignMessage(t, repo, ctxB, "camp-shared", "user", "b-msg", base.Add(time.Second))

	aMsgs, err := repo.ListRecentByCampaignID(ctxA, "camp-shared", 10)
	if err != nil {
		t.Fatalf("list A: %v", err)
	}
	if len(aMsgs) != 1 || aMsgs[0].Content != "a-msg" {
		t.Fatalf("tenant A should see only its own message, got %+v", aMsgs)
	}

	bMsgs, err := repo.ListRecentByCampaignID(ctxB, "camp-shared", 10)
	if err != nil {
		t.Fatalf("list B: %v", err)
	}
	if len(bMsgs) != 1 || bMsgs[0].Content != "b-msg" {
		t.Fatalf("tenant B should see only its own message, got %+v", bMsgs)
	}
}
