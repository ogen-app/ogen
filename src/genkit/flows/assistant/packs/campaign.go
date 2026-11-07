package packs

import (
	"context"
	"errors"
	"strings"

	"github.com/firebase/genkit/go/genkit"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/genkit/flows/assistant"
	"github.com/ogen-app/ogen/src/genkit/flows/campaign_assistant"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/assistantkit"
)

const campaignPackName = "campaign"

// campaignPack hands campaign work to the campaign assistant, which keeps its
// own tools, state snapshot, proposals and undo: planning, the brief, dates,
// platforms, drafting, redistributing and consistency checks.
func campaignPack(d Deps) assistant.Pack {
	return assistant.Pack{
		Name:    campaignPackName,
		Summary: "plan a campaign's content, edit its brief, dates and platforms, draft and redistribute its posts, and check its consistency",
		Scopes:  []models.AssistantScopeKind{models.AssistantScopeCampaign},
		Define: func(g *genkit.Genkit) ([]assistantkit.Spec, error) {
			tool := genkit.DefineTool(g, "workOnCampaign",
				"Hands a request about one campaign to the campaign assistant, which plans its content, edits its brief, dates and platforms, drafts and redistributes its posts, and checks its consistency, and returns what it did.",
				assistantkit.Wrap(func(ctx context.Context, in WorkOnCampaignInput) (*WorkOnCampaignOutput, error) {
					return workOnCampaign(ctx, d, in)
				}))
			return []assistantkit.Spec{spec(campaignPackName, tool, assistantkit.TierRead,
				`Call this for anything done to a campaign: plan its content, add ideas for a platform, improve or check its brief, move its dates, add platforms, draft posts from research, redistribute or check its posts. Pass the user's request as instruction, in their words, with any specifics you resolved (dates, platforms, counts). It defaults to this thread's campaign; pass campaignId for another. Set useSelectedAssets when the user wants only the assets they selected used. Its reply is the campaign assistant's: relay it, and treat an offer it made as one waiting for the user.`)}, nil
		},
	}
}

// WorkOnCampaignInput is a request for the campaign assistant.
type WorkOnCampaignInput struct {
	Instruction string `json:"instruction"                 jsonschema:"description=The request, in the user's words, with the specifics you resolved"`
	CampaignID  string `json:"campaignId,omitempty"        jsonschema:"description=The campaign; defaults to this thread's"`
	// AssetIDs limit the sources ("use only these").
	AssetIDs []string `json:"assetIds,omitempty" jsonschema:"description=Only these assets are used as sources; ids from getSelection"`
}

// WorkOnCampaignOutput is what the campaign assistant did.
type WorkOnCampaignOutput struct {
	CampaignID string `json:"campaignId"`
	// Explanation is its reply, Action what it did.
	Explanation string `json:"explanation"`
	Action      string `json:"action"`
	// Result carries its tool results (the plan, the brief, the dates …).
	Result *campaign_assistant.CampaignAssistantResponse `json:"result"`
}

func workOnCampaign(ctx context.Context, d Deps, in WorkOnCampaignInput) (*WorkOnCampaignOutput, error) {
	id := strings.TrimSpace(in.CampaignID)
	if id == "" {
		id = assistant.CampaignID(ctx)
	}
	if id == "" {
		return nil, assistantkit.Refused("this isn't a campaign thread; ask the user which campaign, then pass its id")
	}
	if strings.TrimSpace(in.Instruction) == "" {
		return nil, assistantkit.Refused("say what to do with the campaign")
	}
	resp, err := d.Campaign(ctx, campaign_assistant.CampaignAssistantRequest{
		CampaignID: id, Instruction: in.Instruction, SourceAssetIDs: in.AssetIDs,
	}, func(kind campaign_assistant.SSEEventKind, data any) {
		// Its progress (plans streaming in, offers, results) reaches the
		// client; its own reply is this tool's result.
		assistant.Emit(ctx, string(kind), data)
	})
	if ve, ok := errors.AsType[*campaign_assistant.ValidationError](err); ok {
		return nil, assistantkit.Refused(ve.Msg)
	}
	if ae, ok := errors.AsType[*campaign_assistant.AIError](err); ok {
		return nil, assistantkit.Refused("the campaign assistant couldn't finish that: " + ae.Msg)
	}
	if err != nil {
		return nil, err
	}
	TouchCampaignWork(ctx, id, resp)
	return &WorkOnCampaignOutput{CampaignID: id, Explanation: resp.Explanation, Action: resp.Action, Result: resp}, nil
}

// TouchCampaignWork records what the campaign assistant changed, for the
// client to refresh.
func TouchCampaignWork(ctx context.Context, campaignID string, resp *campaign_assistant.CampaignAssistantResponse) {
	if resp.ContentPlan != nil || resp.GeneratedPosts != nil {
		assistantkit.Touch(ctx, "idea", "")
	}
	if resp.DraftedPosts != nil || resp.Redistribute != nil {
		assistantkit.Touch(ctx, "post", "")
	}
	if resp.Brief != nil || resp.Dates != nil || resp.Platforms != nil {
		assistantkit.Touch(ctx, "campaign", campaignID)
	}
}
