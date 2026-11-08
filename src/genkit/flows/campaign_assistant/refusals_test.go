package campaign_assistant

import (
	"context"
	"slices"
	"testing"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/genkit/flows/content_plan"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/assistantkit"
)

func postTypeCampaign() *models.Campaign {
	c := timelineCampaign()
	c.Platforms = []models.Platform{
		{ID: "pl1", Name: "LinkedIn", PostTypes: models.PostTypeMap{"article": "Article", "carousel": "Carousel", "text-post": "Text post"}},
		{ID: "pl2", Name: "Threads", PostTypes: models.PostTypeMap{"text-post": "Text post"}},
		{ID: "pl3", Name: "Bluesky"},
	}
	// The campaign narrowed LinkedIn to two of the platform's types; Bluesky
	// has none to choose from.
	c.TargetPlatforms = models.CampaignPlatforms{{ID: "pl1", PostTypes: []string{"article", "carousel"}}, {ID: "pl2"}, {ID: "pl3"}}
	return c
}

func TestUnsupportedPostType(t *testing.T) {
	c := postTypeCampaign()
	cases := []struct {
		name      string
		platforms []string
		postType  string
		anyWhen   bool
		bad       bool
		alts      []string
	}{
		{"platform default", []string{"pl1"}, "", false, false, nil},
		{"campaign selection allows it", []string{"pl1"}, "article", false, false, nil},
		{"campaign selection excludes it", []string{"pl1"}, "text-post", false, true, []string{"article", "carousel"}},
		{"no campaign selection, platform allows it", []string{"pl2"}, "text-post", false, false, nil},
		{"no campaign selection, platform lacks it", []string{"pl2"}, "article", false, true, []string{"text-post"}},
		{"no types at all, planning refuses", []string{"pl3"}, "article", false, true, nil},
		{"no types at all, drafting keeps it", []string{"pl3"}, "article", true, false, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reason, alts, bad := unsupportedPostType(c, tc.platforms, tc.postType, tc.anyWhen)
			if bad != tc.bad || !slices.Equal(alts, tc.alts) {
				t.Fatalf("bad = %t alts = %v, want %t %v", bad, alts, tc.bad, tc.alts)
			}
			if bad && reason == "" {
				t.Fatal("a refusal needs a reason")
			}
		})
	}
}

// The beta-test failure: "add LinkedIn text posts" aborted the whole turn with
// "LinkedIn does not support post type text-post". It is now a refusal that
// lists what LinkedIn can take, and the engine never runs.
func TestGeneratePosts_RefusesUnsupportedPostType(t *testing.T) {
	st := &requestState{
		campaignID:       "c1",
		campaign:         postTypeCampaign(),
		maxGeneratePosts: 10,
		generatePosts: func(context.Context, content_plan.GeneratePostsRequest, content_plan.OnEventFunc) (*content_plan.ContentPlanResponse, error) {
			t.Fatal("generation engine must not run for an unsupported post type")
			return nil, nil
		},
	}
	out, err := toolGeneratePosts(withRequestState(t.Context(), st), GeneratePostsInput{Platforms: []string{"LinkedIn"}, PostType: "text-post"})
	if err != nil {
		t.Fatalf("want a refusal, got error %v", err)
	}
	if out.Status != assistantkit.StatusRefused || !slices.Equal(out.Alternatives, []string{"article", "carousel"}) {
		t.Fatalf("out = %+v", out)
	}
	if st.heavyReserved {
		t.Fatal("a refusal must not reserve the heavy slot")
	}
}

// A post type with stray spaces is the type: the engines trim it.
func TestGeneratePosts_TrimsThePostType(t *testing.T) {
	var got content_plan.GeneratePostsRequest
	st := &requestState{
		campaignID: "c1", campaign: postTypeCampaign(), maxGeneratePosts: 10,
		generatePosts: func(_ context.Context, req content_plan.GeneratePostsRequest, _ content_plan.OnEventFunc) (*content_plan.ContentPlanResponse, error) {
			got = req
			return &content_plan.ContentPlanResponse{}, nil
		},
	}
	out, err := toolGeneratePosts(withRequestState(t.Context(), st), GeneratePostsInput{Platforms: []string{"LinkedIn"}, PostType: " article "})
	if err != nil || out.Status != assistantkit.StatusOK || got.PostType != "article" {
		t.Fatalf("out = %+v, err = %v, engine got %q", out, err, got.PostType)
	}
}

// A platform with no types refuses an explicit one before the heavy slot is
// taken, so a corrected call in the same turn still runs.
func TestGeneratePosts_NoTypesRefusedBeforeTheSlot(t *testing.T) {
	st := &requestState{
		campaignID: "c1", campaign: postTypeCampaign(), maxGeneratePosts: 10,
		generatePosts: func(context.Context, content_plan.GeneratePostsRequest, content_plan.OnEventFunc) (*content_plan.ContentPlanResponse, error) {
			return &content_plan.ContentPlanResponse{}, nil
		},
	}
	out, err := toolGeneratePosts(withRequestState(t.Context(), st), GeneratePostsInput{Platforms: []string{"Bluesky"}, PostType: "article"})
	if err != nil || out.Status != assistantkit.StatusRefused || st.heavyReserved {
		t.Fatalf("out = %+v, err = %v, reserved = %t", out, err, st.heavyReserved)
	}
	if out, _ := toolGeneratePosts(withRequestState(t.Context(), st), GeneratePostsInput{Platforms: []string{"Bluesky"}}); out.Status != assistantkit.StatusOK {
		t.Fatalf("the corrected call = %+v", out)
	}
}

// A validation error the engine raises itself is also a refusal, not a turn
// abort.
func TestGeneratePosts_EngineValidationErrorIsRefusal(t *testing.T) {
	st := &requestState{
		campaignID:       "c1",
		campaign:         timelineCampaign(),
		maxGeneratePosts: 10,
		generatePosts: func(context.Context, content_plan.GeneratePostsRequest, content_plan.OnEventFunc) (*content_plan.ContentPlanResponse, error) {
			return nil, &content_plan.ValidationError{Msg: "no platforms specified"}
		},
	}
	out, err := toolGeneratePosts(withRequestState(t.Context(), st), GeneratePostsInput{Platforms: []string{"Threads"}})
	if err != nil {
		t.Fatalf("want a refusal, got error %v", err)
	}
	if out.Status != assistantkit.StatusRefused || out.Reason != "no platforms specified" {
		t.Fatalf("out = %+v", out)
	}
}

// A confirmed call that refuses before generating gives the held slot back,
// so a corrected call in the same message runs; one that generated keeps it.
func TestHoldHeavySlot_ReleasedUnlessUsed(t *testing.T) {
	ran := 0
	st := &requestState{
		campaignID: "c1", campaign: postTypeCampaign(), maxGeneratePosts: 10,
		generatePosts: func(context.Context, content_plan.GeneratePostsRequest, content_plan.OnEventFunc) (*content_plan.ContentPlanResponse, error) {
			ran++
			return &content_plan.ContentPlanResponse{}, nil
		},
	}
	ctx := withRequestState(t.Context(), st)
	held, release, err := holdHeavySlot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	out, err := toolGeneratePosts(held, GeneratePostsInput{Platforms: []string{"LinkedIn"}, PostType: "text-post"})
	release()
	if err != nil || out.Status != assistantkit.StatusRefused || st.heavyReserved {
		t.Fatalf("out = %+v, err = %v, reserved = %t; want a refusal that frees the slot", out, err, st.heavyReserved)
	}
	if out, _ := toolGeneratePosts(ctx, GeneratePostsInput{Platforms: []string{"LinkedIn"}}); out.Status != assistantkit.StatusOK || ran != 1 {
		t.Fatalf("the corrected call = %+v, ran = %d", out, ran)
	}

	st.heavyReserved = false
	held, release, err = holdHeavySlot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if out, _ := toolGeneratePosts(held, GeneratePostsInput{Platforms: []string{"LinkedIn"}}); out.Status != assistantkit.StatusOK {
		t.Fatalf("the confirmed call = %+v", out)
	}
	release()
	if !st.heavyReserved {
		t.Fatal("a confirmed call that generated gave the slot back")
	}
}
