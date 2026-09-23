package repository_test

import (
	"database/sql"
	"errors"
	"testing"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
)

func strptr(s string) *string { return &s }

// TestFlowModelConfigScopes covers the CON-308 assignment store: a global-default
// row and a per-tier override for the SAME (flow, slot) coexist (the COALESCE
// unique index treats NULL tier as a single distinct scope), each resolves via
// GetByScope, an absent scope yields sql.ErrNoRows, Upsert is idempotent per
// scope (updates in place, never a second row), and Delete reports whether a row
// existed.
func TestFlowModelConfigScopes(t *testing.T) {
	db := openMigratedDB(t)
	repo := repository.NewFlowModelConfigRepository(db)
	ctx := t.Context()

	// Global default + a "pro" tier override for the same slot.
	if err := repo.Upsert(ctx, &models.FlowModelConfig{FlowKey: "content_plan", SlotKey: "main", ModelID: "sonnet"}); err != nil {
		t.Fatalf("upsert global: %v", err)
	}
	if err := repo.Upsert(ctx, &models.FlowModelConfig{TierID: strptr("pro"), FlowKey: "content_plan", SlotKey: "main", ModelID: "opus"}); err != nil {
		t.Fatalf("upsert tier: %v", err)
	}

	all, err := repo.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("list len = %d, want 2 (global + tier coexist)", len(all))
	}

	// Global default resolves and is flagged as such.
	gd, err := repo.GetByScope(ctx, nil, "content_plan", "main")
	if err != nil {
		t.Fatalf("get global: %v", err)
	}
	if gd.ModelID != "sonnet" || !gd.IsGlobalDefault() {
		t.Fatalf("global = {model:%q global:%v}, want {sonnet true}", gd.ModelID, gd.IsGlobalDefault())
	}

	// Tier override resolves distinctly.
	ov, err := repo.GetByScope(ctx, strptr("pro"), "content_plan", "main")
	if err != nil {
		t.Fatalf("get tier: %v", err)
	}
	if ov.ModelID != "opus" || ov.IsGlobalDefault() {
		t.Fatalf("tier = {model:%q global:%v}, want {opus false}", ov.ModelID, ov.IsGlobalDefault())
	}

	// A tier with no override falls through (caller then uses the global default).
	if _, err := repo.GetByScope(ctx, strptr("free"), "content_plan", "main"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("absent scope err = %v, want sql.ErrNoRows", err)
	}

	// Upsert is idempotent per scope: re-writing the global updates it in place.
	if err := repo.Upsert(ctx, &models.FlowModelConfig{FlowKey: "content_plan", SlotKey: "main", ModelID: "haiku"}); err != nil {
		t.Fatalf("re-upsert global: %v", err)
	}
	if again, _ := repo.List(ctx); len(again) != 2 {
		t.Fatalf("list len after re-upsert = %d, want 2 (no dup)", len(again))
	}
	if gd2, _ := repo.GetByScope(ctx, nil, "content_plan", "main"); gd2.ModelID != "haiku" {
		t.Fatalf("global model after re-upsert = %q, want haiku", gd2.ModelID)
	}

	// Delete removes only the addressed scope and reports existence.
	deleted, err := repo.Delete(ctx, strptr("pro"), "content_plan", "main")
	if err != nil || !deleted {
		t.Fatalf("delete tier = (%v, %v), want (true, nil)", deleted, err)
	}
	if _, err := repo.GetByScope(ctx, strptr("pro"), "content_plan", "main"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("tier still present after delete: %v", err)
	}
	if gd3, err := repo.GetByScope(ctx, nil, "content_plan", "main"); err != nil || gd3.ModelID != "haiku" {
		t.Fatalf("global gone after tier delete: (%v, %v)", gd3, err)
	}
	if deleted, _ := repo.Delete(ctx, strptr("pro"), "content_plan", "main"); deleted {
		t.Fatal("second delete reported a row, want false")
	}
}
