package campaign_assistant

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/assistantkit"
	"github.com/ogen-app/ogen/src/kernel/logging"
)

// actionProposed labels a turn whose only result is work waiting for the
// user's confirmation.
const actionProposed = "proposed"

// proposalTTL is how long an offer can be accepted. After that the state it
// was planned against may have moved on, so the user asks again.
const proposalTTL = 24 * time.Hour

// defaultProposalThreshold is the largest batch generated without asking.
const defaultProposalThreshold = 3

// campaignSink stores held-back calls against the turn's campaign.
type campaignSink struct{ st *requestState }

// Propose stores p as an offer on the turn's campaign and returns its id. A
// call identical to one already proposed this turn gets that offer's id, so
// the user sees one card for it.
func (s campaignSink) Propose(ctx context.Context, p assistantkit.Proposal) (string, error) {
	st := s.st
	if st.repos.Proposals == nil {
		return "", assistantkit.ErrNoProposalSink
	}
	args, err := toJSONMap(p.Args)
	if err != nil {
		return "", fmt.Errorf("encode %s proposal args: %w", p.Tool, err)
	}
	// Map keys marshal sorted, so equal args give equal keys.
	key, err := json.Marshal(args)
	if err != nil {
		return "", fmt.Errorf("encode %s proposal args: %w", p.Tool, err)
	}
	dedupKey := p.Tool + "\x00" + string(key)
	// Held across the store so parallel identical calls can't both create one.
	st.proposeMu.Lock()
	defer st.proposeMu.Unlock()
	if id, ok := st.proposedKeys[dedupKey]; ok {
		return id, nil
	}
	options, choices, err := proposalOptions(p.Options)
	if err != nil {
		return "", fmt.Errorf("encode %s proposal options: %w", p.Tool, err)
	}
	id, err := models.NewID()
	if err != nil {
		return "", err
	}
	by, _ := logging.UserIDFrom(ctx)
	now := time.Now().UTC()
	row := &models.AssistantProposal{
		ID: id, ScopeKind: models.AssistantScopeCampaign, ScopeID: st.campaignID,
		Tool: p.Tool, Args: args, Summary: p.Summary, Options: options, CreatedBy: by, ExpiresAt: now.Add(proposalTTL),
	}
	if err := st.repos.Proposals.Create(ctx, row); err != nil {
		return "", fmt.Errorf("store %s proposal: %w", p.Tool, err)
	}
	st.mu.Lock()
	st.proposed = append(st.proposed, ProposalCreatedEventPayload{
		ID: id, Tool: p.Tool, Summary: p.Summary, ExpiresAt: row.ExpiresAt.Format(time.RFC3339), Options: choices,
	})
	st.mu.Unlock()
	if st.proposedKeys == nil {
		st.proposedKeys = map[string]string{}
	}
	st.proposedKeys[dedupKey] = id
	return id, nil
}

// proposalOptions converts a proposal's options for storage, and for the
// card, which shows only their ids and titles.
func proposalOptions(opts []assistantkit.Option) ([]models.ProposalOption, []ProposalChoice, error) {
	if len(opts) == 0 {
		return nil, nil, nil
	}
	stored := make([]models.ProposalOption, len(opts))
	choices := make([]ProposalChoice, len(opts))
	for i, o := range opts {
		args, err := toJSONMap(o.Args)
		if err != nil {
			return nil, nil, err
		}
		stored[i] = models.ProposalOption{ID: o.ID, Title: o.Title, Description: o.Description, Tool: o.Tool, Args: args}
		choices[i] = ProposalChoice{ID: o.ID, Title: o.Title, Description: o.Description}
	}
	return stored, choices, nil
}

// toJSONMap converts a tool input into the JSON object stored with a proposal.
func toJSONMap(v any) (models.JSONMap, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	m := models.JSONMap{}
	if string(b) == "null" {
		return m, nil
	}
	return m, json.Unmarshal(b, &m)
}

// proposalOutcomes reports this turn's held-back calls. They are read-only, so
// a turn that also wrote something is labelled by the write.
func proposalOutcomes(proposed []ProposalCreatedEventPayload) []outcome {
	out := make([]outcome, 0, len(proposed))
	for _, p := range proposed {
		out = append(out, outcome{
			action:      actionProposed,
			explanation: fmt.Sprintf("I've prepared this for you to confirm: %s.", p.Summary),
			attach:      func(resp *CampaignAssistantResponse) { resp.Proposals = append(resp.Proposals, p) },
			event:       SSEEventProposalCreated,
			payload:     p,
			readOnly:    true,
		})
	}
	return out
}

// openProposals lists the campaign's offers still waiting for an answer. A
// failed read only costs matching a "yes" to an offer, so it is logged, not
// fatal.
func (t *turn) openProposals(ctx context.Context) []openProposal {
	if t.repos.Proposals == nil {
		return nil
	}
	rows, err := t.repos.Proposals.ListOpen(ctx, models.AssistantScopeCampaign, t.req.CampaignID, time.Now().UTC())
	if err != nil {
		slog.WarnContext(ctx, "list open proposals failed", logging.AttrComponent, logComponent,
			"campaign_id", t.req.CampaignID, logging.AttrError, err)
		return nil
	}
	out := make([]openProposal, len(rows))
	for i, r := range rows {
		out[i] = openProposal{ID: r.ID, Summary: r.Summary}
	}
	return out
}

// proposalThreshold is the turn's largest batch run without asking.
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
			return overGoal(ctx, "generatePosts", max(in.Count, 1), in, func(n int) any { in.Count = n; return in })
		},
		Needs: func(ctx context.Context, in GeneratePostsInput) bool {
			return generateTotal(ctx, in) > proposalThreshold(ctx)
		},
		Summary: func(in GeneratePostsInput) string {
			return fmt.Sprintf("Add %d draft posts for %s", max(in.Count, 1), strings.Join(in.Platforms, ", "))
		},
	}
	draftPostGate = assistantkit.Gate[DraftPostInput]{
		Precheck: func(ctx context.Context, in DraftPostInput) error {
			if err := precheckPosts(ctx, in.Platforms, "", in.PublishDate, in.PublishDate, in.PostType, true); err != nil {
				return err
			}
			var fill func(int) any
			if draftPlatforms(ctx, in) == 1 {
				fill = func(n int) any { in.Count = n; return in }
			}
			return overGoal(ctx, "draftPost", draftTotal(ctx, in), in, fill)
		},
		Needs: func(ctx context.Context, in DraftPostInput) bool {
			return draftTotal(ctx, in) > proposalThreshold(ctx)
		},
		Summary: func(in DraftPostInput) string {
			return fmt.Sprintf("Write %d finished posts for %s from the research in this chat", max(in.Count, 1)*len(in.Platforms), strings.Join(in.Platforms, ", "))
		},
	}
)

// generateTotal is how many posts a generatePosts call creates: the count,
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
