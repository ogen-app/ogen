package repository_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
	"github.com/ogen-app/ogen/src/pgtest"
)

func message(t *testing.T, kind models.AssistantScopeKind, scopeID, role, content string, at time.Time, view models.JSONMap) *models.AssistantMessage {
	t.Helper()
	id, err := models.NewID()
	if err != nil {
		t.Fatal(err)
	}
	return &models.AssistantMessage{ID: id, ScopeKind: kind, ScopeID: scopeID, Role: role, Content: content, ViewContext: view, CreatedAt: at}
}

func TestAssistantMessages_ThreadPerScope(t *testing.T) {
	db := pgtest.MustDB()
	t.Cleanup(func() { _ = db.Close() })
	repo := repository.NewAssistantMessageRepository(db)
	ctx := tenantCtx()
	t0 := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	view := models.JSONMap{"version": float64(1), "screen": "ideas", "selection": map[string]any{"kind": "idea", "ids": []any{"i1", "i2"}}}

	turn1 := []*models.AssistantMessage{
		message(t, models.AssistantScopeWorkspace, "", "user", "what do these share?", t0, view),
		message(t, models.AssistantScopeWorkspace, "", "model", `{"explanation":"..."}`, t0.Add(time.Second), nil),
	}
	other := []*models.AssistantMessage{
		message(t, models.AssistantScopeCampaign, "c1", "user", "plan it", t0, nil),
	}
	turn2 := []*models.AssistantMessage{
		message(t, models.AssistantScopeWorkspace, "", "user", "draft them", t0.Add(time.Minute), nil),
	}
	for _, b := range [][]*models.AssistantMessage{turn1, other, turn2} {
		if err := repo.CreateBatch(ctx, b); err != nil {
			t.Fatal(err)
		}
	}

	got, err := repo.ListRecent(ctx, models.AssistantScopeWorkspace, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].Content != "what do these share?" || got[2].Content != "draft them" {
		t.Fatalf("workspace thread = %+v", got)
	}
	if got[0].ViewContext["screen"] != "ideas" || got[1].ViewContext != nil {
		t.Fatalf("view contexts = %v / %v", got[0].ViewContext, got[1].ViewContext)
	}
	// The limit keeps the most recent messages, still oldest first.
	got, err = repo.ListRecent(ctx, models.AssistantScopeWorkspace, "", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Role != "model" || got[1].Content != "draft them" {
		t.Fatalf("last two = %+v", got)
	}

	// Another tenant sees nothing.
	other2 := tenantctx.With(context.Background(), "tenant-other")
	if got, err := repo.ListRecent(other2, models.AssistantScopeWorkspace, "", 10); err != nil || len(got) != 0 {
		t.Fatalf("another tenant read %d messages (err %v)", len(got), err)
	}
}

func TestAssistantMessages_TiedTimestampsKeepBatchOrder(t *testing.T) {
	db := pgtest.MustDB()
	t.Cleanup(func() { _ = db.Close() })
	repo := repository.NewAssistantMessageRepository(db)
	ctx := tenantCtx()
	at := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	scopeID, err := models.NewID()
	if err != nil {
		t.Fatal(err)
	}

	// Same created_at, and ids that sort against batch order, so only the
	// insertion order can put the user turn first.
	user := message(t, models.AssistantScopeCampaign, scopeID, "user", "plan it", at, nil)
	user.ID = "z-" + user.ID
	model := message(t, models.AssistantScopeCampaign, scopeID, "model", "planned", at, nil)
	model.ID = "a-" + model.ID
	if err := repo.CreateBatch(ctx, []*models.AssistantMessage{user, model}); err != nil {
		t.Fatal(err)
	}

	got, err := repo.ListRecent(ctx, models.AssistantScopeCampaign, scopeID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Role != "user" || got[1].Role != "model" {
		t.Fatalf("thread = %+v", got)
	}
	got, err = repo.ListRecent(ctx, models.AssistantScopeCampaign, scopeID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Role != "model" {
		t.Fatalf("last one = %+v", got)
	}
	got, err = repo.ListRecent(ctx, models.AssistantScopeCampaign, scopeID, 0)
	if err != nil || len(got) != 0 {
		t.Fatalf("zero limit returned %d messages (err %v)", len(got), err)
	}
}

// Deleting a post deletes its assistant thread and offers, and a turn that
// finishes after the delete stores nothing for it.
func TestAssistantMessages_PostThreadGoesWithThePost(t *testing.T) {
	db := openMigratedDB(t)
	ctx := tenantCtx()
	msgs := repository.NewAssistantMessageRepository(db)
	posts := repository.NewPostRepository(db)
	seedPost(t, db, "post-thread", "", "", time.Now().UTC())
	t0 := time.Now().UTC()
	if err := msgs.CreateBatch(ctx, []*models.AssistantMessage{message(t, models.AssistantScopePost, "post-thread", "user", "shorten it", t0, nil)}); err != nil {
		t.Fatal(err)
	}
	offer := &models.AssistantProposal{ID: "offer-thread", ScopeKind: models.AssistantScopePost, ScopeID: "post-thread", Tool: "schedulePost", Args: models.JSONMap{}, Summary: "Schedule it", ExpiresAt: t0.Add(time.Hour)}
	if err := repository.NewAssistantProposalRepository(db).Create(ctx, offer); err != nil {
		t.Fatal(err)
	}
	if deleted, err := posts.Delete(ctx, "post-thread"); err != nil || !deleted {
		t.Fatalf("delete = %v, %v", deleted, err)
	}
	if got, err := msgs.ListRecent(ctx, models.AssistantScopePost, "post-thread", 10); err != nil || len(got) != 0 {
		t.Fatalf("thread after delete = %+v (err %v)", got, err)
	}
	var offers int
	if err := db.NewRaw("SELECT count(*) FROM assistant_proposals WHERE scope_id = ?", "post-thread").Scan(ctx, &offers); err != nil || offers != 0 {
		t.Fatalf("offers after delete = %d (err %v)", offers, err)
	}
	if err := msgs.CreateBatch(ctx, []*models.AssistantMessage{message(t, models.AssistantScopePost, "post-thread", "model", "{}", t0.Add(time.Second), nil)}); err != nil {
		t.Fatal(err)
	}
	if got, err := msgs.ListRecent(ctx, models.AssistantScopePost, "post-thread", 10); err != nil || len(got) != 0 {
		t.Fatalf("a late turn recreated the thread: %+v (err %v)", got, err)
	}
}

// threadPost seeds a post in campaignID for a post thread to belong to.
func threadPost(t *testing.T, db *bun.DB, id, campaignID, title string) {
	t.Helper()
	now := time.Now().UTC()
	post := &models.Post{
		ID:           id,
		CampaignID:   campaignID,
		Title:        title,
		Content:      "content " + id,
		MediaURLs:    models.StringSlice{},
		UsedAssetIDs: models.StringSlice{},
		Status:       models.PostStatusDraft,
		CTAType:      models.CTATypeNone,
		CreatedBy:    "user-1",
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	if _, err := db.NewInsert().Model(post).Exec(tenantCtx()); err != nil {
		t.Fatalf("seed post %s: %v", id, err)
	}
}

func TestAssistantMessages_ListThreads(t *testing.T) {
	db := openMigratedDB(t)
	ctx := tenantCtx()
	msgs := repository.NewAssistantMessageRepository(db)
	campaigns := repository.NewCampaignRepository(db, nil, nil, nil)
	live := seedCampaign(t, campaigns, ctx, "Spring launch")
	archived := seedCampaign(t, campaigns, ctx, "Old promo")
	deleted := seedCampaign(t, campaigns, ctx, "Scrapped")
	threadPost(t, db, "post-live", live, "Teaser")
	threadPost(t, db, "post-in-deleted", deleted, "Lost")

	t0 := time.Date(2026, 11, 9, 9, 0, 0, 0, time.UTC)
	for _, b := range [][]*models.AssistantMessage{
		{
			message(t, models.AssistantScopeWorkspace, "", "user", "what's working?", t0, nil),
			message(t, models.AssistantScopeWorkspace, "", "model", `{"explanation":"Reels."}`, t0.Add(time.Second), nil),
		},
		{message(t, models.AssistantScopeCampaign, live, "user", "plan it", t0.Add(2*time.Minute), nil)},
		{message(t, models.AssistantScopeCampaign, archived, "user", "wrap it up", t0.Add(3*time.Minute), nil)},
		{message(t, models.AssistantScopeCampaign, deleted, "user", "start over", t0.Add(4*time.Minute), nil)},
		{message(t, models.AssistantScopePost, "post-live", "user", "shorten it", t0.Add(5*time.Minute), nil)},
		{message(t, models.AssistantScopePost, "post-in-deleted", "user", "fix it", t0.Add(6*time.Minute), nil)},
	} {
		if err := msgs.CreateBatch(ctx, b); err != nil {
			t.Fatal(err)
		}
	}
	// A post thread whose post is gone, written past CreateBatch's check.
	if _, err := db.NewInsert().Model(message(t, models.AssistantScopePost, "post-gone", "user", "hello?", t0.Add(7*time.Minute), nil)).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	// Another tenant's thread, newer than all of them.
	other := tenantctx.With(context.Background(), "tenant-other")
	if err := msgs.CreateBatch(other, []*models.AssistantMessage{message(t, models.AssistantScopeWorkspace, "", "user", "not yours", t0.Add(8*time.Minute), nil)}); err != nil {
		t.Fatal(err)
	}
	if ok, err := campaigns.Archive(ctx, archived); err != nil || !ok {
		t.Fatalf("archive = %v, %v", ok, err)
	}
	if ok, err := campaigns.Delete(ctx, deleted); err != nil || !ok {
		t.Fatalf("delete = %v, %v", ok, err)
	}

	got, err := msgs.ListThreads(ctx, time.Time{}, 10)
	if err != nil {
		t.Fatal(err)
	}
	want := []repository.AssistantThread{
		{ScopeKind: models.AssistantScopePost, ScopeID: "post-live", Title: "Teaser", CampaignID: live, CampaignTitle: "Spring launch", LastMessageAt: t0.Add(5 * time.Minute), LastRole: "user", LastContent: "shorten it", MessageCount: 1},
		{ScopeKind: models.AssistantScopeCampaign, ScopeID: archived, Title: "Old promo", CampaignID: archived, CampaignTitle: "Old promo", Archived: true, LastMessageAt: t0.Add(3 * time.Minute), LastRole: "user", LastContent: "wrap it up", MessageCount: 1},
		{ScopeKind: models.AssistantScopeCampaign, ScopeID: live, Title: "Spring launch", CampaignID: live, CampaignTitle: "Spring launch", LastMessageAt: t0.Add(2 * time.Minute), LastRole: "user", LastContent: "plan it", MessageCount: 1},
		{ScopeKind: models.AssistantScopeWorkspace, LastMessageAt: t0.Add(time.Second), LastRole: "model", LastContent: `{"explanation":"Reels."}`, MessageCount: 2},
	}
	if len(got) != len(want) {
		t.Fatalf("threads = %+v; want %d", got, len(want))
	}
	for i := range want {
		got[i].LastMessageAt = got[i].LastMessageAt.UTC()
		if got[i] != want[i] {
			t.Fatalf("thread %d = %+v; want %+v", i, got[i], want[i])
		}
	}

	// A page, and the next one from its last row.
	page, err := msgs.ListThreads(ctx, time.Time{}, 2)
	if err != nil || len(page) != 2 || page[1].ScopeID != archived {
		t.Fatalf("first page = %+v (err %v)", page, err)
	}
	next, err := msgs.ListThreads(ctx, page[1].LastMessageAt, 2)
	if err != nil || len(next) != 2 || next[0].ScopeID != live || next[1].ScopeKind != models.AssistantScopeWorkspace {
		t.Fatalf("next page = %+v (err %v)", next, err)
	}

	if got, err := msgs.ListThreads(ctx, time.Time{}, 0); err != nil || got != nil {
		t.Fatalf("zero limit = %+v (err %v)", got, err)
	}
	if _, err := msgs.ListThreads(context.Background(), time.Time{}, 10); !errors.Is(err, tenantctx.ErrNoTenant) {
		t.Fatalf("no tenant: err = %v; want ErrNoTenant", err)
	}
}
