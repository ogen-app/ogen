// Package packs holds the universal assistant's domain packs: the tools a
// turn loads by screen or scope, or through loadPack. Each pack is thin over
// the services REST uses, so a tool and its endpoint never disagree.
package packs

import (
	"context"
	"fmt"

	"github.com/firebase/genkit/go/ai"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/genkit/flows/asset_compare"
	"github.com/ogen-app/ogen/src/genkit/flows/assistant"
	"github.com/ogen-app/ogen/src/genkit/flows/brand_check"
	"github.com/ogen-app/ogen/src/genkit/flows/consistency"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/assistantkit"
	"github.com/ogen-app/ogen/src/genkit/flows/more_like_these"
	"github.com/ogen-app/ogen/src/infra/eventhub"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/usecase/activity/report"
	"github.com/ogen-app/ogen/src/usecase/analytics"
	"github.com/ogen-app/ogen/src/usecase/assetanalysis"
	"github.com/ogen-app/ogen/src/usecase/assistantundo"
	"github.com/ogen-app/ogen/src/usecase/campaign_actions/create"
	"github.com/ogen-app/ogen/src/usecase/ideas"
	"github.com/ogen-app/ogen/src/usecase/post_actions/compose"
	"github.com/ogen-app/ogen/src/usecase/series"
	"github.com/ogen-app/ogen/src/usecase/urlimport"
)

// Revised is what a rewrite wrote to a post, as committed, and whether it was
// saved as a version: false for a rewrite on the post whose version couldn't
// be saved.
type Revised struct {
	Content   string
	Status    models.PostStatus
	Versioned bool
}

// Deps are the services the packs work through. A pack whose service is nil
// is left out.
type Deps struct {
	// Campaign and Post are the campaign and post packs, built with their
	// own services; nil leaves them out, and the repair pack's fixes that
	// run through the campaign's tools with them.
	Campaign *assistant.Pack
	Post     *assistant.Pack

	Ideas    *ideas.Service
	IdeaRepo repository.IdeaRepository
	Compose  *compose.Service
	// Titles tells which ideas, assets and posts still exist, for an offer
	// confirmed after some went; nil keeps the offer as made.
	Titles repository.EntityTitleRepository

	Assets repository.AssetRepository
	// Files gives the assets' length, pages or duration, for their cards;
	// nil leaves it out.
	Files     repository.AssetFileRepository
	Campaigns repository.CampaignRepository
	Tags      repository.TagRepository
	Chunks    repository.AssetChunksRepository
	Embedder  ai.Embedder
	Analysis  *assetanalysis.Service
	// Analyses reads the lenses: the bank's state and what an analysis
	// found; nil leaves both out.
	Analyses repository.AssetAnalysisRepository
	// Compare reads assets together for compareAssets; nil leaves it out.
	Compare asset_compare.Run
	// CreateCampaign and CampaignTypes set up a campaign from assets; nil
	// leaves createCampaignFromAssets out.
	CreateCampaign *create.Service
	CampaignTypes  repository.CampaignTypeRepository
	URLImport      *urlimport.Service

	Posts repository.PostRepository
	// Remove deletes and withdraws posts for the repair pack's fixes; nil
	// leaves deletePosts out, and scheduled posts to unschedule by hand.
	Remove PostRemover
	// Attachments counts a post's media for the repair pack's delete card
	// and conversion rows; nil leaves the counts out.
	Attachments repository.PostAttachmentRepository
	Links       repository.PostIdeaRepository
	Analytics   *analytics.Service

	Brand repository.BrandRepository
	// Series keeps the series and the campaigns' runs of them; nil leaves
	// the series tools out.
	Series *series.Service
	// Distill reads the brand off a website or assets; nil leaves the
	// brand reading tools out.
	Distill        Distiller
	Activity       *report.Service
	Settings       repository.SettingRepository
	SocialAccounts repository.SocialAccountRepository
	// ProfileID is the workspace's publishing profile, which its connected
	// accounts hang off.
	ProfileID func(ctx context.Context) (string, error)
	// AutoPublish is the platforms whose scheduled posts go out on their
	// own; nil leaves it out of workspaceSetup, and setAutoPublish out.
	AutoPublish repository.AutoPublishAllowlistRepository
	// ConnectPlatform names a platform the workspace can connect; nil
	// leaves openConnect and setAutoPublish out.
	ConnectPlatform PlatformLookup
	// Digest builds and emails the weekly report on request; nil leaves
	// weeklyDigest and emailWeeklyDigest out.
	Digest Digester
	// Versions, PostLogs and Users read a post's change history; nil
	// Versions or PostLogs leaves postHistory out, nil Users names no one.
	Versions   repository.PostVersionRepository
	PostLogs   repository.PostLogRepository
	Users      repository.UserRepository
	Dismissals repository.AssistantDismissalRepository
	// CheckBrief and CheckPosts review a campaign's consistency; nil leaves
	// reviewCampaign out of the repair pack.
	CheckBrief func(ctx context.Context, campaignID string, onEvent consistency.OnEventFunc) (*consistency.BriefReview, error)
	CheckPosts func(ctx context.Context, req consistency.PostsCheckRequest, onEvent consistency.OnEventFunc) (*consistency.PostsReview, error)
	// Revise rewrites a post by an instruction with the post pack's writer,
	// saving a version, and returns what it wrote. nil leaves revisePost out
	// of the repair pack.
	Revise func(ctx context.Context, postID, instruction, note string) (Revised, error)
	// CheckBrand checks posts against their voice and guardrails; nil leaves
	// checkBrand out of the repair pack.
	CheckBrand brand_check.Run
	// MoreLike writes ideas like posts that performed well; nil leaves
	// moreLikeThese out of the analytics pack.
	MoreLike more_like_these.Run

	// Hub announces committed changes to the workspace's other sessions;
	// nil publishes nothing.
	Hub eventhub.Hub

	// Actions logs reversible changes for undo; nil leaves them without one.
	Actions repository.AssistantActionRepository
	// ProposalThreshold is N: a bulk change or a generation above it waits
	// for the user's yes.
	ProposalThreshold int
	// DraftMax caps the posts one draft request writes.
	DraftMax int
}

// All returns the packs whose services are set, in a fixed order: the order
// their tools reach the model in.
func All(d Deps) []assistant.Pack {
	var out []assistant.Pack
	if d.Campaign != nil {
		out = append(out, *d.Campaign)
	}
	if d.Post != nil {
		out = append(out, *d.Post)
	}
	if d.Ideas != nil && d.IdeaRepo != nil && d.Compose != nil {
		out = append(out, ideasPack(d))
	}
	if d.Assets != nil && d.Campaigns != nil {
		out = append(out, assetsPack(d))
	}
	if d.Posts != nil && d.Campaigns != nil {
		out = append(out, repairPack(d))
	}
	if d.Analytics != nil && d.Posts != nil {
		out = append(out, analyticsPack(d))
	}
	if d.Brand != nil {
		out = append(out, foundationPack(d))
	}
	if d.Activity != nil {
		out = append(out, workspacePack(d))
	}
	return out
}

var allScopes = []assistantkit.Scope{assistantkit.ScopeWorkspace, assistantkit.ScopeCampaign, assistantkit.ScopePost}

func spec(pack string, tool ai.ToolRef, tier assistantkit.Tier, guide string) assistantkit.Spec {
	return assistantkit.Spec{Name: tool.Name(), Pack: pack, Scopes: allScopes, Tier: tier, Guide: guide, Tool: tool,
		Generates: generatingTools[tool.Name()]}
}

// generatingTools write posts or ideas, so each says what they are grounded
// in.
var generatingTools = map[string]bool{
	"draftIdeas": true, "compileIdeas": true, "compareAssets": true,
	"distillMoreIdeas": true, "moreLikeThese": true, "revisePost": true,
}

func actor(ctx context.Context) string {
	id, _ := logging.UserIDFrom(ctx)
	return id
}

// recordUndo logs a reversible change and returns its undo id; a change that
// could not be logged is done but not undoable, so the id is then empty.
func recordUndo(ctx context.Context, d Deps, kind models.AssistantActionKind, entityID string, before, after models.JSONMap) string {
	if d.Actions == nil {
		return ""
	}
	scope, _ := assistant.Scope(ctx)
	id, err := assistantundo.Record(ctx, d.Actions, &models.AssistantAction{
		ScopeKind: scope.Kind, ScopeID: scope.ID, Kind: kind, EntityID: entityID,
		Before: before, After: after, CreatedBy: actor(ctx),
	})
	if err != nil {
		return ""
	}
	return id
}

// overN is the gate rule for bulk work: more than N items waits for a yes.
func overN(d Deps, n int) bool {
	return n > max(d.ProposalThreshold, 1)
}

func plural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}
