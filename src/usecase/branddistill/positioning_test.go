package branddistill

import (
	"testing"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/genkit/flows/brand_distill"
)

func TestGroundPositioning(t *testing.T) {
	quote := "We roast light because it is the only way to taste where a coffee is from."
	d := brand_distill.Draft{Positioning: &brand_distill.DraftPositioning{
		Statement: "The light roaster that tells you where each coffee is from.",
		Evidence:  []brand_distill.DraftEvidence{ev("s1.1", "the only way to taste where a coffee is from")},
		Differentiators: []brand_distill.DraftLine{
			{Text: "Roasts light", Evidence: []brand_distill.DraftEvidence{ev("s1.1", quote)}},
			{Text: "Ships within 48 hours", Evidence: []brand_distill.DraftEvidence{ev("s2.1", "within 48 hours of roasting")}},
			{Text: "Cheapest in town", Evidence: nil},
		},
	}}
	brand := &models.BrandData{Positioning: &models.BrandPositioning{Statement: "Light roasts, made plain.", Differentiators: models.StringSlice{"roasts LIGHT"}}}
	p := ground(d, groundInput{candidates: candidates, brand: brand, today: today})

	s := p.Positioning.Statement
	if s == nil || s.ProposalID != statementProposalID || s.Current == nil || *s.Current != "Light roasts, made plain." || len(s.Evidence) != 1 {
		t.Fatalf("statement = %+v", s)
	}
	if got := p.Positioning.Differentiators; len(got) != 1 || got[0].Text != "Ships within 48 hours" || got[0].ProposalID != "p:differentiators:1" {
		t.Fatalf("differentiators = %+v, want the new backed one only", got)
	}
	if p.Dropped.Duplicate != 1 || p.Dropped.Ungrounded != 1 {
		t.Fatalf("dropped = %+v", p.Dropped)
	}
	if c := CountsOf(&p); c.Positioning != 2 {
		t.Fatalf("counts = %+v", c)
	}

	t.Run("a statement nothing backs is dropped", func(t *testing.T) {
		p := ground(brand_distill.Draft{Positioning: &brand_distill.DraftPositioning{Statement: "The best."}}, groundInput{candidates: candidates, today: today})
		if p.Positioning.Statement != nil || p.Positioning.Differentiators == nil {
			t.Fatalf("positioning = %+v", p.Positioning)
		}
	})

	t.Run("a commit takes only the proposal's own items", func(t *testing.T) {
		run := &models.BrandDistillRun{ID: "run", Proposal: &p}
		batch, err := buildBatch(run, CommitInput{
			Statement:       &CommitLine{ProposalID: statementProposalID, Text: " Light, and from somewhere. "},
			Differentiators: []CommitLine{{ProposalID: "p:differentiators:1", Text: "Ships within 48 hours"}},
		})
		if err != nil {
			t.Fatal(err)
		}
		if batch.Statement == nil || batch.Statement.Text != "Light, and from somewhere." || len(batch.Differentiators) != 1 {
			t.Fatalf("batch = %+v", batch)
		}
		if _, err := buildBatch(run, CommitInput{MayClaim: []CommitLine{{ProposalID: "p:differentiators:1", Text: "x"}}}); err == nil {
			t.Fatal("a differentiator was accepted as a claim")
		}
	})
}
