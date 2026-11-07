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
			return max(in.Count, 1) > proposalThreshold(ctx)
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
			var fill func(int) any
			if len(in.Platforms) == 1 {
				fill = func(n int) any { in.Count = n; return in }
			}
			return overGoal(ctx, "draftPost", max(in.Count, 1)*len(in.Platforms), in, fill)
		},
		Needs: func(ctx context.Context, in DraftPostInput) bool {
			return max(in.Count, 1)*len(in.Platforms) > proposalThreshold(ctx)
		},
		Summary: func(in DraftPostInput) string {
			return fmt.Sprintf("Write %d finished posts for %s from the research in this chat", max(in.Count, 1)*len(in.Platforms), strings.Join(in.Platforms, ", "))
		},
	}
)

// overGoal returns a conflict when adding count posts would take the
// campaign past its goal. The options are to fill the goal exactly (when
// there is room and fill can rebuild the call for fewer posts) or to add
// them all anyway; each is the tool call picking it runs.
func overGoal(ctx context.Context, tool string, count int, args any, fill func(int) any) error {
	g := getRequestState(ctx).snapshot.Goal
	if g == nil || count <= g.Room {
		return nil
	}
	reason := fmt.Sprintf("That would add %d posts, but the goal is %d and the campaign already has %d, so there is room for %d more.",
		count, g.Target, g.Planned, g.Room)
	var opts []assistantkit.Option
	if g.Room > 0 && fill != nil {
		opts = append(opts, assistantkit.Option{ID: "fill", Title: fmt.Sprintf("Add %d to reach the goal", g.Room), Tool: tool, Args: fill(g.Room)})
	}
	opts = append(opts, assistantkit.Option{ID: "anyway", Title: fmt.Sprintf("Add all %d anyway, past the goal", count), Tool: tool, Args: args})
	return assistantkit.Conflicted(reason, opts...)
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
