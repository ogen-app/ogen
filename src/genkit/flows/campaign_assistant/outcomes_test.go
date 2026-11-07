package campaign_assistant

import (
	"testing"

	"github.com/ogen-app/ogen/src/genkit/flows/consistency"
)

func briefReview() *consistency.BriefReview {
	return &consistency.BriefReview{
		CampaignID: "c1",
		Findings:   []consistency.Finding{{Aspect: "tone", Severity: "high", Issue: "second person", Suggestion: "drop 'you'"}},
		Summary:    "one tone issue",
	}
}

func TestTurnAction(t *testing.T) {
	review := outcome{action: actionBriefReviewed, readOnly: true}
	postsReview := outcome{action: actionPostsReviewed, readOnly: true}
	enrich := outcome{action: actionBriefEnriched}
	generate := outcome{action: actionPostsGenerated}

	dates := outcome{action: actionDatesUpdated}
	redistribute := outcome{action: actionPostsRedistributed}

	tests := []struct {
		name   string
		outs   []outcome
		writes []string
		want   string
	}{
		{"none", nil, nil, ""},
		{"review only", []outcome{review}, nil, actionBriefReviewed},
		{"write then review", []outcome{enrich, review}, []string{actionBriefEnriched}, actionBriefEnriched},
		{"review then write", []outcome{postsReview, generate}, []string{actionPostsGenerated}, actionPostsGenerated},
		{"two reviews", []outcome{review, postsReview}, nil, actionPostsReviewed},
		{"review with a dismissal", []outcome{review, {readOnly: true}}, nil, actionBriefReviewed},
		{"dismissal alone", []outcome{{readOnly: true}}, nil, ""},
		{"writes without a recorded order", []outcome{enrich, generate}, nil, actionPostsGenerated},
		// outcomes() always lists dates before redistribute; the commit order decides.
		{"dates committed last", []outcome{dates, redistribute}, []string{actionPostsRedistributed, actionDatesUpdated}, actionDatesUpdated},
		{"redistribute committed last", []outcome{dates, redistribute}, []string{actionDatesUpdated, actionPostsRedistributed}, actionPostsRedistributed},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := turnAction(tc.outs, tc.writes); got != tc.want {
				t.Errorf("turnAction = %q, want %q", got, tc.want)
			}
		})
	}
}
