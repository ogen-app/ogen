package repository_test

import (
	"slices"
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
)

// SetTriageIf writes only while the stored verdict, reminder and decision are
// the expected ones, NULLs included.
func TestIdeas_SetTriageIf(t *testing.T) {
	db := openMigratedDB(t)
	repo := repository.NewIdeaRepository(db)
	ctx := tenantctx.With(t.Context(), "tenant-triage-if")

	decided := time.Now().UTC().Truncate(time.Microsecond)
	remind := decided.Add(72 * time.Hour)
	stored := &models.Idea{ID: "cas-1", Title: "t", Verdict: new(models.IdeaVerdictLater), RemindAt: &remind, DecidedAt: &decided, DecidedBy: new("u1")}
	if err := repo.Create(ctx, stored); err != nil {
		t.Fatal(err)
	}
	inbox := &models.Idea{ID: "cas-1", UpdatedAt: time.Now().UTC()}

	// A stale expectation (another verdict, or another decider) matches nothing.
	for _, was := range []*models.Idea{
		{Verdict: new(models.IdeaVerdictYes), RemindAt: &remind, DecidedAt: &decided, DecidedBy: new("u1")},
		{Verdict: new(models.IdeaVerdictLater), DecidedAt: &decided, DecidedBy: new("u1")},
		{Verdict: new(models.IdeaVerdictLater), RemindAt: &remind, DecidedAt: &decided, DecidedBy: new("u2")},
	} {
		ok, err := repo.SetTriageIf(ctx, nil, inbox, was)
		if err != nil || ok {
			t.Fatalf("stale %+v: ok = %v, err = %v", was, ok, err)
		}
	}
	ok, err := repo.SetTriageIf(ctx, nil, inbox, stored)
	if err != nil || !ok {
		t.Fatalf("matching: ok = %v, err = %v", ok, err)
	}
	got, err := repo.GetByID(ctx, "cas-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Verdict != nil || got.RemindAt != nil || got.DecidedAt != nil || got.DecidedBy != nil {
		t.Fatalf("idea = %+v; want back in the inbox", got)
	}
	// All NULL now, so a NULL expectation matches.
	back := &models.Idea{ID: "cas-1", Verdict: stored.Verdict, RemindAt: &remind, DecidedAt: &decided, DecidedBy: new("u1"), UpdatedAt: time.Now().UTC()}
	if ok, err := repo.SetTriageIf(ctx, nil, back, &models.Idea{}); err != nil || !ok {
		t.Fatalf("from inbox: ok = %v, err = %v", ok, err)
	}
	if got, _ = repo.GetByID(ctx, "cas-1"); got.Verdict == nil || *got.Verdict != models.IdeaVerdictLater || !got.RemindAt.Equal(remind) {
		t.Fatalf("idea = %+v; want later again", got)
	}
}

// SetAssetIDsIf writes only asset_ids and use_assets, and only while both
// hold the expected values.
func TestCampaigns_SetAssetIDsIf(t *testing.T) {
	db := openMigratedDB(t)
	ctx := tenantCtx()
	repo := repository.NewCampaignRepository(db, nil, nil, nil)
	id := seedCampaign(t, repo, ctx, "Membership")
	// A brief edit made since the membership was read survives the restore.
	if _, err := db.NewUpdate().Model((*models.Campaign)(nil)).
		Set("asset_ids = ?::jsonb", `["a1","a2"]`).Set("use_assets = true").Set("description = ?", "edited").
		Where("id = ?", id).Exec(ctx); err != nil {
		t.Fatal(err)
	}

	for _, stale := range []struct {
		ids []string
		use bool
	}{{[]string{"a2", "a1"}, true}, {[]string{"a1"}, true}, {[]string{"a1", "a2"}, false}} {
		ok, err := repo.SetAssetIDsIf(ctx, nil, id, stale.ids, stale.use, nil, false)
		if err != nil || ok {
			t.Fatalf("stale %v/%v: ok = %v, err = %v", stale.ids, stale.use, ok, err)
		}
	}
	ok, err := repo.SetAssetIDsIf(ctx, nil, id, []string{"a1", "a2"}, true, nil, false)
	if err != nil || !ok {
		t.Fatalf("matching: ok = %v, err = %v", ok, err)
	}
	c := new(models.Campaign)
	if err := db.NewSelect().Model(c).Where("c.id = ?", id).Scan(ctx); err != nil {
		t.Fatal(err)
	}
	if len(c.AssetIDs) != 0 || c.UseAssets || c.Description != "edited" {
		t.Fatalf("campaign assets = %v, use = %v, description = %q", c.AssetIDs, c.UseAssets, c.Description)
	}
	if ok, err := repo.SetAssetIDsIf(ctx, nil, id, nil, false, []string{"a3"}, true); err != nil || !ok {
		t.Fatalf("from empty: ok = %v, err = %v", ok, err)
	}
	if err := db.NewSelect().Model(c).Where("c.id = ?", id).Scan(ctx); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(c.AssetIDs, models.StringSlice{"a3"}) || !c.UseAssets {
		t.Fatalf("campaign assets = %v, use = %v", c.AssetIDs, c.UseAssets)
	}
}
