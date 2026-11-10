package assistantkit

import (
	"context"
	"strings"
	"testing"

	"github.com/firebase/genkit/go/ai"
	"github.com/firebase/genkit/go/genkit"
)

func TestRegistry_TierMatchesGate(t *testing.T) {
	g := genkit.Init(t.Context())
	ungated := Spec{Name: "ungated", Pack: "p", Scopes: []Scope{ScopeCampaign}, Tier: TierProposal, Guide: "g", Tool: tool(g, "ungated")}
	if _, err := NewRegistry(ungated); err == nil || !strings.Contains(err.Error(), "must be wrapped in Gated") {
		t.Fatalf("ungated proposal tool: %v", err)
	}
	read := Spec{Name: "gatedRead", Pack: "p", Scopes: []Scope{ScopeCampaign}, Tier: TierRead, Guide: "g", Tool: gatedTool(g, "gatedRead")}
	if _, err := NewRegistry(read); err == nil || !strings.Contains(err.Error(), "must be proposal tier") {
		t.Fatalf("gated read tool: %v", err)
	}
}

// guardRun runs name through the guard of r, with next calling the tool.
func guardRun(t *testing.T, r *Registry, ctx context.Context, name string) *ai.MultipartToolResponse {
	t.Helper()
	s, _ := r.Lookup(name)
	tl := s.Tool.(ai.Tool)
	resp, err := tierGuard{reg: r}.wrapTool(ctx, &ai.ToolParams{Request: &ai.ToolRequest{Name: name, Input: map[string]any{}}, Tool: tl},
		func(ctx context.Context, p *ai.ToolParams) (*ai.MultipartToolResponse, error) {
			return p.Tool.RunRawMultipart(ctx, p.Request.Input)
		})
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestGuard_HoldsProposalTierToItsGate(t *testing.T) {
	g := genkit.Init(t.Context())
	r, err := NewRegistry(spec(g, "gen", "p", TierProposal, nil), spec(g, "look", "p", TierRead, nil))
	if err != nil {
		t.Fatal(err)
	}
	ctx := WithProposalSink(t.Context(), &memSink{})
	if out := guardRun(t, r, ctx, "gen").Output.(map[string]any); out["status"] != string(StatusProposed) {
		t.Fatalf("gated call = %+v, want proposed", out)
	}
	if out := guardRun(t, r, ctx, "look").Output.(map[string]any); out["status"] != string(StatusOK) {
		t.Fatalf("read call = %+v", out)
	}
	// A proposal-tier tool with no gate, registered past the check, is
	// refused without running.
	ran := false
	bare := genkit.DefineTool(g, "bare", "bare", func(*ai.ToolContext, noIn) (Outcome[string], error) {
		ran = true
		return OK("done"), nil
	})
	r.byName["bare"] = len(r.specs)
	r.specs = append(r.specs, Spec{Name: "bare", Pack: "p", Tier: TierProposal, Tool: bare})
	if out := guardRun(t, r, ctx, "bare").Output.(Outcome[struct{}]); ran || out.Status != StatusRefused {
		t.Fatalf("ungated call = %+v, ran = %t", out, ran)
	}
	// A confirmed call passes the guard to its gate.
	if out := guardRun(t, r, WithConfirmed(ctx), "gen").Output.(map[string]any); out["status"] != string(StatusOK) {
		t.Fatalf("confirmed call = %+v", out)
	}
}

type idsIn struct {
	IDs []string `json:"ids"`
}

// idsTool runs on ids that exist; gone are deleted while the offer waits.
func idsTool(ran *[]string, gone map[string]bool, refuse *bool) ai.ToolFunc[idsIn, Outcome[int]] {
	return Gated("touchIDs", Gate[idsIn]{
		Precheck: func(_ context.Context, in idsIn) error {
			if *refuse {
				return Refused("the campaign was archived")
			}
			return Conflicted("past the goal", Option{ID: "anyway", Title: "Anyway"})
		},
		Recheck: func(_ context.Context, in idsIn) (idsIn, error) {
			var keep []string
			for _, id := range in.IDs {
				if !gone[id] {
					keep = append(keep, id)
				}
			}
			if len(keep) == 0 {
				return in, Refused("none of them exist any more")
			}
			return idsIn{IDs: keep}, nil
		},
		Summary: func(in idsIn) string { return "Touch " + strings.Join(in.IDs, ",") },
	}, Wrap(func(_ context.Context, in idsIn) (int, error) {
		*ran = append(*ran, in.IDs...)
		return len(in.IDs), nil
	}))
}

func TestGated_ConfirmedRechecks(t *testing.T) {
	var ran []string
	gone, refuse := map[string]bool{}, false
	run := idsTool(&ran, gone, &refuse)
	sink := &memSink{}
	ctx := &ai.ToolContext{Context: WithConfirmed(WithProposalSink(t.Context(), sink))}

	// Nothing changed: the precheck's conflict was answered, and it runs.
	if out, err := run(ctx, idsIn{IDs: []string{"a", "b"}}); err != nil || !out.Succeeded() || len(ran) != 2 {
		t.Fatalf("unchanged: out = %+v, err = %v, ran = %v", out, err, ran)
	}
	// Some went: nothing runs, and what still holds is offered afresh.
	ran, gone["b"] = nil, true
	out, err := run(ctx, idsIn{IDs: []string{"a", "b"}})
	if err != nil || out.Status != StatusProposed || len(ran) != 0 || out.Summary != "Touch a" || !strings.HasPrefix(out.Reason, ChangedSince) {
		t.Fatalf("narrowed: out = %+v, err = %v, ran = %v", out, err, ran)
	}
	if len(sink.got) != 1 || len(sink.got[0].Args.(idsIn).IDs) != 1 {
		t.Fatalf("fresh offer = %+v", sink.got)
	}
	// All went: refused.
	if out, err := run(ctx, idsIn{IDs: []string{"b"}}); err != nil || out.Status != StatusRefused || len(ran) != 0 {
		t.Fatalf("all gone: out = %+v, err = %v", out, err)
	}
	// The precheck now refuses: nothing runs.
	refuse = true
	if out, err := run(ctx, idsIn{IDs: []string{"a"}}); err != nil || out.Status != StatusRefused || out.Reason != ChangedSince+"the campaign was archived" || len(ran) != 0 {
		t.Fatalf("refused: out = %+v, err = %v", out, err)
	}
}

// halfTool does the ids before "x", and stops there.
func halfTool() ai.ToolFunc[idsIn, Outcome[[]string]] {
	return GatedRetry("halfIDs", Gate[idsIn]{
		Summary: func(in idsIn) string { return "Do " + strings.Join(in.IDs, ",") },
	}, func(in idsIn, done []string) (idsIn, []NotMade, bool) {
		left := in.IDs[len(done):]
		var notMade []NotMade
		for _, id := range left {
			notMade = append(notMade, NotMade{ID: id, Reason: "stopped early"})
		}
		return idsIn{IDs: left}, notMade, len(left) > 0
	}, Wrap(func(_ context.Context, in idsIn) ([]string, error) {
		var done []string
		for _, id := range in.IDs {
			if id == "x" {
				break
			}
			done = append(done, id)
		}
		return done, nil
	}))
}

func TestGatedRetry_OffersWhatWasLeft(t *testing.T) {
	sink := &memSink{}
	ctx := &ai.ToolContext{Context: WithConfirmed(WithProposalSink(t.Context(), sink))}
	out, err := halfTool()(ctx, idsIn{IDs: []string{"a", "x", "b"}})
	if err != nil || !out.Succeeded() || len(*out.Result) != 1 || len(out.NotMade) != 2 || out.RetryProposalID != "prop-1" {
		t.Fatalf("out = %+v, err = %v", out, err)
	}
	if len(sink.got) != 1 || sink.got[0].Summary != "Retry: Do x,b" || sink.got[0].Tool != "halfIDs" {
		t.Fatalf("retry offer = %+v", sink.got)
	}
	// A run that did it all offers nothing.
	if out, _ := halfTool()(ctx, idsIn{IDs: []string{"a"}}); out.RetryProposalID != "" || out.NotMade != nil || len(sink.got) != 1 {
		t.Fatalf("whole run = %+v", out)
	}
}
