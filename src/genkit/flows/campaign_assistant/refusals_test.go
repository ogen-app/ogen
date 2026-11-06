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
	}
	// The campaign narrowed LinkedIn to two of the platform's types.
	c.TargetPlatforms = models.CampaignPlatforms{{ID: "pl1", PostTypes: []string{"article", "carousel"}}, {ID: "pl2"}}
	return c
}

func TestUnsupportedPostType(t *testing.T) {
	c := postTypeCampaign()
	cases := []struct {
		name      string
		platforms []string
		postType  string
		bad       bool
		alts      []string
	}{
		{"platform default", []string{"pl1"}, "", false, nil},
		{"campaign selection allows it", []string{"pl1"}, "article", false, nil},
		{"campaign selection excludes it", []string{"pl1"}, "text-post", true, []string{"article", "carousel"}},
		{"no campaign selection, platform allows it", []string{"pl2"}, "text-post", false, nil},
		{"no campaign selection, platform lacks it", []string{"pl2"}, "article", true, []string{"text-post"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reason, alts, bad := unsupportedPostType(c, tc.platforms, tc.postType)
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
