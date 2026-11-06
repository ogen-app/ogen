package campaign_assistant

import (
	"reflect"
	"slices"
	"strings"
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

// The model is given the tools in this order, and the prewarm warms the
// grammar for exactly this list.
func TestToolOrder(t *testing.T) {
	want := []string{"runContentPlan", "enrichBrief", "listCampaignPosts", "getCampaignOverview", "generatePosts",
		"draftPost", "setCampaignDates", "redistributePosts", "checkBrief", "checkPostsConsistency",
		"updateCampaignPlatforms", "confirmProposal", "dismissProposal", "askCampaignAssets"}
	var got []string
	for _, r := range newTestTools(t).all() {
		got = append(got, r.Name())
	}
	if !slices.Equal(got, want) {
		t.Fatalf("tool order = %v, want %v", got, want)
	}
}

// The prompt is rendered from the registry, so every tool the model can call
// is described, and every action it may answer with is one it is told about.
func TestSystemPromptMatchesRegistry(t *testing.T) {
	tools := newTestTools(t)
	p, err := loadPrompts(tools.reg)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(p.system, "{{") || strings.Contains(p.system, "<no value>") {
		t.Fatal("system prompt has unrendered template fields")
	}
	for _, s := range tools.reg.Specs() {
		if !strings.Contains(p.system, "\n- "+s.Name+": ") {
			t.Errorf("tool %s has no guide line in the system prompt", s.Name)
		}
	}

	actions := []string{actionAnswered, actionProposed}
	for _, a := range tools.reg.Actions() {
		actions = append(actions, a.Name)
		if !strings.Contains(p.system, `- "`+a.Name+`" — `+a.Rule) {
			t.Errorf("action %s has no rule in the system prompt", a.Name)
		}
	}
	actions = append(actions, "declined")
	if want := `"action": "<` + strings.Join(actions, " | ") + `>"`; !strings.Contains(p.system, want) {
		t.Errorf("system prompt lacks the action enum %s", want)
	}

	// The response schema the envelope is parsed against must allow exactly
	// the same actions.
	field, _ := reflect.TypeFor[CampaignAssistantResponse]().FieldByName("Action")
	var schemaEnum []string
	for part := range strings.SplitSeq(field.Tag.Get("jsonschema"), ",") {
		if v, ok := strings.CutPrefix(part, "enum="); ok {
			schemaEnum = append(schemaEnum, v)
		}
	}
	slices.Sort(schemaEnum)
	slices.Sort(actions)
	if !slices.Equal(schemaEnum, actions) {
		t.Errorf("response schema actions = %v, prompt actions = %v", schemaEnum, actions)
	}
}
