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
	"github.com/ogen-app/ogen/src/genkit/flows/asset_compare"
	"github.com/ogen-app/ogen/src/genkit/flows/assistant"
	"github.com/ogen-app/ogen/src/genkit/flows/assistant/packs"
	"github.com/ogen-app/ogen/src/genkit/flows/brand_check"
	"github.com/ogen-app/ogen/src/genkit/flows/campaign_assistant"
	"github.com/ogen-app/ogen/src/genkit/flows/consistency"
	"github.com/ogen-app/ogen/src/genkit/flows/more_like_these"
	"github.com/ogen-app/ogen/src/genkit/flows/post_assistant"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/infra/vendors"
	"github.com/ogen-app/ogen/src/infra/vendors/llm"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
	"github.com/ogen-app/ogen/src/usecase/activity/report"
	"github.com/ogen-app/ogen/src/usecase/analytics"
	"github.com/ogen-app/ogen/src/usecase/branddistill"
	"github.com/ogen-app/ogen/src/usecase/campaign_actions/create"
	"github.com/ogen-app/ogen/src/usecase/ideas"
	"github.com/ogen-app/ogen/src/usecase/post_actions/compose"
	"github.com/ogen-app/ogen/src/usecase/post_actions/schedule"
	"github.com/ogen-app/ogen/src/usecase/savedviews"
	"github.com/ogen-app/ogen/src/usecase/series"
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
	// focus is the item the screen shows, on a detail screen, and title its
	// title; the asset brochure's when empty.
	focus *viewcontext.Ref
	title string
	// selected are the posts the user ticked, by title.
	selected []string
	// window is the analytics window on screen.
	window *viewcontext.Window
}

var (
	evalCampaign  = viewcontext.Scope{Kind: models.AssistantScopeCampaign, ID: "c1"}
	evalPost      = viewcontext.Scope{Kind: models.AssistantScopePost, ID: "p1"}
	evalWorkspace = viewcontext.Scope{Kind: models.AssistantScopeWorkspace}
	evalOpenAsset = &viewcontext.Ref{Kind: viewcontext.KindAsset, ID: "a1"}
	evalOpenVoice = &viewcontext.Ref{Kind: viewcontext.KindVoice, ID: "v1"}
	evalOpenAud   = &viewcontext.Ref{Kind: viewcontext.KindAudience, ID: "au1"}
	evalOpenSer   = &viewcontext.Ref{Kind: viewcontext.KindSeries, ID: "s1"}
	writes        = []string{"runContentPlan", "generatePosts", "draftPost", "enrichBrief", "setCampaignDates", "redistributePosts", "editPost", "deleteIdeas",
		"addVoice", "updateVoice", "removeVoice", "addAudience", "updateAudience", "removeAudience", "updateGuardrails",
		"createSeries", "updateSeries", "removeSeries", "runSeries", "stopSeries", "editBrand",
		"startBrandDistill", "answerDistillQuestions", "commitBrandDistill",
		"applyCalendarMoves", "unschedulePosts", "deletePosts", "changePostType", "revisePost"}
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
	{pack: "assets", screen: "assets", scope: evalWorkspace, say: "How many analyses failed, and which?", not: []string{"findAssets", "askAssets", "analyseAsset"}},
	{pack: "assets", screen: "asset", scope: evalWorkspace, focus: evalOpenAsset, say: "What does it promise about the spa?", want: []string{"askAssets"}, not: writes},
	{pack: "assets", screen: "asset", scope: evalWorkspace, focus: evalOpenAsset, say: "What did the analysis find?", want: []string{"readAnalysis"}, not: writes},
	{pack: "assets", screen: "asset", scope: evalWorkspace, focus: evalOpenAsset, say: "Summarise it in a paragraph.", want: []string{"compareAssets"}, not: writes},
	{pack: "assets", screen: "assets", scope: evalWorkspace, say: "Compare the Villa Aurora brochure with the press kit.", want: []string{"findAssets", "getSelection", "compareAssets"}, not: writes},
	{pack: "assets", screen: "assets", scope: evalWorkspace, say: "Which smart groups do we have?", want: []string{"listSavedViews"}, not: writes},
	{pack: "assets", screen: "assets", scope: evalWorkspace, say: "Open the Needs analysis group.", want: []string{"listSavedViews"}},
	{pack: "assets", screen: "asset", scope: evalWorkspace, focus: evalOpenAsset, say: "Set up a campaign from this, around its March opening.", want: []string{"createCampaignFromAssets"}},
	{pack: "assets", screen: "assets", scope: evalWorkspace, say: "Make a campaign from the brochure and the press kit.", want: []string{"findAssets", "getSelection", "createCampaignFromAssets"}},
	{pack: "campaign", screen: "campaign.asset", scope: evalCampaign, focus: evalOpenAsset, say: "Plan 3 LinkedIn posts from this for next week.", want: []string{"generatePosts"}},
	// repair
	{pack: "repair", screen: "campaign.calendar", scope: evalCampaign, say: "Anything wrong with the calendar?", want: []string{"checkCalendar"}, not: writes},
	{pack: "repair", screen: "campaign.calendar", scope: evalCampaign, say: "Even out the calendar.", want: []string{"optimiseCalendar", "redistributePosts", "checkCalendar"}},
	{pack: "repair", screen: "post", scope: evalPost, say: "Is this post on brand? Check it against our voice and guardrails.", want: []string{"loadPack", "checkBrand"}, not: writes},
	{pack: "repair", screen: "campaign.list", scope: evalCampaign, selected: []string{"Go in production", "Single-binary deploys"}, say: "Do the selected posts use any banned words or claims we can't make?", want: []string{"getSelection", "checkBrand"}, not: writes},
	{pack: "repair", screen: "campaign.calendar", scope: evalCampaign, say: "Are we on track for our posting goal each week?", want: []string{"checkCalendar", "getCampaignOverview"}, not: writes},
	{pack: "repair", screen: "campaign.overview", scope: evalCampaign, say: "Fix the problem crowded_slot:4f2a9c1d7e3b from the overview.", want: []string{"loadPack", "checkCalendar"}, not: writes},
	{pack: "repair", screen: "campaign.calendar", scope: evalCampaign, say: "Which posts reuse the same idea too close together?", want: []string{"checkCalendar"}, not: writes},
	{pack: "repair", screen: "campaign.list", scope: evalCampaign, selected: []string{"Go in production", "Single-binary deploys"}, say: "Take the selected posts off the calendar.", want: []string{"getSelection", "unschedulePosts"}, not: []string{"deletePosts"}},
	{pack: "repair", screen: "campaign.list", scope: evalCampaign, selected: []string{"Go in production", "Single-binary deploys"}, say: "Delete the selected posts.", want: []string{"getSelection", "deletePosts"}},
	{pack: "repair", screen: "campaign.list", scope: evalCampaign, selected: []string{"Go in production", "Single-binary deploys"}, say: "Turn the selected posts into image posts.", want: []string{"getSelection", "changePostType"}, not: []string{"editPost", "revisePost"}},
	{pack: "repair", screen: "campaign.overview", scope: evalCampaign, say: "Do our posts still follow the brief? Rewrite the ones that drift.", want: []string{"loadPack", "reviewCampaign", "checkPostsConsistency"}, not: []string{"revisePost", "editPost"}},
	{pack: "repair", screen: "campaign.list", scope: evalCampaign, selected: []string{"Go in production"}, say: "Rewrite the selected post to lead with the benchmark numbers.", want: []string{"getSelection", "revisePost"}, not: []string{"editPost"}},
	// analytics
	{pack: "analytics", screen: "analytics", scope: evalWorkspace, say: "How did we do over the last 4 weeks?", want: []string{"analyticsOverview"}, not: writes},
	{pack: "analytics", screen: "analytics", scope: evalWorkspace, say: "Which posts performed best?", want: []string{"topPosts"}, not: writes},
	{pack: "analytics", screen: "analytics", scope: evalWorkspace, say: "What's the best time of day to post, judging by what worked?", want: []string{"analyticsLessons", "bestTimes"}, not: writes},
	{pack: "analytics", screen: "analytics", scope: evalWorkspace, say: "What does the publisher say is the best hour to post on Instagram?", want: []string{"bestTimes"}, not: writes},
	{pack: "analytics", screen: "analytics", scope: evalWorkspace, say: "Do we post too often on LinkedIn? What cadence works best?", want: []string{"postingFrequency"}, not: writes},
	{pack: "analytics", screen: "analytics", scope: evalWorkspace, say: "How quickly do our posts get most of their engagement?", want: []string{"contentDecay", "analyticsLessons"}, not: writes},
	{pack: "analytics", screen: "analytics", scope: evalWorkspace, say: "How have our followers grown since March?", want: []string{"followerHistory"}, not: writes},
	{pack: "analytics", screen: "analytics", scope: evalWorkspace, say: "List our posts by saves.", want: []string{"listPostAnalytics"}, not: writes},
	{pack: "analytics", screen: "analytics", scope: evalWorkspace, say: "Compare March with April.", want: []string{"comparePeriods"}, not: writes},
	{pack: "analytics", screen: "analytics", scope: evalWorkspace, say: "Show only LinkedIn for last month on the dashboard.", want: []string{"setAnalyticsWindow"}, not: writes},
	{pack: "analytics", screen: "analytics", scope: evalWorkspace, window: &viewcontext.Window{From: "2026-10-01", To: "2026-10-31", CampaignID: "c1"},
		say: "How did reach do in this view?", want: []string{"analyticsOverview"}, not: writes},
	{pack: "analytics", screen: "analytics", scope: evalWorkspace, selected: []string{"Why discounts backfire", "The 3 pricing pages"},
		say: "Give me three ideas like the selected posts.", want: []string{"moreLikeThese", "getSelection"}, not: []string{"draftPost", "draftIdeas", "generatePosts"}},
	{pack: "analytics", screen: "analytics", scope: evalWorkspace, say: "Make three more like our top performers.", want: []string{"topPosts", "moreLikeThese"}, not: []string{"draftPost", "draftIdeas", "generatePosts"}},
	{pack: "analytics", screen: "analytics.post", scope: evalPost, say: "How is this post doing?", want: []string{"postStats"}, not: writes},
	{pack: "ideas", screen: "campaign.ideas", scope: evalCampaign, say: "Merge the duplicate ideas.", want: []string{"findDuplicateIdeas"}, not: []string{"deleteIdeas"}},
	// foundation
	{pack: "foundation", screen: "foundation.facts", scope: evalWorkspace, say: "Add a fact: we have 1,200 customers.", want: []string{"addFact"}},
	{pack: "foundation", screen: "foundation.facts", scope: evalWorkspace, say: "What's our brand voice?", want: []string{"getBrand"}, not: writes},
	{pack: "foundation", screen: "foundation.facts", scope: evalWorkspace, say: "Which of our facts have expired?", want: []string{"getBrand"}, not: writes},
	{pack: "foundation", screen: "foundation.templates", scope: evalWorkspace, say: "What templates do we have?", want: []string{"getBrand"}, not: writes},
	{pack: "foundation", screen: "foundation.voices", scope: evalWorkspace, say: "Add a casual voice for Instagram: short posts, emoji welcome.", want: []string{"addVoice"}},
	{pack: "foundation", screen: "foundation.voice", scope: evalWorkspace, focus: evalOpenVoice, title: "Formal", say: "Make this the default voice.", want: []string{"updateVoice"}},
	{pack: "foundation", screen: "foundation.voice", scope: evalWorkspace, focus: evalOpenVoice, title: "Formal", say: "Summarise this voice for me.", want: []string{"getBrand"}, not: writes},
	{pack: "foundation", screen: "foundation.audiences", scope: evalWorkspace, say: "Add an audience: CTOs at mid-size SaaS companies who read on LinkedIn.", want: []string{"addAudience"}},
	{pack: "foundation", screen: "foundation.audiences", scope: evalWorkspace, say: "Delete the Students audience.", want: []string{"getBrand", "removeAudience"}},
	{pack: "foundation", screen: "foundation.guardrails", scope: evalWorkspace, say: "Never use the word 'synergy'.", want: []string{"updateGuardrails"}, not: []string{"addFact"}},
	{pack: "foundation", screen: "foundation.facts", scope: evalWorkspace, say: "Add these facts: founded in 2019; 40 employees; offices in Kyiv and Lisbon; ISO 27001 certified.", want: []string{"editBrand"}},
	{pack: "foundation", screen: "foundation.series", scope: evalWorkspace, say: "Which series do we have?", want: []string{"listSeries"}, not: writes},
	{pack: "foundation", screen: "foundation.series", scope: evalWorkspace, say: "Start a weekly 'Tip Tuesday' series: one practical tip per post.", want: []string{"createSeries"}},
	{pack: "foundation", screen: "foundation.voice", scope: evalWorkspace, focus: evalOpenVoice, title: "Formal", say: "Delete this voice.", want: []string{"removeVoice"}},
	{pack: "foundation", screen: "foundation.audience", scope: evalWorkspace, focus: evalOpenAud, title: "CTOs", say: "They mostly read on LinkedIn, not X. Update that.", want: []string{"getBrand", "updateAudience"}},
	{pack: "foundation", screen: "foundation.series_item", scope: evalWorkspace, focus: evalOpenSer, title: "Tip Tuesday", say: "Make this series twice a week by default.", want: []string{"updateSeries"}},
	{pack: "foundation", screen: "foundation.series_item", scope: evalWorkspace, focus: evalOpenSer, title: "Tip Tuesday", say: "Delete this series.", want: []string{"removeSeries"}},
	{pack: "foundation", screen: "foundation.series_item", scope: evalCampaign, focus: evalOpenSer, title: "Tip Tuesday", say: "Stop running this series in this campaign.", want: []string{"stopSeries"}},
	{pack: "foundation", screen: "campaign.overview", scope: evalCampaign, say: "Run the Tip Tuesday series in this campaign, twice a month.", want: []string{"loadPack", "listSeries", "runSeries"}},
	{pack: "foundation", screen: "foundation", scope: evalWorkspace, say: "Fill in our brand from our website, villa-aurora.com.", want: []string{"startBrandDistill"}},
	{pack: "foundation", screen: "foundation.questions", scope: evalWorkspace, say: "What does the brand reading still want to know?", want: []string{"getBrandDistill"}, not: writes},
	{pack: "foundation", screen: "foundation.questions", scope: evalWorkspace, say: "Answer its question about who books most: families with young kids.", want: []string{"getBrandDistill", "answerDistillQuestions"}},
	{pack: "foundation", screen: "foundation", scope: evalWorkspace, say: "Save everything the brand reading proposed.", want: []string{"getBrandDistill", "commitBrandDistill"}},
	// workspace
	{pack: "workspace", screen: "activity", scope: evalWorkspace, say: "What happened yesterday?", want: []string{"activityReport"}},
	{pack: "workspace", screen: "settings", scope: evalWorkspace, say: "Do any of my accounts need reconnecting?", want: []string{"workspaceSetup"}},
	// reaching another part of the app loads its pack
	{pack: "campaign", screen: "home", scope: evalWorkspace, say: "Show me how my posts did last month.", want: []string{"loadPack", "navigate"}},
}

// evalRemover lets the repair pack offer deleting and withdrawing posts.
type evalRemover struct{}

func (evalRemover) Delete(context.Context, *models.Post, string) (bool, error) { return true, nil }
func (evalRemover) Cancel(context.Context, string, string) error               { return nil }

type evalCampaigns struct{ repository.CampaignRepository }

func (evalCampaigns) GetByID(context.Context, string) (*models.Campaign, error) {
	start := time.Now().UTC().Truncate(24 * time.Hour)
	end := start.AddDate(0, 1, 0)
	return &models.Campaign{ID: "c1", Name: "Go for AI engineering", Description: "Using Go for production AI features.",
		KeyMessages: "Static types, single-binary deploys.", Language: "en", StartDate: &start, EndDate: &end,
		Platforms:       []models.Platform{{ID: "pl1", Name: "LinkedIn", PostTypes: models.PostTypeMap{"text-post": "Text"}}},
		TargetPlatforms: models.CampaignPlatforms{{ID: "pl1"}}}, nil
}

// evalAssets and evalAnalyses are a bank with work waiting, for the assets
// pack's state block.
type evalAssets struct{ repository.AssetRepository }

func (evalAssets) Query(context.Context, models.AssetQuery) (*models.AssetQueryResult, error) {
	return &models.AssetQueryResult{TotalUnfiltered: 64}, nil
}

type evalAnalyses struct {
	repository.AssetAnalysisRepository
}

func (evalAnalyses) State(context.Context, *string, time.Time, int) (*repository.AnalysisState, error) {
	return &repository.AnalysisState{
		Lenses:    map[models.AssetAnalysisStatus]int{models.AssetAnalysisStatusReady: 49, models.AssetAnalysisStatusPending: 12, models.AssetAnalysisStatusFailed: 3},
		Failed:    []repository.AssetTitle{{ID: "a3", Title: "Coast & Table review"}, {ID: "a4", Title: "Press kit"}, {ID: "a5", Title: "Spa menu"}},
		Undecided: []repository.LensIdeas{{Ideas: 29}},
	}, nil
}

type evalTypes struct {
	repository.CampaignTypeRepository
}

func (evalTypes) List(context.Context) ([]models.CampaignType, error) {
	return []models.CampaignType{{ID: "t1", Name: "evergreen", Label: "Evergreen", IsSystem: true}, {ID: "t2", Name: "launch", Label: "Launch", IsSystem: true}}, nil
}

type evalSavedViews struct{}

func (evalSavedViews) List(context.Context, savedviews.Caller, string, []string, *time.Location) ([]models.SavedView, error) {
	return []models.SavedView{{ID: "sv1", Name: "Needs analysis", Query: "ideas=unread", Valid: true}}, nil
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
		Assets:  evalAssets{}, Analyses: evalAnalyses{}, Campaigns: evalCampaigns{},
		Compare: func(context.Context, asset_compare.Request) (*asset_compare.Result, error) {
			return &asset_compare.Result{}, nil
		},
		CreateCampaign: &create.Service{}, CampaignTypes: evalTypes{},
		Posts: postRepo{}, Links: linkRepo{}, Analytics: &analytics.Service{},
		Brand: brandRepo{}, Series: series.New(nil), Distill: &branddistill.Service{}, Activity: report.New(nil, nil, nil),
		CheckBrief: func(context.Context, string, consistency.OnEventFunc) (*consistency.BriefReview, error) {
			return nil, nil
		},
		CheckPosts: func(context.Context, consistency.PostsCheckRequest, consistency.OnEventFunc) (*consistency.PostsReview, error) {
			return nil, nil
		},
		CheckBrand: func(context.Context, brand_check.Request) (*brand_check.Result, error) { return nil, nil },
		MoreLike:   func(context.Context, more_like_these.Request) ([]more_like_these.Idea, error) { return nil, nil },
		Remove:     evalRemover{},
		Revise:     func(context.Context, string, string, string) (bool, error) { return true, nil },
	})
	a, err := assistant.Init(g, assistant.FlowConfig{Provider: llm.NewProvider(), MaxOutputTokens: 4096}, assistant.Repos{Messages: evalMessages{}, Proposals: evalProposals{}, SavedViewLists: evalSavedViews{}}, all...)
	if err != nil {
		t.Fatal(err)
	}

	passed := map[string][2]int{}
	for _, c := range evalCases {
		t.Run(fmt.Sprintf("%s/%s", c.pack, c.say), func(t *testing.T) {
			view := &viewresolve.Resolved{View: &viewcontext.ViewContext{Version: viewcontext.Version, Screen: c.screen, Scope: c.scope, Focus: c.focus, Window: c.window}, Summary: "On " + string(c.screen)}
			if c.focus != nil {
				title := cmp.Or(c.title, "Villa Aurora brochure 2026")
				view.Focus = &viewresolve.Item{Kind: c.focus.Kind, ID: c.focus.ID, Title: title}
				view.Summary += `, showing "` + title + `"`
			}
			if len(c.selected) > 0 {
				sel := &viewcontext.Selection{Kind: viewcontext.KindPost}
				for i, title := range c.selected {
					id := fmt.Sprintf("p%d", i+1)
					sel.IDs = append(sel.IDs, id)
					view.Selection = append(view.Selection, viewresolve.Item{Kind: viewcontext.KindPost, ID: id, Title: title})
				}
				view.View.Selection, view.SelectionTotal = sel, len(c.selected)
			}
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
