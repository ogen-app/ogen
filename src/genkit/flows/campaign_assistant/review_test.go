package campaign_assistant

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/genkit/flows/consistency"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/assistantkit"
	"github.com/ogen-app/ogen/src/infra/repository"
)

// proposals records the offers made, numbering them.
type proposals struct{ got []assistantkit.Proposal }

func (s *proposals) Propose(_ context.Context, p assistantkit.Proposal) (string, error) {
	s.got = append(s.got, p)
	return fmt.Sprintf("offer-%d", len(s.got)), nil
}

// dismissedIn has the campaign's dismissed keys.
type dismissedIn struct {
	repository.AssistantDismissalRepository
	keys map[string]bool
}

func (d dismissedIn) Keys(_ context.Context, kind models.AssistantScopeKind, id string) (map[string]bool, error) {
	if kind == models.AssistantScopeCampaign && id == "c1" {
		return d.keys, nil
	}
	return map[string]bool{}, nil
}

// checkBrief and checkPostsConsistency answer as reviewCampaign does: each
// open finding with its key and its fixes offered, the dismissed ones set
// aside and counted, and what the review read.
func TestReviewTools_OfferFixesAndSetDismissedAside(t *testing.T) {
	improve := func(aspect string) []consistency.Fix {
		return []consistency.Fix{{Kind: consistency.FixImproveBrief, Title: "Improve the " + aspect, Instruction: "fix " + aspect}}
	}
	st := &requestState{campaignID: "c1", campaign: timelineCampaign(),
		repos: CampaignAssistantRepos{Dismissals: dismissedIn{keys: map[string]bool{"b2": true}}},
		checkBrief: func(context.Context, string, consistency.OnEventFunc) (*consistency.BriefReview, error) {
			return &consistency.BriefReview{Summary: "Mostly fine", Findings: []consistency.Finding{
				{Key: "b1", Aspect: "persona", Severity: "high", Issue: "no persona", Fixes: improve("persona")},
				{Key: "b2", Aspect: "tone", Severity: "low", Issue: "vague", Fixes: improve("tone")},
			}}, nil
		},
		checkPosts: func(context.Context, consistency.PostsCheckRequest, consistency.OnEventFunc) (*consistency.PostsReview, error) {
			return &consistency.PostsReview{Checked: 3, Total: 5, Findings: []consistency.PostFinding{{Key: "p1", PostID: "p1", Title: "Late check-out",
				Issue: "free parking", Fixes: []consistency.Fix{{Kind: consistency.FixOpenPost, Title: "Open the post", PostID: "p1"}}}}}, nil
		}}
	run := func(tool string) ReviewOutput {
		sink := &proposals{}
		raw, err := runToolRaw(t, assistantkit.WithProposalSink(st.bind(t.Context()), sink), tool, map[string]any{})
		if err != nil {
			t.Fatal(err)
		}
		var out assistantkit.Outcome[ReviewOutput]
		b, _ := json.Marshal(raw)
		if err := json.Unmarshal(b, &out); err != nil || out.Result == nil {
			t.Fatalf("decode %s: %s, %v", tool, b, err)
		}
		return *out.Result
	}

	brief := run("checkBrief")
	if len(brief.Findings) != 1 || brief.Findings[0].Key != "b1" || brief.Findings[0].Kind != "brief" ||
		len(brief.Findings[0].Fixes) != 1 || brief.Findings[0].Fixes[0].ProposalID == "" {
		t.Fatalf("brief findings = %+v", brief.Findings)
	}
	if brief.Dismissed != 1 || len(brief.DismissedFindings) != 1 || brief.DismissedFindings[0].Key != "b2" ||
		len(brief.ReadFrom) != 1 || brief.ReadFrom[0] != ReadBrief || brief.BriefSummary != "Mostly fine" || brief.CheckedAt.IsZero() {
		t.Fatalf("brief review = %+v", brief)
	}

	// The finished turn reports the review the tool returned: the dismissed
	// finding neither counted nor listed as open.
	o := briefReviewOutcome(st.briefReviewResult, st.briefReviewOut)
	resp := &CampaignAssistantResponse{}
	o.attach(resp)
	if resp.BriefReview == nil || len(resp.BriefReview.Findings) != 1 || o.explanation != "I found 1 consistency issue(s) in the brief." ||
		o.payload.(CheckBriefCompleteEventPayload).FindingCount != 1 {
		t.Fatalf("outcome = %q, %+v, response %+v", o.explanation, o.payload, resp.BriefReview)
	}

	st.heavyReserved = false
	posts := run("checkPostsConsistency")
	if len(posts.Findings) != 1 || posts.Findings[0].PostID != "p1" || posts.Findings[0].Fixes[0].Kind != consistency.FixOpenPost ||
		posts.PostsChecked != 3 || posts.PostsTotal != 5 || posts.ReadFrom[0] != ReadPosts {
		t.Fatalf("posts review = %+v", posts)
	}
}
