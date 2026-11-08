// Package packs holds the universal assistant's domain packs: the tools a
// turn loads by screen or scope, or through loadPack. Each pack is thin over
// the services REST uses, so a tool and its endpoint never disagree.
package packs

import (
	"context"
	"fmt"

	"github.com/firebase/genkit/go/ai"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/genkit/flows/assistant"
	"github.com/ogen-app/ogen/src/genkit/flows/consistency"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/assistantkit"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/usecase/activity/report"
	"github.com/ogen-app/ogen/src/usecase/analytics"
	"github.com/ogen-app/ogen/src/usecase/assetanalysis"
	"github.com/ogen-app/ogen/src/usecase/assistantundo"
	"github.com/ogen-app/ogen/src/usecase/ideas"
	"github.com/ogen-app/ogen/src/usecase/post_actions/compose"
	"github.com/ogen-app/ogen/src/usecase/urlimport"
)

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

	Assets    repository.AssetRepository
	Campaigns repository.CampaignRepository
	Tags      repository.TagRepository
	Chunks    repository.AssetChunksRepository
	Embedder  ai.Embedder
	Analysis  *assetanalysis.Service
	URLImport *urlimport.Service

	Posts     repository.PostRepository
	Links     repository.PostIdeaRepository
	Analytics *analytics.Service

	Brand          repository.BrandRepository
	Activity       *report.Service
	Settings       repository.SettingRepository
	SocialAccounts repository.SocialAccountRepository
	// ProfileID is the workspace's publishing profile, which its connected
	// accounts hang off.
	ProfileID  func(ctx context.Context) (string, error)
	Dismissals repository.AssistantDismissalRepository
	// CheckBrief and CheckPosts review a campaign's consistency; nil leaves
	// reviewCampaign out of the repair pack.
	CheckBrief func(ctx context.Context, campaignID string, onEvent consistency.OnEventFunc) (*consistency.BriefReview, error)
	CheckPosts func(ctx context.Context, req consistency.PostsCheckRequest, onEvent consistency.OnEventFunc) (*consistency.PostsReview, error)

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
	return assistantkit.Spec{Name: tool.Name(), Pack: pack, Scopes: allScopes, Tier: tier, Guide: guide, Tool: tool}
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
