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
	repo.byID["i1"].CampaignID, repo.byID["i1"].SourceAssetID, repo.byID["i1"].AssetAnalysisID = &c1, new("a1"), new("an1")
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
// streamed event: the brochure, through the idea and the analysis it came
// from, for the analysed idea; nothing for the one written by hand.
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
	if first.Grounding.Mode != assistantkit.Grounded || !slices.Equal(first.Grounding.Sources, []assistantkit.Source{{ID: "a1", Title: "Brochure", AnalysisID: "an1", IdeaID: "i1"}}) ||
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
	in := DraftIdeasInput{IdeaIDs: []string{"i1", "i2"}}
	out, err := draftIdeas(ctx, d, in)
	if err != nil || len(out.Posts) != 1 || len(out.Warnings) != 1 {
		t.Fatalf("draft = %+v, %v", out, err)
	}
	// The idea the stop left is offered again.
	if retry, notMade, ok := draftRetry(in, out); !ok || !slices.Equal(retry.IdeaIDs, []string{"i2"}) || len(notMade) != 1 {
		t.Fatalf("retry = %+v, notMade = %+v", retry, notMade)
	}
	d, _ = draftDeps(1)
	if _, err := draftIdeas(ctx, d, DraftIdeasInput{IdeaIDs: []string{"i1"}}); err == nil {
		t.Fatal("a draft that wrote nothing must fail")
	}
	_, err = draftIdeas(ctx, d, DraftIdeasInput{IdeaIDs: []string{"i1"}, PlatformIDs: []string{"myspace"}})
	refused(t, err)
}

// A stop leaves a tail of the jobs: a whole grid of ideas and platforms is
// retried as it is; part of an idea's platforms leaves the retry to the
// ideas nothing was written from.
func TestUnwritten(t *testing.T) {
	plan := &compose.DraftPlan{Ideas: []models.Idea{{ID: "a", Title: "A"}, {ID: "b", Title: "B"}}}
	for _, idea := range []string{"a", "b"} {
		for _, p := range []string{"li", "x"} {
			plan.Jobs = append(plan.Jobs, compose.DraftJob{IdeaIDs: []string{idea}, Request: compose.DraftRequest{PlatformID: p}})
		}
	}
	post := func(idea, platform string) compose.DraftedPost {
		return compose.DraftedPost{Written: compose.Written{PlatformID: platform}, IdeaIDs: []string{idea}}
	}
	in := DraftIdeasInput{IdeaIDs: []string{"a", "b"}, PlatformIDs: []string{"li", "x"}}

	retry, notMade := unwritten(plan, []compose.DraftedPost{post("a", "li"), post("a", "x")}, in, "stopped")
	if retry == nil || !slices.Equal(retry.IdeaIDs, []string{"b"}) || !slices.Equal(retry.PlatformIDs, []string{"li", "x"}) || len(notMade) != 2 || notMade[0].Title != "B · li" {
		t.Fatalf("whole idea left: retry = %+v, notMade = %+v", retry, notMade)
	}
	retry, notMade = unwritten(plan, []compose.DraftedPost{post("a", "li")}, in, "stopped")
	if retry == nil || !slices.Equal(retry.IdeaIDs, []string{"b"}) || !slices.Equal(retry.PlatformIDs, []string{"li", "x"}) || len(notMade) != 3 {
		t.Fatalf("part of an idea left: retry = %+v, notMade = %+v", retry, notMade)
	}
	together := DraftIdeasInput{IdeaIDs: []string{"a", "b"}, PlatformIDs: []string{"li", "x"}, Together: true}
	plan.Jobs = []compose.DraftJob{{IdeaIDs: []string{"a", "b"}, Request: compose.DraftRequest{PlatformID: "li"}}, {IdeaIDs: []string{"a", "b"}, Request: compose.DraftRequest{PlatformID: "x"}}}
	retry, _ = unwritten(plan, []compose.DraftedPost{post("a", "li")}, together, "stopped")
	if retry == nil || !slices.Equal(retry.IdeaIDs, []string{"a", "b"}) || !slices.Equal(retry.PlatformIDs, []string{"x"}) {
		t.Fatalf("together: retry = %+v", retry)
	}
}
