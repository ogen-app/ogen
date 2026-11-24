package branddistill

import (
	"testing"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/genkit/flows/brand_distill"
)

func TestCountCoverage(t *testing.T) {
	reads := []models.BrandDistillRun{
		{Candidates: []models.DistillCandidate{
			{Kind: brand_distill.KindVoice, AssetID: "a1"},
			{Kind: brand_distill.KindVoice, AssetID: "a1"},
			{Kind: brand_distill.KindFact, AssetID: "a1"},
			{Kind: brand_distill.KindClaim, AssetID: "a1"},
			{Kind: brand_distill.KindClaim, AssetID: "a1"},
		}},
		{Candidates: []models.DistillCandidate{{Kind: brand_distill.KindVoice, AssetID: "a2"}}},
		{Answers: []models.DistillAnswer{{QuestionID: "q_legal", Answer: "No health claims."}, {QuestionID: "r0_q1", Answer: "x"}}},
		// A second answer to the same question replaces the first; it adds nothing.
		{Answers: []models.DistillAnswer{{QuestionID: "q_legal", Answer: "No health or medical claims."}}},
	}
	got := countCoverage(reads)
	want := map[string]models.DistillCoverage{
		SectionVoices:      {Level: LevelFound, Note: "3 passages from 2 sources."},
		SectionAudiences:   {Level: LevelNone, Note: "Nothing yet."},
		SectionGuardrails:  {Level: LevelThin, Note: "1 answer."},
		SectionFacts:       {Level: LevelThin, Note: "1 passage from 1 source."},
		SectionPositioning: {Level: LevelFound, Note: "2 passages from 1 source."},
	}
	if len(got) != len(want) {
		t.Fatalf("sections = %v", got)
	}
	for section, w := range want {
		if got[section] != w {
			t.Errorf("%s = %+v, want %+v", section, got[section], w)
		}
	}
}

func TestGatherRenumbersEachReadsRefs(t *testing.T) {
	run := &models.BrandDistillRun{}
	ok := gather(run, []models.BrandDistillRun{
		{Website: &models.DistillWebsite{URL: "https://acme.com"},
			Sources:    []models.DistillSource{{AssetID: "p1"}, {AssetID: "p2"}},
			Candidates: []models.DistillCandidate{{Ref: "s2.3", AssetID: "p2"}}},
		{Sources: []models.DistillSource{{AssetID: "deck"}},
			Candidates: []models.DistillCandidate{{Ref: "s1.1", AssetID: "deck"}}},
		{Answers: []models.DistillAnswer{{QuestionID: "q_offer", Answer: "Coffee"}}},
		{Answers: []models.DistillAnswer{{QuestionID: "q_offer", Answer: "Coffee, roasted"}}},
	})
	if !ok || len(run.Sources) != 3 || run.Website == nil {
		t.Fatalf("ok = %v, run = %+v", ok, run)
	}
	if run.Candidates[0].Ref != "s2.3" || run.Candidates[1].Ref != "s3.1" {
		t.Fatalf("refs = %s %s", run.Candidates[0].Ref, run.Candidates[1].Ref)
	}
	if assetOfRef(run, run.Candidates[1].Ref) != "deck" {
		t.Fatal("a renumbered ref no longer points at its asset")
	}
	if len(run.Answers) != 1 || run.Answers[0].Answer != "Coffee, roasted" {
		t.Fatalf("answers = %+v, want the later answer to replace the earlier", run.Answers)
	}
	if gather(&models.BrandDistillRun{}, nil) {
		t.Fatal("nothing read is something to compose")
	}
}
