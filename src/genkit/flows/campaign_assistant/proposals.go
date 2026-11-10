package campaign_assistant

import (
	"cmp"
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/ogen-app/ogen/src/genkit/flows/internal/assistantkit"
)

// defaultProposalThreshold is the largest batch generated without asking.
const defaultProposalThreshold = 3

func proposalThreshold(ctx context.Context) int {
	return cmp.Or(getRequestState(ctx).proposalThreshold, defaultProposalThreshold)
}

// Whole-campaign rewrites always wait for a yes.
var (
	contentPlanGate = assistantkit.Gate[RunContentPlanInput]{
		Precheck: func(ctx context.Context, in RunContentPlanInput) error {
			return needSources(ctx, "runContentPlan", in.Exploratory, func() any { in.Exploratory = true; return in })
		},
		Summary: func(in RunContentPlanInput) string {
			if in.Exploratory {
				return "Generate a new content plan for this campaign, without its sources"
			}
			return "Generate a new content plan for this campaign"
		},
	}
	redistributeGate = assistantkit.Gate[struct{}]{
		Precheck: func(ctx context.Context, _ struct{}) error { return checkCampaignDated(getRequestState(ctx).campaign) },
		Summary:  func(struct{}) string { return "Re-date the campaign's unpublished drafts across its timeline" },
	}
)

// Small batches run straight away; a batch above the threshold waits for a
// yes. Input that could never run is refused before anything is proposed.
var (
	generatePostsGate = assistantkit.Gate[GeneratePostsInput]{
		Precheck: func(ctx context.Context, in GeneratePostsInput) error {
			if err := precheckPosts(ctx, in.Platforms, in.Phase, in.WindowStart, in.WindowEnd, in.PostType, false); err != nil {
				return err
			}
			if err := needSources(ctx, "generatePosts", in.Exploratory, func() any { in.Exploratory = true; return in }); err != nil {
				return err
			}
			// Ideas fill no goal, so the plan is never over it.
			return nil
		},
		Needs: func(ctx context.Context, in GeneratePostsInput) bool {
			return generateTotal(ctx, in) > proposalThreshold(ctx)
		},
		Summary: func(in GeneratePostsInput) string {
			return fmt.Sprintf("Plan %d ideas for %s", max(in.Count, 1), strings.Join(in.Platforms, ", "))
		},
	}
	draftPostGate = assistantkit.Gate[DraftPostInput]{
		Precheck: func(ctx context.Context, in DraftPostInput) error {
			if err := precheckPosts(ctx, in.Platforms, "", in.PublishDate, in.PublishDate, in.PostType, true); err != nil {
				return err
			}
			return draftOverGoal(ctx, in)
		},
		Needs: func(ctx context.Context, in DraftPostInput) bool {
			return draftTotal(ctx, in) > proposalThreshold(ctx)
		},
		Summary: func(in DraftPostInput) string {
			return fmt.Sprintf("Write %d finished posts for %s from the research in this chat", max(in.Count, 1)*len(in.Platforms), strings.Join(in.Platforms, ", "))
		},
	}
)

// generateTotal is how many ideas a generatePosts call creates: the count,
// capped per call as the tool caps it.
func generateTotal(ctx context.Context, in GeneratePostsInput) int {
	n, _, _ := resolveGenerateCount(in.Count, getRequestState(ctx).maxGeneratePosts)
	return n
}

// draftPlatforms is how many distinct campaign platforms a draftPost call
// targets: the tool drafts once per platform, however often one is named.
func draftPlatforms(ctx context.Context, in DraftPostInput) int {
	ids, _, err := resolveTargetPlatforms(getRequestState(ctx).campaign, in.Platforms)
	if err != nil {
		return len(in.Platforms)
	}
	return len(ids)
}

// draftTotal is how many posts a draftPost call creates: the count on each
// distinct platform, with the total capped per call as the tool caps it.
func draftTotal(ctx context.Context, in DraftPostInput) int {
	maxN := getRequestState(ctx).draftCap()
	perPlatform, _, _ := resolveGenerateCount(in.Count, maxN)
	return min(perPlatform*draftPlatforms(ctx, in), maxN)
}

// precheckPosts runs the post tools' user-correctable checks — target
// platform, phase, publish window, post type — so a request that could never
// run is refused instead of proposed. anyWhenNone is the engine's rule for a
// type on a platform with none to choose from: drafting keeps it.
func precheckPosts(ctx context.Context, platforms []string, phase, windowStart, windowEnd, postType string, anyWhenNone bool) error {
	postType = strings.TrimSpace(postType)
	st := getRequestState(ctx)
	now := time.Now().UTC()
	ids, _, err := resolveTargetPlatforms(st.campaign, platforms)
	if err != nil {
		if offer := offerPlatforms(ctx, platforms, postType); offer != nil {
			return offer
		}
		return assistantkit.Refused(err.Error())
	}
	if _, _, err := resolvePhase(st.campaign, phase, now); err != nil {
		return assistantkit.Refused(err.Error())
	}
	if _, _, err := resolveWindow(windowStart, windowEnd, now); err != nil {
		return assistantkit.Refused(err.Error())
	}
	if reason, alts, bad := unsupportedPostType(st.campaign, ids, postType, anyWhenNone); bad {
		return assistantkit.Refused(reason, alts...)
	}
	return nil
}
