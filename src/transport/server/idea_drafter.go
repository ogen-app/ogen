package server

import (
	"context"

	"github.com/ogen-app/ogen/src/genkit/flows/draft_post"
	"github.com/ogen-app/ogen/src/usecase/post_actions/compose"
)

// ideaDrafter writes posts from ideas through the draft_post flow, which
// writes finished copy and saves each post as a draft with its source.
func ideaDrafter(gk *genkitRuntime) compose.Drafter {
	return func(ctx context.Context, req compose.DraftRequest) ([]compose.Written, []string, error) {
		resp, err := gk.DraftPost(ctx, draft_post.DraftPostRequest{
			CampaignID:     req.CampaignID,
			PlatformID:     req.PlatformID,
			PostType:       req.PostType,
			Count:          req.Count,
			SourceMaterial: req.SourceMaterial,
			Instruction:    req.Instruction,
			WindowStart:    req.WindowStart,
			WindowEnd:      req.WindowEnd,
			PhaseID:        req.PhaseID,
			UsedAssetIDs:   req.UsedAssetIDs,
			CreatedBy:      req.CreatedBy,
		}, nil)
		if err != nil {
			return nil, nil, err
		}
		out := make([]compose.Written, 0, len(resp.Posts))
		for _, p := range resp.Posts {
			out = append(out, compose.Written{PostID: p.PostID, Title: p.Title, PlatformID: p.PlatformID, PublishDate: p.PublishDate})
		}
		return out, resp.Warnings, nil
	}
}
