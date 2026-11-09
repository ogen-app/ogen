//go:build evals

package packs_test

import (
	"cmp"
	"context"
	"database/sql"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/firebase/genkit/go/genkit"
	"github.com/firebase/genkit/go/plugins/anthropic"

	"github.com/ogen-app/ogen/src/domain/modelconfig"
	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/domain/viewcontext"
	"github.com/ogen-app/ogen/src/genkit/flows/assistant"
	"github.com/ogen-app/ogen/src/genkit/flows/assistant/packs"
	"github.com/ogen-app/ogen/src/genkit/flows/campaign_assistant"
	"github.com/ogen-app/ogen/src/genkit/flows/consistency"
	"github.com/ogen-app/ogen/src/genkit/flows/post_assistant"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/infra/vendors"
	"github.com/ogen-app/ogen/src/infra/vendors/llm"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
	"github.com/ogen-app/ogen/src/usecase/activity/report"
	"github.com/ogen-app/ogen/src/usecase/analytics"
	"github.com/ogen-app/ogen/src/usecase/ideas"
	"github.com/ogen-app/ogen/src/usecase/post_actions/compose"
	"github.com/ogen-app/ogen/src/usecase/post_actions/schedule"
	"github.com/ogen-app/ogen/src/usecase/settings"
	"github.com/ogen-app/ogen/src/usecase/viewresolve"
)

// The per-pack evals: does a message, sent from a screen, reach the tool it
// should? Each case makes one live call to the orchestrator model with every
// pack registered as the server registers them, and checks the first tool it
// asks for. No tool runs. Run with `make evals` (ANTHROPIC_API_KEY set;
// EVAL_MODEL_ID overrides the orchestrator model).

type evalCase struct {
	pack   string
	screen viewcontext.Screen
	scope  viewcontext.Scope
	say    string
	// want lists the tools an acceptable first call is one of; not, those it
	// must never be. An empty want means: answer without a tool.
	want []string
	not  []string
}

var (
	evalCampaign  = viewcontext.Scope{Kind: models.AssistantScopeCampaign, ID: "c1"}
	evalPost      = viewcontext.Scope{Kind: models.AssistantScopePost, ID: "p1"}
	evalWorkspace = viewcontext.Scope{Kind: models.AssistantScopeWorkspace}
	writes        = []string{"runContentPlan", "generatePosts", "draftPost", "enrichBrief", "setCampaignDates", "redistributePosts", "editPost", "deleteIdeas"}
)

var evalCases = []evalCase{
	// campaign
	{pack: "campaign", screen: "campaign.overview", scope: evalCampaign, say: "Generate a content plan for this campaign.", want: []string{"runContentPlan"}},
	{pack: "campaign", screen: "campaign.overview", scope: evalCampaign, say: "Add 2 LinkedIn posts for next week.", want: []string{"generatePosts"}},
	{pack: "campaign", screen: "campaign.overview", scope: evalCampaign, say: "Make the brief more technical and benchmark-driven.", want: []string{"enrichBrief"}},
	{pack: "campaign", screen: "campaign.overview", scope: evalCampaign, say: "Move the campaign end to the last day of next month.", want: []string{"setCampaignDates"}},
	{pack: "campaign", screen: "campaign.overview", scope: evalCampaign, say: "Is the brief consistent?", want: []string{"checkBrief"}, not: writes},
	{pack: "campaign", screen: "campaign.overview", scope: evalCampaign, say: "How many posts does each phase have?", want: []string{"getCampaignOverview"}, not: writes},
	{pack: "campaign", screen: "campaign.overview", scope: evalCampaign, say: "What is this campaign about, in one sentence?", not: writes},
	// post
	{pack: "post", screen: "post", scope: evalPost, say: "Shorten the intro.", want: []string{"editPost"}},
	{pack: "post", screen: "post", scope: evalPost, say: "Write the post from its theses.", want: []string{"editPost"}},
	{pack: "post", screen: "post", scope: evalPost, say: "Clone this for Threads.", want: []string{"clonePost"}},
	{pack: "post", screen: "post", scope: evalPost, say: "Undo the last change.", want: []string{"restoreVersion"}},
	{pack: "post", screen: "post", scope: evalPost, say: "Write a Nano Banana image prompt for this post.", want: []string{"createNote"}, not: []string{"editPost"}},
	{pack: "post", screen: "post", scope: evalPost, say: "Schedule it for next Monday at 9am.", want: []string{"schedulePost"}, not: []string{"editPost"}},
	{pack: "post", screen: "post", scope: evalPost, say: "How long is this post?", not: []string{"editPost", "clonePost", "createNote"}},
	// ideas
	{pack: "ideas", screen: "ideas", scope: evalWorkspace, say: "Which undecided ideas are about pricing?", want: []string{"findIdeas", "setFilter"}, not: writes},
	{pack: "ideas", screen: "ideas", scope: evalWorkspace, say: "Find duplicate ideas.", want: []string{"findDuplicateIdeas", "findIdeas"}, not: writes},
	// assets
	{pack: "assets", screen: "assets", scope: evalWorkspace, say: "What do my assets say about enterprise pricing?", want: []string{"askAssets", "findAssets"}, not: writes},
	{pack: "assets", screen: "assets", scope: evalWorkspace, say: "Import https://example.com/launch as an asset.", want: []string{"importPage"}},
	// repair
	{pack: "repair", screen: "campaign.calendar", scope: evalCampaign, say: "Anything wrong with the calendar?", want: []string{"checkCalendar"}, not: writes},
	{pack: "repair", screen: "campaign.calendar", scope: evalCampaign, say: "Even out the calendar.", want: []string{"optimiseCalendar", "redistributePosts", "checkCalendar"}},
	// analytics
	{pack: "analytics", screen: "analytics", scope: evalWorkspace, say: "How did we do over the last 4 weeks?", want: []string{"analyticsOverview"}, not: writes},
	{pack: "analytics", screen: "analytics", scope: evalWorkspace, say: "Which posts performed best?", want: []string{"topPosts"}, not: writes},
	{pack: "analytics", screen: "analytics", scope: evalWorkspace, say: "What's the best time of day to post, judging by what worked?", want: []string{"analyticsLessons"}, not: writes},
	{pack: "analytics", screen: "analytics.post", scope: evalPost, say: "How is this post doing?", want: []string{"postStats"}, not: writes},
	{pack: "ideas", screen: "campaign.ideas", scope: evalCampaign, say: "Merge the duplicate ideas.", want: []string{"findDuplicateIdeas"}, not: []string{"deleteIdeas"}},
	// foundation
	{pack: "foundation", screen: "foundation.facts", scope: evalWorkspace, say: "Add a fact: we have 1,200 customers.", want: []string{"addFact"}},
	{pack: "foundation", screen: "foundation.facts", scope: evalWorkspace, say: "What's our brand voice?", want: []string{"getBrand"}, not: writes},
	// workspace
	{pack: "workspace", screen: "activity", scope: evalWorkspace, say: "What happened yesterday?", want: []string{"activityReport"}},
	{pack: "workspace", screen: "settings", scope: evalWorkspace, say: "Do any of my accounts need reconnecting?", want: []string{"workspaceSetup"}},
	// reaching another part of the app loads its pack
	{pack: "campaign", screen: "home", scope: evalWorkspace, say: "Show me how my posts did last month.", want: []string{"loadPack", "navigate"}},
}

type evalCampaigns struct{ repository.CampaignRepository }

func (evalCampaigns) GetByID(context.Context, string) (*models.Campaign, error) {
	start := time.Now().UTC().Truncate(24 * time.Hour)
	end := start.AddDate(0, 1, 0)
	return &models.Campaign{ID: "c1", Name: "Go for AI engineering", Description: "Using Go for production AI features.",
		KeyMessages: "Static types, single-binary deploys.", Language: "en", StartDate: &start, EndDate: &end,
		Platforms:       []models.Platform{{ID: "pl1", Name: "LinkedIn", PostTypes: models.PostTypeMap{"text-post": "Text"}}},
		TargetPlatforms: models.CampaignPlatforms{{ID: "pl1"}}}, nil
}

type evalPosts struct{ repository.PostRepository }

func (evalPosts) GetByID(context.Context, string) (*models.Post, error) {
	c, _ := evalCampaigns{}.GetByID(context.Background(), "c1")
	return &models.Post{ID: "p1", CampaignID: "c1", Campaign: c, PlatformID: "pl1", Title: "Why Go for AI",
		Content: "# Why Go for AI\n\nGo's static types catch schema drift early. Goroutines suit streaming. One binary ships.",
		Status:  models.PostStatusDraft}, nil
}

// evalNotes gives the post its draft theses, for "write the post from its
// theses".
type evalNotes struct{ repository.PostNoteRepository }

func (evalNotes) ListByPostID(context.Context, string) ([]models.PostNote, error) {
	return []models.PostNote{{ID: "n1", PostID: "p1", Type: models.PostNoteTypeDraftThesis, Title: "Draft thesis",
		Body: "- Static types catch schema drift before production\n- Goroutines fit token streaming\n- One binary ships the model client"}}, nil
}

// evalSettings is a workspace with its timezone set, as real ones are, so
// "next Monday at 9am" has a zone to resolve against.
type evalSettings struct{ repository.SettingRepository }

func (evalSettings) GetByKey(_ context.Context, key string) (*models.Setting, error) {
	if key != settings.TimezoneKey {
		return nil, sql.ErrNoRows
	}
	return &models.Setting{Key: key, Value: "Europe/Kyiv"}, nil
}

type evalVersions struct {
	repository.PostVersionRepository
}

func (evalVersions) CountByPostID(context.Context, string) (int, error) { return 2, nil }
func (evalVersions) ListByPostID(context.Context, string) ([]models.PostVersion, error) {
	return []models.PostVersion{{VersionNumber: 1, Content: "v1", Creator: models.PostVersionCreatorUser, Note: "Initial version"},
		{VersionNumber: 2, Content: "v2", Creator: models.PostVersionCreatorAssistant, Note: "Tighter"}}, nil
}

type evalMessages struct {
	repository.AssistantMessageRepository
}

func (evalMessages) ListRecent(context.Context, models.AssistantScopeKind, string, int) ([]models.AssistantMessage, error) {
	return nil, nil
}

type evalProposals struct {
	repository.AssistantProposalRepository
}

func (evalProposals) ListOpen(context.Context, models.AssistantScopeKind, string, time.Time) ([]models.AssistantProposal, error) {
	return nil, nil
}

func TestPackEvals(t *testing.T) {
	if os.Getenv("ANTHROPIC_API_KEY") == "" {
		t.Skip("ANTHROPIC_API_KEY not set")
	}
	ctx := tenantctx.With(t.Context(), models.DefaultTenantID)
	model := cmp.Or(os.Getenv("EVAL_MODEL_ID"), modelconfig.SeedDefaults.Planning)
	modelconfig.Init(ctx, &memModelConfig{}, modelconfig.Defaults{Generation: model, Planning: model, Quality: model}, vendors.VendorOf, tenantctx.TierFrom)

	g := genkit.Init(ctx, genkit.WithPlugins(&anthropic.Anthropic{}))
	campaign, err := campaign_assistant.NewPack(campaign_assistant.CampaignAssistantFlowConfig{}, campaign_assistant.CampaignAssistantRepos{Campaigns: evalCampaigns{}})
	if err != nil {
		t.Fatal(err)
	}
	post, err := post_assistant.NewPack(g, post_assistant.PostAssistantFlowConfig{ScheduleService: &schedule.Service{}}, post_assistant.PostAssistantRepos{
		Posts: evalPosts{}, Campaigns: evalCampaigns{}, Versions: evalVersions{}, Notes: evalNotes{}, Settings: evalSettings{}})
	if err != nil {
		t.Fatal(err)
	}
	all := packs.All(packs.Deps{
		Campaign: &campaign, Post: &post,
		Ideas: ideas.New(ideaRepo{}, nil), IdeaRepo: ideaRepo{},
		Compose: compose.New(nil, ideaRepo{}, campaignRepo{}, nil, nil, nil),
		Assets:  assetRepo{}, Campaigns: evalCampaigns{},
		Posts: postRepo{}, Links: linkRepo{}, Analytics: &analytics.Service{},
		Brand: brandRepo{}, Activity: report.New(nil, nil, nil),
		CheckBrief: func(context.Context, string, consistency.OnEventFunc) (*consistency.BriefReview, error) {
			return nil, nil
		},
		CheckPosts: func(context.Context, consistency.PostsCheckRequest, consistency.OnEventFunc) (*consistency.PostsReview, error) {
			return nil, nil
		},
	})
	a, err := assistant.Init(g, assistant.FlowConfig{Provider: llm.NewProvider(), MaxOutputTokens: 4096}, assistant.Repos{Messages: evalMessages{}, Proposals: evalProposals{}}, all...)
	if err != nil {
		t.Fatal(err)
	}

	passed := map[string][2]int{}
	for _, c := range evalCases {
		t.Run(fmt.Sprintf("%s/%s", c.pack, c.say), func(t *testing.T) {
			view := &viewresolve.Resolved{View: &viewcontext.ViewContext{Version: viewcontext.Version, Screen: c.screen, Scope: c.scope}, Summary: "On " + string(c.screen)}
			r, err := a.Route(ctx, assistant.Request{Instruction: c.say, View: view})
			if err != nil {
				t.Fatal(err)
			}
			ok, why := c.judge(r)
			n := passed[c.pack]
			n[1]++
			if ok {
				n[0]++
			}
			passed[c.pack] = n
			if !ok {
				t.Errorf("%s — packs %v, calls %v, reply %q", why, r.Packs, names(r.Calls), r.Reply)
			}
		})
	}
	for _, p := range slices.Sorted(maps.Keys(passed)) {
		t.Logf("%-10s %d/%d", p, passed[p][0], passed[p][1])
	}
}

// judge checks the first call: one of want (or only reads when want is
// empty), and no call at all among not.
func (c evalCase) judge(r *assistant.Routing) (bool, string) {
	got := names(r.Calls)
	for _, n := range got {
		if slices.Contains(c.not, n) {
			return false, "called " + n
		}
	}
	switch {
	case len(c.want) == 0 && slices.ContainsFunc(got, func(n string) bool { return !strings.HasPrefix(n, "get") && n != "listCampaignPosts" }):
		return false, "called a tool for a plain answer"
	case len(c.want) > 0 && (len(got) == 0 || !slices.Contains(c.want, got[0])):
		return false, fmt.Sprintf("first call isn't one of %v", c.want)
	}
	return true, ""
}

func names(calls []assistant.RoutedCall) []string {
	out := make([]string, len(calls))
	for i, c := range calls {
		out[i] = c.Name
	}
	return out
}

// memModelConfig is an empty model-config store: every slot takes its
// default.
type memModelConfig struct{ rows []models.FlowModelConfig }

func (m *memModelConfig) List(context.Context) ([]models.FlowModelConfig, error) { return m.rows, nil }
func (m *memModelConfig) Upsert(_ context.Context, c *models.FlowModelConfig) error {
	m.rows = append(m.rows, *c)
	return nil
}
