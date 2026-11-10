package packs

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/genkit/flows/assistant"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/assistantkit"
	"github.com/ogen-app/ogen/src/usecase/post_actions/compose"
)

// draftDeps is the ideas pack with a writer that writes one post per call,
// failing on the call failOn (0: never).
func draftDeps(failOn int) (Deps, *[]compose.DraftRequest) {
	d, repo, _ := ideaDeps()
	c1 := "c1"
	repo.byID["i1"].CampaignID, repo.byID["i1"].SourceAssetID = &c1, new("a1")
	repo.byID["i1"].Plan = models.IdeaPlan{PlatformID: new("linkedin")}
	repo.byID["i2"].CampaignID, repo.byID["i2"].Plan = &c1, models.IdeaPlan{PlatformID: new("linkedin")}
	camp := &models.Campaign{ID: "c1", TargetPlatforms: models.CampaignPlatforms{{ID: "linkedin"}}}
	var calls []compose.DraftRequest
	d.Compose = compose.New(nil, repo, &memCampaigns{c: camp}, memLinks{}, nil, nil).
		WithDrafter(func(_ context.Context, req compose.DraftRequest) ([]compose.Written, []string, error) {
			calls = append(calls, req)
			if len(calls) == failOn {
				return nil, nil, errors.New("writer down")
			}
			return []compose.Written{{PostID: "p" + string(rune('0'+len(calls))), Title: "Post", PlatformID: req.PlatformID}}, nil, nil
		})
	d.Assets = memAssets{all: []models.Asset{{ID: "a1", Title: "Brochure"}}}
	d.DraftMax = 5
	return d, &calls
}

// Each drafted post says what it is grounded in, in the result and in its
// streamed event: the brochure for the analysed idea, nothing for the one
// written by hand.
func TestDraftIdeas_Grounding(t *testing.T) {
	d, calls := draftDeps(0)
	var events []DraftedIdeaPost
	ctx, touched := turn(t, workspace, func(kind assistant.SSEEventKind, data any) {
		if kind == "idea_draft_post" {
			events = append(events, data.(DraftedIdeaPost))
		}
	})
	out, err := draftIdeas(ctx, d, DraftIdeasInput{IdeaIDs: []string{"i1", "i2"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(*calls) != 2 || len(out.Posts) != 2 || len(events) != 2 || len(touched()) != 2 {
		t.Fatalf("posts = %+v, events %d, calls %d", out.Posts, len(events), len(*calls))
	}
	first, second := out.Posts[0], out.Posts[1]
	if first.Grounding.Mode != assistantkit.Grounded || !slices.Equal(first.Grounding.Sources, []assistantkit.Source{{ID: "a1", Title: "Brochure"}}) ||
		!slices.Equal(first.IdeaIDs, []string{"i1"}) {
		t.Fatalf("first = %+v", first)
	}
	if second.Grounding.Mode != assistantkit.Exploratory || len(second.Grounding.Sources) != 0 || events[1].Grounding.Mode != assistantkit.Exploratory {
		t.Fatalf("second = %+v, event %+v", second, events[1])
	}
}

// A writer that fails part way keeps what it wrote, with a warning; one
// that wrote nothing fails.
func TestDraftIdeas_StoppedEarly(t *testing.T) {
	d, _ := draftDeps(2)
	ctx, _ := turn(t, workspace, nil)
	out, err := draftIdeas(ctx, d, DraftIdeasInput{IdeaIDs: []string{"i1", "i2"}})
	if err != nil || len(out.Posts) != 1 || len(out.Warnings) != 1 {
		t.Fatalf("draft = %+v, %v", out, err)
	}
	d, _ = draftDeps(1)
	if _, err := draftIdeas(ctx, d, DraftIdeasInput{IdeaIDs: []string{"i1"}}); err == nil {
		t.Fatal("a draft that wrote nothing must fail")
	}
	_, err = draftIdeas(ctx, d, DraftIdeasInput{IdeaIDs: []string{"i1"}, PlatformIDs: []string{"myspace"}})
	refused(t, err)
}
