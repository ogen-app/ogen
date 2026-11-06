package assistantkit

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/firebase/genkit/go/ai"
	"github.com/firebase/genkit/go/genkit"
)

type countIn struct {
	Count int `json:"count"`
}

type memSink struct{ got []Proposal }

func (s *memSink) Propose(_ context.Context, p Proposal) (string, error) {
	s.got = append(s.got, p)
	return fmt.Sprintf("prop-%d", len(s.got)), nil
}

func countTool(ran *int) ai.ToolFunc[countIn, Outcome[int]] {
	return Gated("generate", Gate[countIn]{
		Needs:   func(_ context.Context, in countIn) bool { return in.Count > 3 },
		Summary: func(in countIn) string { return fmt.Sprintf("Generate %d posts", in.Count) },
	}, Wrap(func(_ context.Context, in countIn) (int, error) {
		*ran++
		return in.Count, nil
	}))
}

func TestGated_RunsBelowThreshold(t *testing.T) {
	var ran int
	sink := &memSink{}
	out, err := countTool(&ran)(&ai.ToolContext{Context: WithProposalSink(t.Context(), sink)}, countIn{Count: 3})
	if err != nil || !out.Succeeded() || ran != 1 || len(sink.got) != 0 {
		t.Fatalf("out = %+v, err = %v, ran = %d, proposals = %d", out, err, ran, len(sink.got))
	}
}

func TestGated_ProposesAboveThreshold(t *testing.T) {
	var ran int
	sink := &memSink{}
	out, err := countTool(&ran)(&ai.ToolContext{Context: WithProposalSink(t.Context(), sink)}, countIn{Count: 5})
	if err != nil {
		t.Fatal(err)
	}
	if ran != 0 {
		t.Fatal("a proposed call must not run")
	}
	if out.Status != StatusProposed || out.ProposalID != "prop-1" || out.Summary != "Generate 5 posts" {
		t.Fatalf("out = %+v", out)
	}
	if len(sink.got) != 1 || sink.got[0].Tool != "generate" || sink.got[0].Args.(countIn).Count != 5 {
		t.Fatalf("stored = %+v", sink.got)
	}
}

func TestGated_ConfirmedCallRuns(t *testing.T) {
	var ran int
	sink := &memSink{}
	ctx := WithConfirmed(WithProposalSink(t.Context(), sink))
	out, err := countTool(&ran)(&ai.ToolContext{Context: ctx}, countIn{Count: 9})
	if err != nil || !out.Succeeded() || ran != 1 || len(sink.got) != 0 {
		t.Fatalf("out = %+v, err = %v, ran = %d, proposals = %d", out, err, ran, len(sink.got))
	}
}

func TestGated_AlwaysWithoutNeeds(t *testing.T) {
	var ran int
	tool := Gated("plan", Gate[countIn]{}, Wrap(func(context.Context, countIn) (int, error) { ran++; return 0, nil }))
	sink := &memSink{}
	out, err := tool(&ai.ToolContext{Context: WithProposalSink(t.Context(), sink)}, countIn{})
	if err != nil || out.Status != StatusProposed || out.Summary != "plan" || ran != 0 {
		t.Fatalf("out = %+v, err = %v, ran = %d", out, err, ran)
	}
}

// A stored proposal runs by tool name with its stored (JSON-decoded) args,
// past its own gate.
func TestRegistry_RunConfirmed(t *testing.T) {
	g := genkit.Init(t.Context())
	var ran int
	tool := genkit.DefineTool(g, "generate", "generate posts", countTool(&ran))
	r, err := NewRegistry(Spec{Name: "generate", Pack: "campaign", Scopes: []Scope{ScopeCampaign},
		Tier: TierProposal, Guide: "Generate posts.", Tool: tool})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := r.RunConfirmed(WithProposalSink(t.Context(), &memSink{}), "generate", map[string]any{"count": float64(7)})
	if err != nil {
		t.Fatal(err)
	}
	out, ok := raw.(map[string]any)
	if !ok || out["status"] != string(StatusOK) || fmt.Sprint(out["result"]) != "7" || ran != 1 {
		t.Fatalf("raw = %#v, ran = %d", raw, ran)
	}
	if _, err := r.RunConfirmed(t.Context(), "missing", nil); err == nil {
		t.Fatal("running an unknown tool succeeded")
	}
}

// A request that could never run is refused up front, not stored as a
// proposal that fails after the user says yes.
func TestGated_PrecheckRefusesBeforeProposing(t *testing.T) {
	var ran int
	tool := Gated("generate", Gate[countIn]{
		Precheck: func(_ context.Context, in countIn) error {
			if in.Count < 0 {
				return Refused("count must be positive")
			}
			return nil
		},
	}, Wrap(func(context.Context, countIn) (int, error) { ran++; return 0, nil }))
	sink := &memSink{}
	out, err := tool(&ai.ToolContext{Context: WithProposalSink(t.Context(), sink)}, countIn{Count: -1})
	if err != nil || out.Status != StatusRefused || out.Reason != "count must be positive" || len(sink.got) != 0 || ran != 0 {
		t.Fatalf("out = %+v, err = %v, proposals = %d, ran = %d", out, err, len(sink.got), ran)
	}
}

func TestGated_FailsClosedWithoutSink(t *testing.T) {
	var ran int
	_, err := countTool(&ran)(&ai.ToolContext{Context: t.Context()}, countIn{Count: 5})
	if !errors.Is(err, ErrNoProposalSink) || ran != 0 {
		t.Fatalf("err = %v, ran = %d", err, ran)
	}
}
