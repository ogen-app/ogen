package campaign_assistant

import (
	"slices"
	"testing"

	"github.com/firebase/genkit/go/genkit"
)

func newTestTools(t *testing.T) *toolSet {
	t.Helper()
	tools, err := defineTools(genkit.Init(t.Context()))
	if err != nil {
		t.Fatal(err)
	}
	return tools
}

// The model is given the pack's tools in this order.
func TestToolOrder(t *testing.T) {
	want := []string{"runContentPlan", "enrichBrief", "listCampaignPosts", "getCampaignOverview", "generatePosts",
		"draftPost", "setCampaignDates", "redistributePosts", "checkBrief", "checkPostsConsistency",
		"updateCampaignPlatforms", "setCampaignGoal", "replaceWeakestDrafts", "askCampaignAssets"}
	var got []string
	for _, r := range newTestTools(t).reg.Tools() {
		got = append(got, r.Name())
	}
	if !slices.Equal(got, want) {
		t.Fatalf("tool order = %v, want %v", got, want)
	}
}
