package packs

import (
	"context"
	"slices"
	"testing"

	"github.com/firebase/genkit/go/ai"
	"github.com/firebase/genkit/go/genkit"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/assistantkit"
	"github.com/ogen-app/ogen/src/infra/eventhub"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/usecase/ideas"
)

func (r *memIdeaRepo) OpenIdeas(_ context.Context, campaignID string, limit int) ([]models.Idea, bool, error) {
	var out []models.Idea
	for _, i := range r.byID {
		if (campaignID == "" && i.CampaignID == nil) || (i.CampaignID != nil && *i.CampaignID == campaignID) {
			out = append(out, *i)
		}
	}
	slices.SortFunc(out, func(a, b models.Idea) int { return a.CreatedAt.Compare(b.CreatedAt) })
	return out[:min(len(out), limit)], len(out) > limit, nil
}

func (r *memIdeaRepo) MergeSet(_ context.Context, ids []string) (repository.IdeaMergeSet, error) {
	var set repository.IdeaMergeSet
	for _, id := range ids {
		if i, ok := r.byID[id]; ok {
			set.Ideas = append(set.Ideas, *i)
		}
	}
	return set, nil
}

func (r *memIdeaRepo) MergeIdeas(ctx context.Context, ids []string, plan func(repository.IdeaMergeSet) (repository.IdeaMerge, error)) (*models.Idea, error) {
	set, _ := r.MergeSet(ctx, ids)
	m, err := plan(set)
	if err != nil {
		return nil, err
	}
	r.byID[m.Keep.ID] = m.Keep
	for _, id := range m.Delete {
		delete(r.byID, id)
	}
	return m.Keep, nil
}

func duplicateDeps() (Deps, *memIdeaRepo) {
	d, repo, _ := ideaDeps()
	c1 := "c1"
	repo.byID["i3"] = &models.Idea{ID: "i3", Title: "Pricing myths!", AssetIDs: []string{"a1"}}
	repo.byID["i4"] = &models.Idea{ID: "i4", Title: "Launch recap", CampaignID: &c1}
	repo.byID["i5"] = &models.Idea{ID: "i5", Title: "Recap of the launch", CampaignID: &c1}
	return d, repo
}

func TestFindDuplicateIdeas_ThreadScope(t *testing.T) {
	d, _ := duplicateDeps()
	ctx, _ := turn(t, workspace, nil)
	out, err := findDuplicateIdeas(ctx, d, FindDuplicateIdeasInput{})
	if err != nil || out.Total != 1 || out.Groups[0].KeepID != "i3" || len(out.Groups[0].Ideas) != 2 {
		t.Fatalf("workspace = %+v, %v; want the two pricing ideas, the one with assets kept", out, err)
	}
	ctx, _ = turn(t, campaign1, nil)
	out, err = findDuplicateIdeas(ctx, d, FindDuplicateIdeasInput{Limit: 5})
	if err != nil || out.Total != 1 || out.Groups[0].Ideas[0].CampaignID != "c1" {
		t.Fatalf("campaign = %+v, %v", out, err)
	}
}

type recordingHub struct{ got []eventhub.Event }

func (h *recordingHub) Publish(_ context.Context, ev eventhub.Event) error {
	h.got = append(h.got, ev)
	return nil
}

func (h *recordingHub) Subscribe(context.Context, eventhub.SubscribeOpts) (<-chan eventhub.Event, func(), error) {
	return nil, func() {}, nil
}

func TestMergeIdeas(t *testing.T) {
	d, repo := duplicateDeps()
	hub := &recordingHub{}
	d.Hub = hub
	ctx, touched := turn(t, workspace, nil)
	_, err := mergeIdeas(ctx, d, MergeIdeasInput{KeepID: "i3", MergeIDs: []string{"i4"}})
	refused(t, err)
	_, err = mergeIdeas(ctx, d, MergeIdeasInput{KeepID: "i3", MergeIDs: []string{"gone"}})
	refused(t, err)
	_, err = mergeIdeas(ctx, d, MergeIdeasInput{KeepID: "i3"})
	refused(t, err)

	out, err := mergeIdeas(ctx, d, MergeIdeasInput{KeepID: "i3", MergeIDs: []string{"i1"}})
	if err != nil || out.Idea.ID != "i3" || !slices.Equal(out.Deleted, []string{"i1"}) || repo.byID["i1"] != nil {
		t.Fatalf("merge = %+v, %v", out, err)
	}
	if !slices.Equal(touched(), []assistantkit.Touched{{Kind: "idea"}}) {
		t.Fatalf("touched = %v", touched())
	}
	// Other sessions hear of the deleted idea, as after a merge over HTTP.
	if len(hub.got) != 1 || hub.got[0].Type != ideas.EventDeleted || hub.got[0].TenantID != "tenant-1" ||
		!slices.Equal(hub.got[0].Payload.(map[string][]string)["ids"], []string{"i1"}) {
		t.Fatalf("events = %+v", hub.got)
	}
}

type memProposals struct{ got []assistantkit.Proposal }

func (s *memProposals) Propose(_ context.Context, p assistantkit.Proposal) (string, error) {
	s.got = append(s.got, p)
	return "prop-1", nil
}

// A merge always waits for a yes, named after the idea it keeps; one that
// could never run is refused before it is offered.
func TestMergeIdeas_AlwaysProposed(t *testing.T) {
	d, repo := duplicateDeps()
	specs := defineMergeTools(genkit.Init(t.Context()), d)
	tool := specs[1].Tool.(ai.Tool)
	if specs[1].Tier != assistantkit.TierProposal || tool.Name() != "mergeIdeas" {
		t.Fatalf("spec = %+v", specs[1])
	}
	sink := &memProposals{}
	ctx, _ := turn(t, workspace, nil)
	ctx = assistantkit.WithProposalSink(ctx, sink)

	raw, err := tool.RunRaw(ctx, map[string]any{"keepId": "i3", "mergeIds": []any{"i1"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(sink.got) != 1 || sink.got[0].Summary != "Merge 2 ideas into “Pricing myths!”" || repo.byID["i1"] == nil {
		t.Fatalf("raw = %v, proposals = %+v; want one proposal and nothing merged", raw, sink.got)
	}
	if _, err := tool.RunRaw(ctx, map[string]any{"keepId": "i3", "mergeIds": []any{"i4"}}); err != nil || len(sink.got) != 1 {
		t.Fatalf("a cross-campaign merge was offered: %v, %d", err, len(sink.got))
	}
}
