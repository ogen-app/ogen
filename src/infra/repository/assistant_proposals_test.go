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

func newProposal(t *testing.T, scopeID, tool string, created, expires time.Time) *models.AssistantProposal {
	t.Helper()
	id, err := models.NewID()
	if err != nil {
		t.Fatal(err)
	}
	return &models.AssistantProposal{
		ID: id, ScopeKind: models.AssistantScopeCampaign, ScopeID: scopeID, Tool: tool,
		Args:      models.JSONMap{"count": float64(5)},
		Summary:   "Draft 5 posts",
		Options:   []models.ProposalOption{{ID: "fill", Title: "Fill the goal", Tool: "generatePosts", Args: models.JSONMap{"count": float64(2)}}},
		CreatedBy: "user-1", CreatedAt: created, ExpiresAt: expires,
	}
}

func TestAssistantProposals_RoundTripAndListOpen(t *testing.T) {
	db := pgtest.MustDB()
	t.Cleanup(func() { _ = db.Close() })
	repo := repository.NewAssistantProposalRepository(db)
	ctx := tenantCtx()
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)

	older := newProposal(t, "camp-1", "redistributePosts", now.Add(-2*time.Minute), now.Add(time.Hour))
	newer := newProposal(t, "camp-1", "generatePosts", now.Add(-time.Minute), now.Add(time.Hour))
	expired := newProposal(t, "camp-1", "runContentPlan", now.Add(-3*time.Hour), now.Add(-time.Hour))
	elsewhere := newProposal(t, "camp-2", "generatePosts", now, now.Add(time.Hour))
	for _, p := range []*models.AssistantProposal{older, newer, expired, elsewhere} {
		if err := repo.Create(ctx, p); err != nil {
			t.Fatalf("create %s: %v", p.Tool, err)
		}
	}

	got, err := repo.GetByID(ctx, newer.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != models.ProposalPending || got.Args["count"] != float64(5) {
		t.Fatalf("stored proposal = %+v", got)
	}
	if o, ok := got.Option("fill"); !ok || o.Tool != "generatePosts" || o.Args["count"] != float64(2) {
		t.Fatalf("option round-trip = %+v, %t", o, ok)
	}

	open, err := repo.ListOpen(ctx, models.AssistantScopeCampaign, "camp-1", now)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 2 || open[0].ID != newer.ID || open[1].ID != older.ID {
		t.Fatalf("open = %v, want [newer older]", proposalIDs(open))
	}
}

// A button click and a typed "yes" may race; only one resolves the proposal.
// An expired proposal cannot be resolved at all.
func TestAssistantProposals_ResolveOnce(t *testing.T) {
	db := pgtest.MustDB()
	t.Cleanup(func() { _ = db.Close() })
	repo := repository.NewAssistantProposalRepository(db)
	ctx := tenantCtx()
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)

	p := newProposal(t, "camp-1", "generatePosts", now, now.Add(time.Hour))
	gone := newProposal(t, "camp-1", "generatePosts", now.Add(-2*time.Hour), now.Add(-time.Hour))
	for _, x := range []*models.AssistantProposal{p, gone} {
		if err := repo.Create(ctx, x); err != nil {
			t.Fatal(err)
		}
	}

	if ok, err := repo.Resolve(ctx, p.ID, models.ProposalConfirmed, "fill", "user-1", now); err != nil || !ok {
		t.Fatalf("first resolve = %t, %v", ok, err)
	}
	if ok, err := repo.Resolve(ctx, p.ID, models.ProposalDismissed, "", "user-2", now); err != nil || ok {
		t.Fatalf("second resolve = %t, %v, want false", ok, err)
	}
	got, err := repo.GetByID(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != models.ProposalConfirmed || got.Chosen != "fill" || got.ResolvedBy != "user-1" || got.ResolvedAt == nil {
		t.Fatalf("resolved proposal = %+v", got)
	}
	if ok, err := repo.Resolve(ctx, gone.ID, models.ProposalConfirmed, "", "user-1", now); err != nil || ok {
		t.Fatalf("resolve expired = %t, %v, want false", ok, err)
	}
}

// Proposals are tenant-scoped: another tenant can neither read nor resolve them.
func TestAssistantProposals_TenantScoped(t *testing.T) {
	db := pgtest.MustDB()
	t.Cleanup(func() { _ = db.Close() })
	repo := repository.NewAssistantProposalRepository(db)
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)

	p := newProposal(t, "camp-1", "generatePosts", now, now.Add(time.Hour))
	if err := repo.Create(tenantCtx(), p); err != nil {
		t.Fatal(err)
	}
	other := tenantctx.With(context.Background(), "tenant-other")
	if _, err := repo.GetByID(other, p.ID); err == nil {
		t.Fatal("another tenant read the proposal")
	}
	if ok, err := repo.Resolve(other, p.ID, models.ProposalConfirmed, "", "intruder", now); err != nil || ok {
		t.Fatalf("another tenant resolved the proposal: %t, %v", ok, err)
	}
}

func proposalIDs(ps []models.AssistantProposal) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.ID
	}
	return out
}
