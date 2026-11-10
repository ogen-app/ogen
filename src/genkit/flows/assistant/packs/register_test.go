package packs_test

import (
	"context"
	"slices"
	"testing"

	"github.com/firebase/genkit/go/ai"
	"github.com/firebase/genkit/go/genkit"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/genkit/flows/assistant"
	"github.com/ogen-app/ogen/src/genkit/flows/assistant/packs"
	"github.com/ogen-app/ogen/src/genkit/flows/campaign_assistant"
	"github.com/ogen-app/ogen/src/genkit/flows/consistency"
	"github.com/ogen-app/ogen/src/genkit/flows/post_assistant"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/usecase/activity/report"
	"github.com/ogen-app/ogen/src/usecase/analytics"
	"github.com/ogen-app/ogen/src/usecase/ideas"
	"github.com/ogen-app/ogen/src/usecase/post_actions/compose"
	"github.com/ogen-app/ogen/src/usecase/weeklyreport"
)

type proposals struct {
	repository.AssistantProposalRepository
}
type ideaRepo struct{ repository.IdeaRepository }

// Query and PlannedIdeas give the ideas pack a state to show, for the evals.
func (ideaRepo) Query(context.Context, models.IdeaQuery) (*models.IdeaQueryResult, error) {
	return &models.IdeaQueryResult{
		TotalUnfiltered: 40,
		Piles:           map[string]int{models.IdeaPileWaiting: 12, models.IdeaPileYes: 20, models.IdeaPileLater: 3, models.IdeaPileNo: 5},
		Facets: map[string][]models.AssetFacetCount{
			models.IdeaFacetOrigin: {{Value: models.IdeaOriginKeyAnalysis, Count: 9}, {Value: models.IdeaOriginKeyManual, Count: 3}},
		},
	}, nil
}

func (ideaRepo) PlannedIdeas(context.Context, string, int) ([]models.Idea, int, error) {
	return []models.Idea{{ID: "i7", Title: "Spa opening teaser", Plan: models.IdeaPlan{PlatformID: new("linkedin")}}}, 1, nil
}

type assetRepo struct{ repository.AssetRepository }
type campaignRepo struct{ repository.CampaignRepository }
type postRepo struct{ repository.PostRepository }
type linkRepo struct{ repository.PostIdeaRepository }
type brandRepo struct{ repository.BrandRepository }

// Every pack registers its tools on the one genkit instance the server runs,
// alongside the other flows; a tool name used twice panics at boot. This
// registers them all the way the runtime does.
func TestEveryPackRegistersTogether(t *testing.T) {
	g := genkit.Init(t.Context())
	campaign, err := campaign_assistant.NewPack(campaign_assistant.CampaignAssistantFlowConfig{}, campaign_assistant.CampaignAssistantRepos{})
	if err != nil {
		t.Fatal(err)
	}
	post, err := post_assistant.NewPack(g, post_assistant.PostAssistantFlowConfig{}, post_assistant.PostAssistantRepos{})
	if err != nil {
		t.Fatal(err)
	}
	d := packs.Deps{
		Campaign: &campaign, Post: &post,
		Ideas: ideas.New(ideaRepo{}, nil), IdeaRepo: ideaRepo{},
		Compose: compose.New(nil, ideaRepo{}, campaignRepo{}, nil, nil, nil),
		Assets:  assetRepo{}, Campaigns: campaignRepo{},
		Posts: postRepo{}, Links: linkRepo{}, Analytics: &analytics.Service{},
		Brand: brandRepo{}, Activity: report.New(nil, nil, nil),
		AutoPublish: evalAllowlist{}, ConnectPlatform: func(string) (string, bool) { return "LinkedIn", true },
		Digest: evalDigest{}, Versions: evalVersionRepo{}, PostLogs: evalPostLogs{},
		CheckBrief: func(context.Context, string, consistency.OnEventFunc) (*consistency.BriefReview, error) {
			return nil, nil
		},
		CheckPosts: func(context.Context, consistency.PostsCheckRequest, consistency.OnEventFunc) (*consistency.PostsReview, error) {
			return nil, nil
		},
	}
	all := packs.All(d)
	if len(all) != 8 {
		t.Fatalf("packs = %d, want all eight", len(all))
	}
	if _, err := assistant.Init(g, assistant.FlowConfig{}, assistant.Repos{Proposals: proposals{}}, all...); err != nil {
		t.Fatal(err)
	}
	checkNotStrict(t, all)
	checkTools(t, all, "workspace", "activityReport", "workspaceSetup", "weeklyDigest", "emailWeeklyDigest", "postHistory", "setAutoPublish", "openConnect")
	checkTools(t, all, "ideas", "findIdeas", "setIdeaVerdict", "setIdeaVerdicts", "moveIdeas", "deleteIdeas", "compileIdeas", "draftIdeas",
		"getIdea", "ideasInCommon", "editIdea", "similarIdeas", "findDuplicateIdeas", "mergeIdeas")
}

// A pack has every tool once its services are set, in order.
func checkTools(t *testing.T, all []assistant.Pack, pack string, want ...string) {
	t.Helper()
	i := slices.IndexFunc(all, func(p assistant.Pack) bool { return p.Name == pack })
	specs, err := all[i].Define(genkit.Init(t.Context()))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, s := range specs {
		names = append(names, s.Name)
	}
	if !slices.Equal(names, want) {
		t.Fatalf("%s tools = %v, want %v", pack, names, want)
	}
}

// Anthropic refuses a request with more than 20 strict tools, and a turn's
// packs carry more than that between them; only the core tools stay strict.
func checkNotStrict(t *testing.T, all []assistant.Pack) {
	t.Helper()
	n := 0
	defer func() {
		if n <= 20 {
			t.Errorf("checked %d pack tools; want them all (more than 20)", n)
		}
	}()
	for _, p := range all {
		specs, err := p.Define(genkit.Init(t.Context()))
		if err != nil {
			t.Fatal(err)
		}
		n += len(specs)
		for _, s := range specs {
			tool, ok := s.Tool.(ai.Tool)
			if !ok {
				t.Fatalf("%s/%s: not a genkit tool", p.Name, s.Name)
			}
			if strict, _ := tool.Definition().Metadata["strict"].(bool); strict || tool.Definition().Metadata["strict"] == nil {
				t.Errorf("%s/%s is strict; pack tools must use ai.WithStrictSchema(false)", p.Name, s.Name)
			}
		}
	}
}

// evalAllowlist, evalDigest, evalPostLogs and evalVersionRepo let the
// workspace pack offer its settings, report and history tools, here and in
// the evals.
type evalAllowlist struct {
	repository.AutoPublishAllowlistRepository
}

type evalPostLogs struct{ repository.PostLogRepository }

type evalDigest struct{}

func (evalDigest) Build(context.Context, string) (*weeklyreport.Week, error) { return nil, nil }
func (evalDigest) CanEmail(context.Context, string) error                    { return nil }
func (evalDigest) EmailTo(context.Context, string, string) (*weeklyreport.Week, error) {
	return nil, nil
}

type evalVersionRepo struct {
	repository.PostVersionRepository
}
