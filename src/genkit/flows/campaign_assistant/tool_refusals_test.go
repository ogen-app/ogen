package campaign_assistant

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/firebase/genkit/go/ai"

	"github.com/ogen-app/ogen/src/genkit/flows/consistency"
	"github.com/ogen-app/ogen/src/genkit/flows/content_plan"
	"github.com/ogen-app/ogen/src/genkit/flows/draft_post"
	"github.com/ogen-app/ogen/src/genkit/flows/enrich_brief"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/assistantkit"
)

// runTool calls a registered tool the way genkit does during a turn and
// decodes its outcome.
func runTool(t *testing.T, st *requestState, name string, input map[string]any) assistantkit.Outcome[json.RawMessage] {
	t.Helper()
	raw, err := runToolRaw(t, st.bind(t.Context()), name, input)
	if err != nil {
		t.Fatalf("%s returned an error, want an outcome: %v", name, err)
	}
	var out assistantkit.Outcome[json.RawMessage]
	b, _ := json.Marshal(raw)
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("decode %s outcome %s: %v", name, b, err)
	}
	return out
}

// runToolRaw calls a registered tool as genkit does, with ctx, and returns
// what it returned, error included.
func runToolRaw(t *testing.T, ctx context.Context, name string, input map[string]any) (any, error) {
	t.Helper()
	spec, ok := newTestTools(t).reg.Lookup(name)
	if !ok {
		t.Fatalf("tool %s is not registered", name)
	}
	tool, ok := spec.Tool.(ai.Tool)
	if !ok {
		t.Fatalf("tool %s is a %T, not an ai.Tool", name, spec.Tool)
	}
	return tool.RunRaw(ctx, input)
}

// The sub-flow tools are registered with the matcher for their own flow's
// ValidationError: each becomes a refusal carrying the flow's message, and an
// error no matcher claims still aborts the call.
func TestToolRefusals_SubFlowValidationErrors(t *testing.T) {
	plan := func(err error) *requestState {
		return &requestState{campaignID: "c1", campaign: timelineCampaign(),
			contentPlan: func(context.Context, content_plan.ContentPlanRequest, content_plan.OnEventFunc) (*content_plan.ContentPlanResponse, error) {
				return nil, err
			}}
	}
	brief := func(err error) *requestState {
		return &requestState{campaignID: "c1", campaign: timelineCampaign(),
			enrichBrief: func(context.Context, enrich_brief.EnrichBriefRequest, enrich_brief.OnEventFunc) (*enrich_brief.EnrichBriefResponse, error) {
				return nil, err
			}}
	}
	review := func(err error) *requestState {
		return &requestState{campaignID: "c1", campaign: timelineCampaign(),
			checkBrief: func(context.Context, string, consistency.OnEventFunc) (*consistency.BriefReview, error) {
				return nil, err
			},
			checkPosts: func(context.Context, consistency.PostsCheckRequest, consistency.OnEventFunc) (*consistency.PostsReview, error) {
				return nil, err
			}}
	}
	cases := []struct {
		tool  string
		state func(error) *requestState
		err   error
	}{
		{"runContentPlan", plan, &content_plan.ValidationError{Msg: "the campaign has no target platforms"}},
		{"enrichBrief", brief, &enrich_brief.ValidationError{Msg: "the campaign has no type"}},
		{"checkBrief", review, &consistency.ValidationError{Msg: "the brief is empty"}},
		{"checkPostsConsistency", review, &consistency.ValidationError{Msg: "there are no posts to check"}},
	}
	for _, tc := range cases {
		t.Run(tc.tool, func(t *testing.T) {
			// Run as a confirmed call, so runContentPlan, always offered first,
			// reaches its flow.
			run := func(st *requestState) (any, error) {
				return runToolRaw(t, assistantkit.WithConfirmed(st.bind(t.Context())), tc.tool, map[string]any{})
			}
			raw, err := run(tc.state(fmt.Errorf("flow: %w", tc.err)))
			if err != nil {
				t.Fatalf("%s returned an error, want a refusal: %v", tc.tool, err)
			}
			var out assistantkit.Outcome[json.RawMessage]
			b, _ := json.Marshal(raw)
			if err := json.Unmarshal(b, &out); err != nil {
				t.Fatal(err)
			}
			if out.Status != assistantkit.StatusRefused || out.Reason != tc.err.Error() {
				t.Fatalf("outcome = %+v, want refused %q", out, tc.err.Error())
			}

			// Another flow's ValidationError, or an infrastructure failure, is
			// not this tool's to refuse.
			for _, other := range []error{errors.New("database is down"), &draft_post.ValidationError{Msg: "not mine"}} {
				if _, err := run(tc.state(other)); err == nil {
					t.Fatalf("%v came back as an outcome, want the call to fail", other)
				}
			}
		})
	}
}

// User-correctable input to the date and redistribute tools is refused with
// a reason instead of aborting the turn. None of these cases reach a
// repository, so the state needs none.
func TestToolRefusals(t *testing.T) {
	cases := []struct {
		name   string
		tool   string
		input  map[string]any
		reason string
	}{
		{"no dates given", "setCampaignDates", map[string]any{}, "give a new start or end date"},
		{"bad start", "setCampaignDates", map[string]any{"startDate": "next monday"}, "startDate must be an ISO date (YYYY-MM-DD)"},
		{"start after end", "setCampaignDates", map[string]any{"startDate": "2026-02-10", "endDate": "2026-02-01"}, "the start date must be before the end date"},
		{"redistribute without dates", "redistributePosts", map[string]any{}, "set the campaign's start and end dates first, then I can redistribute the posts"},
		{"empty asset question", "askCampaignAssets", map[string]any{"query": "  "}, "ask a question about the campaign's assets"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := timelineCampaign()
			if tc.tool == "redistributePosts" {
				c.StartDate, c.EndDate = nil, nil
			}
			out := runTool(t, &requestState{campaignID: "c1", campaign: c}, tc.tool, tc.input)
			if out.Status != assistantkit.StatusRefused || out.Reason != tc.reason {
				t.Fatalf("outcome = %+v, want refused %q", out, tc.reason)
			}
		})
	}
}
