package repository_test

import (
	"context"
	"testing"
	"time"

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
