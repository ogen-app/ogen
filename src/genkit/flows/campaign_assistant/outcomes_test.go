package campaign_assistant

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/genkit/flows/consistency"
	"github.com/ogen-app/ogen/src/genkit/jsonstream"
)

func (f *fakeMessagesRepo) CreateBatch(_ context.Context, msgs []*models.CampaignAssistantMessage) error {
	for _, m := range msgs {
		f.msgs = append(f.msgs, *m)
	}
	return nil
}

func briefReview() *consistency.BriefReview {
	return &consistency.BriefReview{
		CampaignID: "c1",
		Findings:   []consistency.Finding{{Aspect: "tone", Severity: "high", Issue: "second person", Suggestion: "drop 'you'"}},
		Summary:    "one tone issue",
	}
}

// assembled runs assembleResult over the given tool state and envelope text.
func assembled(t *testing.T, st *requestState, envelope string) *CampaignAssistantResponse {
	t.Helper()
	tr := &turn{st: st, scanner: jsonstream.New([]string{"explanation"}, nil)}
	tr.scanner.Push(envelope)
	if err := tr.assembleResult(context.Background()); err != nil {
		t.Fatal(err)
	}
	return &tr.result
}

func TestAssembleResult_WriteOutranksReview(t *testing.T) {
	st := &requestState{briefReviewResult: briefReview(), briefResult: &BriefResult{Applied: true}}
	st.recordWrite(actionBriefEnriched)

	r := assembled(t, st, `{"explanation":"Reviewed and improved the brief.","action":"brief_reviewed"}`)

	if r.Action != actionBriefEnriched {
		t.Errorf("action = %q, want %q", r.Action, actionBriefEnriched)
	}
	if r.Brief == nil || !r.Brief.Applied || r.BriefReview == nil {
		t.Errorf("both results should be attached: brief=%+v review=%+v", r.Brief, r.BriefReview)
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

func TestPersistTurn_StoresCompleteEventShape(t *testing.T) {
	repo := &fakeMessagesRepo{}
	result := &CampaignAssistantResponse{
		Explanation: "Reviewed and improved the brief.",
		Action:      actionBriefEnriched,
		Brief:       &BriefResult{Applied: true},
		BriefReview: briefReview(),
	}

	if err := persistTurn(context.Background(), CampaignAssistantRepos{Messages: repo}, CampaignAssistantRequest{CampaignID: "c1", Instruction: "improve"}, result); err != nil {
		t.Fatal(err)
	}

	if len(repo.msgs) != 2 || repo.msgs[0].Content != "improve" {
		t.Fatalf("stored %+v", repo.msgs)
	}
	var stored CampaignAssistantResponse
	if err := json.Unmarshal([]byte(repo.msgs[1].Content), &stored); err != nil {
		t.Fatal(err)
	}
	if stored.Action != actionBriefEnriched || stored.Brief == nil || !stored.Brief.Applied {
		t.Errorf("stored action/brief = %q/%+v", stored.Action, stored.Brief)
	}
	if stored.BriefReview == nil || len(stored.BriefReview.Findings) != 1 || stored.BriefReview.Findings[0].Suggestion != "drop 'you'" {
		t.Errorf("review findings not stored: %+v", stored.BriefReview)
	}
}

func TestPlannerTurn(t *testing.T) {
	full := `{"explanation":"Done.","action":"brief_reviewed","briefReview":{"findings":[{"issue":"x"}]}}`
	if got, want := plannerTurn(full), `{"action":"brief_reviewed","explanation":"Done."}`; got != want {
		t.Errorf("plannerTurn = %s, want %s", got, want)
	}
	if got := plannerTurn("plain prose"); got != "plain prose" {
		t.Errorf("plain text changed: %s", got)
	}
}

func TestNormalizeHistory(t *testing.T) {
	msgs := []models.CampaignAssistantMessage{
		userMsg(`{"briefApplied":true}`),
		modelMsg(`{"action":"brief_enriched","explanation":"x","briefApplied":true}`),
		modelMsg(`{"action":"content_plan_generated","explanation":"y","postCount":6}`),
		modelMsg(`{"explanation":"z","action":"brief_enriched","brief":{"applied":true}}`),
		modelMsg("plain prose"),
	}

	NormalizeHistory(msgs)

	want := []string{
		`{"briefApplied":true}`,
		`{"action":"brief_enriched","brief":{"applied":true},"explanation":"x"}`,
		`{"action":"content_plan_generated","contentPlan":{"postCount":6},"explanation":"y"}`,
		`{"explanation":"z","action":"brief_enriched","brief":{"applied":true}}`,
		"plain prose",
	}
	for i, m := range msgs {
		if m.Content != want[i] {
			t.Errorf("msg %d = %s, want %s", i, m.Content, want[i])
		}
	}
}
