package repository_test

import (
	"slices"
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/pgtest"
)

// The offers waiting on a source are the scope's confirmed with one of the
// options asked for since then, oldest answer first, until another offer
// replaces them; an option's input round-trips.
func TestAssistantProposals_ListAwaiting(t *testing.T) {
	db := pgtest.MustDB()
	t.Cleanup(func() { _ = db.Close() })
	repo := repository.NewAssistantProposalRepository(db)
	ctx := tenantCtx()
	now := time.Now().UTC().Truncate(time.Millisecond)
	since := now.Add(-time.Hour)

	answered := func(scope, chosen string, at time.Time) *models.AssistantProposal {
		p := newProposal(t, scope, "generatePosts", now.Add(-2*time.Hour), now.Add(time.Hour))
		p.Options = []models.ProposalOption{{ID: "import_page", Title: "Import a page", Tool: "importPage",
			Input: &models.ProposalOptionInput{Kind: "url", Label: "Page link", Arg: "url"}}}
		if err := repo.Create(ctx, p); err != nil {
			t.Fatal(err)
		}
		if chosen != "" {
			if ok, err := repo.Resolve(ctx, p.ID, models.ProposalConfirmed, chosen, "user-1", at); err != nil || !ok {
				t.Fatalf("resolve: %v, %t", err, ok)
			}
		}
		return p
	}
	upload := answered("camp-1", "add_document", now.Add(-time.Minute))
	page := answered("camp-1", "import_page", now.Add(-2*time.Minute))
	answered("camp-1", "exploratory", now.Add(-time.Minute))
	answered("camp-1", "", now)
	answered("camp-2", "add_document", now.Add(-time.Minute))
	chosen := []string{"add_document", "import_page"}

	got, err := repo.ListAwaiting(ctx, models.AssistantScopeCampaign, "camp-1", chosen, since)
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, len(got))
	for i, p := range got {
		ids[i] = p.ID
	}
	// import_page was answered first.
	if !slices.Equal(ids, []string{page.ID, upload.ID}) {
		t.Fatalf("awaiting = %v, want [%s %s]", ids, page.ID, upload.ID)
	}
	if in := got[0].Options[0].Input; in == nil || in.Arg != "url" || in.Kind != "url" {
		t.Fatalf("input = %+v", in)
	}
	if later, err := repo.ListAwaiting(ctx, models.AssistantScopeCampaign, "camp-1", chosen, now); err != nil || len(later) != 0 {
		t.Fatalf("answered before since = %+v, %v", later, err)
	}

	// Once offered again, the request no longer waits.
	fresh := newProposal(t, "camp-1", "generatePosts", now, now.Add(time.Hour))
	fresh.Replaces = upload.ID
	if err := repo.Create(ctx, fresh); err != nil {
		t.Fatal(err)
	}
	got, err = repo.ListAwaiting(ctx, models.AssistantScopeCampaign, "camp-1", chosen, since)
	if err != nil || len(got) != 1 || got[0].ID != page.ID {
		t.Fatalf("after the re-offer = %+v, %v", got, err)
	}
}
