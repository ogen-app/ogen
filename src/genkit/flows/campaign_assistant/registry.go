package campaign_assistant

import (
	"fmt"

	"github.com/firebase/genkit/go/ai"

	"github.com/ogen-app/ogen/src/genkit/flows/internal/assistantkit"
)

// campaignPack is the pack every campaign assistant tool belongs to.
const campaignPack = "campaign"

var campaignScope = []assistantkit.Scope{assistantkit.ScopeCampaign}

// ran is the rule for an action bound to tool: the model answers with it
// when it called that tool this turn.
func ran(tool string) string { return fmt.Sprintf("you called %s this turn.", tool) }

// heavyTools run an expensive sub-flow; at most one runs per turn.
var heavyTools = map[string]bool{
	"runContentPlan": true, "enrichBrief": true, "generatePosts": true,
	"draftPost": true, "checkBrief": true, "checkPostsConsistency": true,
	// They draft once their change is made.
	"setCampaignGoal": true, "replaceWeakestDrafts": true,
}

// reviewTools are the heavy tools that only read; every other heavy tool
// writes posts, ideas or the brief, and says what they are grounded in.
var reviewTools = map[string]bool{"checkBrief": true, "checkPostsConsistency": true}

// newToolRegistry describes the campaign tools for the prompt and the runner.
// The order is the order the model is given the tools in, so it must not
// change between the planner call and the prewarm.
func newToolRegistry(t *toolSet) (*assistantkit.Registry, error) {
	spec := func(tool ai.ToolRef, tier assistantkit.Tier, action, guide string) assistantkit.Spec {
		s := assistantkit.Spec{Name: tool.Name(), Pack: campaignPack, Scopes: campaignScope, Tier: tier, Guide: guide, Tool: tool,
			Generates: heavyTools[tool.Name()] && !reviewTools[tool.Name()]}
		if heavyTools[tool.Name()] {
			s.Prepare = holdHeavySlot
		}
		if action != "" {
			s.Action = &assistantkit.Action{Name: action, Rule: ran(tool.Name())}
		}
		return s
	}
	return assistantkit.NewRegistry(
		spec(t.runContentPlan, assistantkit.TierProposal, actionContentPlan,
			`Call this when the user asks to generate, create, build, or regenerate a content plan (e.g. "generate a content plan", "make me some posts for this campaign"). It saves the plan as ideas — each with a thesis and a suggested platform, post type, phase and date — not as posts: say so, and tell the user to pick the ideas worth making. Takes no arguments.`),
		spec(t.enrichBrief, assistantkit.TierReversible, actionBriefEnriched,
			"Call this when the user asks to enrich, improve, refine, sharpen, or rewrite the brief (e.g. \"enrich the brief\", \"improve the brief\", \"make the brief more B2B\"). Pass the user's steering as the `instruction` argument when they give any. The enriched brief is saved to the campaign automatically — you do not need to ask for confirmation."),
		spec(t.listCampaignPosts, assistantkit.TierRead, "",
			"Call this when you need to know what posts already exist in the campaign to answer a question. Takes no arguments."),
		spec(t.getCampaignOverview, assistantkit.TierRead, "",
			`Call this for a quick overview or status of the campaign, or for how content is distributed — e.g. "give me an overview", "how is content spread across phases", "which phase has the least content", "how many posts per platform / status / content type". It returns the phases with per-phase post counts and the distribution by status, platform, and content type. Takes no arguments.`),
		spec(t.generatePosts, assistantkit.TierProposal, actionPostsGenerated, guideGeneratePosts),
		spec(t.draftPost, assistantkit.TierProposal, actionPostDrafted, guideDraftPost),
		spec(t.setCampaignDates, assistantkit.TierReversible, actionDatesUpdated,
			`Call this when the user asks to move, shift, extend, or shorten the campaign's start/end dates — e.g. "move the campaign end to the beginning of July", "push the start to next Monday". Resolve relative phrasing into ISO YYYY-MM-DD using today's date below; pass only the field(s) that change. The change is saved automatically. When the result reports posts now outside the new range, MENTION the count and OFFER to redistribute them — but do NOT call redistributePosts in the same turn unless the user also asked to.`),
		spec(t.redistributePosts, assistantkit.TierProposal, actionPostsRedistributed,
			`Call this when the user asks to redistribute, re-spread, rebalance, or re-schedule the drafts / unpublished posts — e.g. "redistribute the drafts". It evenly re-dates only draft and ready-for-publish posts across the timeline, phase by phase; it never moves already-scheduled or published posts. Takes no arguments.`),
		spec(t.checkBrief, assistantkit.TierRead, actionBriefReviewed,
			`Call this when the user asks to check, review, or critique the brief, or whether the brief is consistent/complete — e.g. "is the brief consistent?", "review the brief". It is READ-ONLY — it returns findings with suggestions but does NOT change the brief. Takes no arguments. When it reports issues, summarise them and OFFER to improve the brief with enrichBrief — but do NOT call enrichBrief in the same turn unless the user also asks you to.`),
		spec(t.checkPostsConsistency, assistantkit.TierRead, actionPostsReviewed,
			"Call this when the user asks whether the posts match, follow, or are consistent with the brief — e.g. \"do the posts follow the brief?\", \"check the posts against the brief\". It is READ-ONLY — it returns per-post findings but does NOT change any post. It reviews only non-published posts and is capped; when the result reports a cap, MENTION that only the first N were checked. Pass `max` only if the user asks for a specific number."),
		spec(t.updateCampaignPlatforms, assistantkit.TierProposal, actionPlatformsUpdated,
			`Call this when the user asks to add a platform to the campaign, or a post type to a platform it already targets — e.g. "add Threads", "also do LinkedIn carousels" — or accepts your offer to add one; and when they ask to remove one — "drop Threads", "no more LinkedIn carousels" (remove, with postTypes to drop only those types). Pass each platform by name, with postTypes only when the user names specific types. It is proposed for the user to confirm before anything changes. A removal leaves the posts on it as they are: say how many are left (postsLeft), that scheduled ones still publish, and that the calendar check offers what to do with them. It can't remove the campaign's last platform.`),
		spec(t.setCampaignGoal, assistantkit.TierProposal, actionGoalUpdated,
			`Call this when the user asks to set, raise or lower the campaign's goal — "make it 5 posts a week", "raise the goal to 20". Pass perPeriod, the posts per goal period. It is proposed for the user to confirm, and can be undone. A budget guard's "raise the goal" option is already this offer.`),
		spec(t.replaceWeakestDrafts, assistantkit.TierProposal, actionPostDrafted,
			`Never call this yourself: it is what a budget guard's "replace the weakest drafts" option runs once confirmed.`),
		spec(t.askCampaignAssets, assistantkit.TierRead, "",
			"Call this when the user asks a question about the campaign's attached assets (uploaded PDFs, markdown, etc.) — e.g. \"what does the pricing PDF say about enterprise tiers?\", \"summarise the attached brief\". It is READ-ONLY: it returns the most relevant asset excerpts (with title + page) for you to answer from; it does NOT generate posts. Answer from the returned excerpts and cite the asset title(s) you used. If the result reports `available:false` or has no excerpts, tell the user you couldn't search or find matching asset content — do not invent an answer."),
	)
}

const guideGeneratePosts = "Call this to plan one or more NEW ideas targeted at a specific platform, phase, and timeframe (saved as ideas, not posts; describe them as ideas) — e.g. \"add a few Threads posts in the current phase for the upcoming weeks\", \"generate 5 LinkedIn articles for the launch phase\", \"write 1 post for Threads\". This is different from runContentPlan (which regenerates the whole plan). Only platforms the campaign already targets are allowed; if the user names a different platform, STILL call the tool with it: it comes back as an offer to add the platform (a conflict with an add option), which is what the user's \"yes\" then confirms — never offer to add a platform in words alone. Pass phase:\"current\" (or omit) for the current phase. Resolve the requested timeframe into windowStart/windowEnd (ISO YYYY-MM-DD) using today's date shown below; omit both to default to the next two weeks; for a single specific date (\"for Aug 20\"), set BOTH windowStart and windowEnd to that same date so the post lands exactly there. The window must be today or later: if the user asks for a date BEFORE today (shown below), do NOT call generatePosts — tell them that date has already passed and offer to use today or a future date. ALWAYS set count: the exact number the user names (\"write 1 post\" -> count 1, \"generate 5 articles\" -> count 5), or 3 for a vague \"a few\"/\"some\". If you leave count out, only 1 post is created — so never omit it for a multi-post request. Count is capped per call. If generatePosts comes back refused (e.g. the date is in the past, the platform isn't a target, or the post type isn't allowed), do NOT report success — relay the reason and suggest a valid alternative (today or a later date, one of the campaign's target platforms, or one of the listed alternatives)."

const guideDraftPost = "Call this when the user asks to turn the research just discussed into an actual post — e.g. \"create a post with this info for tomorrow, for LinkedIn\", \"draft 3 LinkedIn posts from that research\", \"write a post based on what you just told me\". It rewrites the research into finished, ready-to-publish copy (a full post body, NOT a bullet outline) and saves it as a draft. This is DIFFERENT from generatePosts, which produces terse bullet-point draft-theses for bulk planning: use draftPost when the user wants a real, finished post built from a specific piece of research or pasted material, and generatePosts when they want to bulk-add planning drafts for a platform/timeframe. Only platforms the campaign already targets are allowed; if the user names a different one, STILL call the tool with it: it comes back as an offer to add the platform, which the user's \"yes\" then confirms — never offer to add a platform in words alone. Resolve any date into ISO YYYY-MM-DD against today's date below and pass it as publishDate (omit for the next two weeks); the date must be today or later. A loose date ('next week', 'soon') is the first day of that range — never ask the user for an exact day. The research can be your own earlier answer about the topic, not only an asset search; don't hold the draft back to research assets first. Set count to the number the user asks for. Do NOT paste the research into sourceMaterial — it is loaded automatically from this conversation; pass sourceMaterial only when the user gave brand-new material in this same message. If there's no prior research in the chat, the tool refuses with that reason — then ask the user to research the assets first. If draftPost comes back refused (e.g. the date is in the past, the platform isn't a target, or the post type isn't allowed), do NOT report success — relay the reason and suggest a valid alternative (today or a later date, one of the campaign's target platforms, or one of the listed alternatives)."
