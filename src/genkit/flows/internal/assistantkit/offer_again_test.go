package assistantkit

import (
	"context"
	"testing"

	"github.com/firebase/genkit/go/genkit"
)

// Offering a call again offers it whatever its size, made afresh for the
// offer named, and runs nothing; a tool with no gate can't be offered.
func TestRegistry_OfferAgain(t *testing.T) {
	g := genkit.Init(t.Context())
	var ran int
	tool := genkit.DefineTool(g, "generate", "generate posts", countTool(&ran))
	free := genkit.DefineTool(g, "look", "look", Wrap(func(context.Context, struct{}) (string, error) { return "seen", nil }))
	r, err := NewRegistry(
		Spec{Name: "generate", Pack: "campaign", Scopes: []Scope{ScopeCampaign}, Tier: TierProposal, Guide: "Generate posts.", Tool: tool},
		Spec{Name: "look", Pack: "campaign", Scopes: []Scope{ScopeCampaign}, Tier: TierRead, Guide: "Look.", Tool: free},
	)
	if err != nil {
		t.Fatal(err)
	}
	sink := &memSink{}
	ctx := WithOffer(WithProposalSink(t.Context(), sink), "waited")
	raw, err := r.OfferAgain(ctx, "generate", map[string]any{"count": float64(2)})
	if err != nil {
		t.Fatal(err)
	}
	if out, _ := raw.(map[string]any); out["status"] != string(StatusProposed) || ran != 0 || len(sink.got) != 1 ||
		sink.got[0].Replaces != "waited" || sink.got[0].Summary != "Generate 2 posts" {
		t.Fatalf("raw = %#v, ran = %d, offers = %+v", raw, ran, sink.got)
	}
	if _, err := r.OfferAgain(ctx, "look", map[string]any{}); err == nil {
		t.Fatal("offered a tool with no gate")
	}
}
