package campaign_assistant

import (
	"fmt"

	"github.com/ogen-app/ogen/src/genkit/flows/consistency"
)

// Action values of CampaignAssistantResponse.Action set by the server.
const (
	actionAnswered           = "answered"
	actionContentPlan        = "content_plan_generated"
	actionBriefEnriched      = "brief_enriched"
	actionPostsGenerated     = "posts_generated"
	actionPostDrafted        = "post_drafted"
	actionDatesUpdated       = "dates_updated"
	actionPostsRedistributed = "posts_redistributed"
	actionBriefReviewed      = "brief_reviewed"
	actionPostsReviewed      = "posts_reviewed"
)

// outcome is a tool result committed this turn. Outcomes are authoritative
// over the model's envelope, so a terse reply cannot mask work that ran.
type outcome struct {
	action string
	// explanation is used when the model gave none.
	explanation string
	attach      func(*CampaignAssistantResponse)
	event       SSEEventKind
	payload     any
}

// outcomes lists the committed tool results in precedence order: when several
// ran, the last one sets the action and the first one supplies the fallback
// explanation. An empty slice means no tool committed anything.
func (st *requestState) outcomes() []outcome {
	var out []outcome
	if r := st.contentPlanResult; r != nil {
		out = append(out, contentPlanOutcome(r))
	}
	if r := st.briefResult; r != nil {
		out = append(out, outcome{
			action:      actionBriefEnriched,
			explanation: "I enriched the campaign brief and saved it to the campaign.",
			attach:      func(resp *CampaignAssistantResponse) { resp.Brief = r },
			event:       SSEEventEnrichBriefComplete,
			payload:     EnrichBriefCompleteEventPayload{Applied: r.Applied},
		})
	}
	if r := st.generatedPostsResult; r != nil {
		out = append(out, generatedPostsOutcome(r))
	}
	if r := st.draftPostResult; r != nil {
		out = append(out, draftPostOutcome(r))
	}
	if r := st.datesResult; r != nil {
		out = append(out, datesOutcome(r))
	}
	if r := st.redistributeResult; r != nil {
		out = append(out, redistributeOutcome(r))
	}
	if r := st.briefReviewResult; r != nil {
		out = append(out, briefReviewOutcome(r))
	}
	if r := st.postsReviewResult; r != nil {
		out = append(out, postsReviewOutcome(r))
	}
	return out
}

func contentPlanOutcome(r *ContentPlanResult) outcome {
	explanation := fmt.Sprintf("I generated a content plan with %d draft post(s) for this campaign.", r.PostCount)
	if r.PostCount == 0 {
		explanation = "I couldn't generate any posts for this campaign."
	}
	return outcome{
		action:      actionContentPlan,
		explanation: explanation,
		attach:      func(resp *CampaignAssistantResponse) { resp.ContentPlan = r },
		event:       SSEEventContentPlanComplete,
		payload:     ContentPlanCompleteEventPayload{PostCount: r.PostCount, Warnings: r.Warnings},
	}
}

func generatedPostsOutcome(r *GeneratedPostsResult) outcome {
	explanation := fmt.Sprintf("I added %d draft post(s) to the campaign.", r.PostCount)
	if r.PostCount == 0 {
		explanation = "I couldn't add any posts for that request."
	}
	return outcome{
		action:      actionPostsGenerated,
		explanation: explanation,
		attach:      func(resp *CampaignAssistantResponse) { resp.GeneratedPosts = r },
		event:       SSEEventGeneratePostsComplete,
		payload:     GeneratePostsCompleteEventPayload{PostCount: r.PostCount, Warnings: r.Warnings},
	}
}

func draftPostOutcome(r *DraftPostResult) outcome {
	explanation := fmt.Sprintf("I drafted %d post(s) from your research, ready for review.", r.PostCount)
	if r.PostCount == 0 {
		explanation = "I couldn't draft a post from that research."
	}
	return outcome{
		action:      actionPostDrafted,
		explanation: explanation,
		attach:      func(resp *CampaignAssistantResponse) { resp.DraftedPosts = r },
		event:       SSEEventDraftPostComplete,
		payload:     DraftPostCompleteEventPayload{PostCount: r.PostCount, Warnings: r.Warnings},
	}
}

func datesOutcome(r *DatesResult) outcome {
	explanation := fmt.Sprintf("Updated the campaign dates to %s – %s.", r.StartDate, r.EndDate)
	if r.PostsOutsideRange > 0 {
		explanation += fmt.Sprintf(" %d draft/ready post(s) now fall outside the new range — want me to redistribute them?", r.PostsOutsideRange)
	}
	return outcome{
		action:      actionDatesUpdated,
		explanation: explanation,
		attach:      func(resp *CampaignAssistantResponse) { resp.Dates = r },
		event:       SSEEventDatesUpdated,
		payload:     DatesUpdatedEventPayload{StartDate: r.StartDate, EndDate: r.EndDate, PostsOutsideRange: r.PostsOutsideRange},
	}
}

func redistributeOutcome(r *RedistributeResult) outcome {
	explanation := fmt.Sprintf("Redistributed %d post(s) across the campaign timeline.", r.PostsUpdated)
	if r.PostsUpdated == 0 {
		explanation = "There were no draft or ready-for-publish posts to redistribute."
	}
	return outcome{
		action:      actionPostsRedistributed,
		explanation: explanation,
		attach:      func(resp *CampaignAssistantResponse) { resp.Redistribute = r },
		event:       SSEEventPostsRedistributed,
		payload:     PostsRedistributedEventPayload{PostsUpdated: r.PostsUpdated},
	}
}

func briefReviewOutcome(r *consistency.BriefReview) outcome {
	explanation := fmt.Sprintf("I found %d consistency issue(s) in the brief.", len(r.Findings))
	if len(r.Findings) == 0 {
		explanation = "The brief looks consistent — no issues found."
	}
	return outcome{
		action:      actionBriefReviewed,
		explanation: explanation,
		attach:      func(resp *CampaignAssistantResponse) { resp.BriefReview = r },
		event:       SSEEventCheckBriefComplete,
		payload:     CheckBriefCompleteEventPayload{Consistent: r.Consistent, FindingCount: len(r.Findings)},
	}
}

func postsReviewOutcome(r *consistency.PostsReview) outcome {
	explanation := fmt.Sprintf("%d of %d checked post(s) drift from the brief.", len(r.Findings), r.Checked)
	if len(r.Findings) == 0 {
		explanation = fmt.Sprintf("Checked %d post(s); they all follow the brief.", r.Checked)
	}
	return outcome{
		action:      actionPostsReviewed,
		explanation: explanation,
		attach:      func(resp *CampaignAssistantResponse) { resp.PostsReview = r },
		event:       SSEEventCheckPostsComplete,
		payload:     CheckPostsCompleteEventPayload{Checked: r.Checked, Total: r.Total, Capped: r.Capped, DriftCount: len(r.Findings)},
	}
}
