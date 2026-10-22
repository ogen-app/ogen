package branddistill

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/genkit/flows/brand_distill"
)

var candidates = []models.DistillCandidate{
	{Kind: brand_distill.KindVoice, Ref: "s1.1", Quote: "We roast light because it is the only way to taste where a coffee is from.", AssetID: "a1", Label: "acme.com/about"},
	{Kind: brand_distill.KindVoice, Ref: "s1.2", Quote: "Grind just before you brew. Everything else matters less.", AssetID: "a1", Label: "acme.com/about"},
	{Kind: brand_distill.KindVoice, Ref: "s2.1", Quote: "Your bag leaves us within 48 hours of roasting.", AssetID: "a2", Label: "acme.com/coffee"},
	{Kind: brand_distill.KindFact, Ref: "s2.1", Quote: "Subscriptions from £9 for 250 g — 2026 prices.", AssetID: "a2", Label: "acme.com/coffee"},
	{Kind: brand_distill.KindDisclaimer, Ref: "s3.1", Quote: "Prices include VAT. Coffee is a natural product.", AssetID: "a3", Label: "acme.com/terms"},
}

var today = time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)

func ev(ref, quote string) brand_distill.DraftEvidence {
	return brand_distill.DraftEvidence{Ref: ref, Quote: quote}
}

func TestGroundKeepsOnlyWhatThePassagesHold(t *testing.T) {
	d := brand_distill.Draft{
		Voices: []brand_distill.DraftVoice{{
			Name: "Warm, exacting", IsDefault: true,
			Samples: []brand_distill.DraftSample{
				{Ref: "s1.1", Text: "We roast light because it is the only way to taste where a coffee is from."},
				{Ref: "s1.2", Text: "Grind just before you brew."},
				{Ref: "s1.2", Text: "We are the best roaster in the world."}, // not in the passage
				{Ref: "s9.9", Text: "Grind just before you brew."},           // unknown ref
			},
			Rules: models.VoiceRules{Emoji: "never", Formality: "Neutral", Person: "us", Length: "medium"},
		}, {
			Name: "Invented", Samples: []brand_distill.DraftSample{{Ref: "s1.1", Text: "Nothing like this was written."}},
		}},
		Audiences: []brand_distill.DraftAudience{
			{Name: "Home brewers", Who: "people with a grinder", Evidence: []brand_distill.DraftEvidence{{QuestionID: "q_offer"}}},
			{Name: "Cafés", Evidence: []brand_distill.DraftEvidence{{QuestionID: "q_unasked"}}},
			{Name: "Existing crowd", Evidence: []brand_distill.DraftEvidence{{QuestionID: "q_offer"}}},
		},
		Guardrails: brand_distill.DraftGuardrails{
			MayClaim:    []brand_distill.DraftLine{{Text: "Ships within 48 hours of roasting", Evidence: []brand_distill.DraftEvidence{ev("s2.1", "within 48 hours of roasting")}}},
			NeverClaim:  []brand_distill.DraftLine{{Text: "Health benefits", Evidence: nil}},
			BannedWords: []brand_distill.DraftLine{{Text: "Artisanal", Evidence: []brand_distill.DraftEvidence{{QuestionID: "q_offer"}}}, {Text: "premium", Evidence: []brand_distill.DraftEvidence{{QuestionID: "q_offer"}}}},
			Disclaimer:  &brand_distill.DraftDisclaimer{Ref: "s3.1", Text: "Prices include VAT. Coffee is a natural product."},
		},
		Facts: []brand_distill.DraftFact{
			{Statement: "Subscriptions start at £9 for 250 g.", Subject: "us", Kind: "documented", ExpiresAt: "2026-12-31", Evidence: []brand_distill.DraftEvidence{ev("s2.1", "Subscriptions from £9 for 250 g")}},
			{Statement: "We roast weekly.", Subject: "nonsense", Kind: "", ExpiresAt: "2020-01-01", Evidence: []brand_distill.DraftEvidence{ev("s1.1", "")}},
			{Statement: "Every coffee ships fresh.", Evidence: []brand_distill.DraftEvidence{ev("s2.1", "a quote that is not there")}},
			{Statement: "  ALREADY   in the ledger. ", Evidence: []brand_distill.DraftEvidence{ev("s1.1", "")}},
		},
		Questions: []brand_distill.DraftQuestion{{Text: "Who are you not for?", Section: "audiences", Why: "nobody is ruled out"}, {Text: "  "}, {Text: "Length?", Section: "bogus"}},
		Coverage:  map[string]models.DistillCoverage{"voices": {Level: "found", Note: "plenty"}, "facts": {Level: "lots"}, "other": {Level: "none"}},
	}
	brand := &models.BrandData{
		Audiences:  []models.BrandAudience{{Name: "existing CROWD"}},
		Guardrails: &models.BrandGuardrails{BannedWords: models.StringSlice{"premium"}},
		Facts:      []models.BrandFact{{Statement: "Already in the ledger."}},
	}
	p := ground(d, groundInput{
		candidates: candidates, brand: brand, today: today, depth: 1,
		answers: []models.DistillAnswer{{QuestionID: "q_offer", Question: "What do you sell?", Answer: "Coffee to home brewers"}},
	})

	if len(p.Voices) != 1 {
		t.Fatalf("voices = %+v, want the invented one dropped", p.Voices)
	}
	v := p.Voices[0]
	if v.ProposalID != "v1" || len(v.Samples) != 2 || !v.Thin || !v.IsDefault {
		t.Fatalf("voice = %+v", v)
	}
	if v.Samples[0].Evidence.Label != "acme.com/about" || v.Samples[0].Evidence.AssetID != "a1" {
		t.Fatalf("sample evidence = %+v", v.Samples[0].Evidence)
	}
	if v.Rules.Emoji != "never" || v.Rules.Formality != "neutral" || v.Rules.Person != "" || v.Rules.Length != "medium" {
		t.Fatalf("rules = %+v", v.Rules)
	}
	if len(p.Audiences) != 1 || p.Audiences[0].Name != "Home brewers" || p.Audiences[0].Evidence[0].Kind != models.EvidenceKindAnswer {
		t.Fatalf("audiences = %+v", p.Audiences)
	}
	g := p.Guardrails
	if len(g.MayClaim) != 1 || g.MayClaim[0].ProposalID != "g:mayClaim:1" || len(g.NeverClaim) != 0 {
		t.Fatalf("claims = %+v / %+v", g.MayClaim, g.NeverClaim)
	}
	if len(g.BannedWords) != 1 || g.BannedWords[0].Text != "artisanal" {
		t.Fatalf("banned words = %+v", g.BannedWords)
	}
	if g.Disclaimer == nil || g.Disclaimer.Current != nil || g.Disclaimer.ProposalID != "g:disclaimer" {
		t.Fatalf("disclaimer = %+v", g.Disclaimer)
	}
	if len(p.Facts) != 2 {
		t.Fatalf("facts = %+v", p.Facts)
	}
	if f := p.Facts[0]; f.ExpiresAt == nil || f.ExpiresAt.Format("2006-01-02") != "2026-12-31" || f.Source != "acme.com/coffee" {
		t.Fatalf("dated fact = %+v", f)
	}
	if f := p.Facts[1]; f.Subject != models.FactSubjectUs || f.Kind != models.FactKindDocumented || f.ExpiresAt != nil {
		t.Fatalf("defaulted fact = %+v", f)
	}
	if len(p.Questions) != 2 || p.Questions[0].ID != "r1_q1" || p.Questions[1].Section != "" {
		t.Fatalf("questions = %+v", p.Questions)
	}
	if len(p.Coverage) != 1 || p.Coverage["voices"].Note != "plenty" {
		t.Fatalf("coverage = %+v", p.Coverage)
	}
	// Dropped: 2 bad samples, the invented voice's sample and the voice itself,
	// the unasked audience, the unsupported never-claim and the unquoted fact
	// = 7 ungrounded; the existing audience, "premium" and the ledger fact = 3
	// duplicates.
	if p.Dropped.Ungrounded != 7 || p.Dropped.Duplicate != 3 {
		t.Fatalf("dropped = %+v", p.Dropped)
	}
}

func TestGroundLeavesTheDefaultAlone(t *testing.T) {
	d := brand_distill.Draft{Voices: []brand_distill.DraftVoice{
		{Name: "A", Samples: []brand_distill.DraftSample{{Ref: "s1.1", Text: "We roast light"}}},
		{Name: "B", IsDefault: true, Samples: []brand_distill.DraftSample{{Ref: "s1.2", Text: "Grind just before you brew."}}},
	}}
	fresh := ground(d, groundInput{candidates: candidates, today: today})
	if fresh.Voices[0].IsDefault || !fresh.Voices[1].IsDefault {
		t.Fatalf("fresh workspace: defaults = %v %v, want the one asked for", fresh.Voices[0].IsDefault, fresh.Voices[1].IsDefault)
	}
	has := ground(d, groundInput{candidates: candidates, today: today, brand: &models.BrandData{Voices: []models.BrandVoice{{Name: "Plain", IsDefault: true}}}})
	if has.Voices[0].IsDefault || has.Voices[1].IsDefault {
		t.Fatal("a workspace with voices got a proposed default")
	}
}

func TestGroundSkipsADisclaimerTheWorkspaceRuns(t *testing.T) {
	d := brand_distill.Draft{Guardrails: brand_distill.DraftGuardrails{Disclaimer: &brand_distill.DraftDisclaimer{Ref: "s3.1", Text: "Prices include VAT. Coffee is a natural product."}}}
	same := ground(d, groundInput{candidates: candidates, today: today, brand: &models.BrandData{Guardrails: &models.BrandGuardrails{Disclaimer: "prices include VAT.  Coffee is a natural product."}}})
	if same.Guardrails.Disclaimer != nil || same.Dropped.Duplicate != 1 {
		t.Fatalf("disclaimer = %+v", same.Guardrails.Disclaimer)
	}
	other := ground(d, groundInput{candidates: candidates, today: today, brand: &models.BrandData{Guardrails: &models.BrandGuardrails{Disclaimer: "Ours."}}})
	if other.Guardrails.Disclaimer == nil || other.Guardrails.Disclaimer.Current == nil || *other.Guardrails.Disclaimer.Current != "Ours." {
		t.Fatalf("disclaimer = %+v", other.Guardrails.Disclaimer)
	}
}

func TestGroundQuestionsFitTheAnswerLimit(t *testing.T) {
	long := strings.Repeat("é", 400) // 800 bytes
	p := ground(brand_distill.Draft{Questions: []brand_distill.DraftQuestion{{Text: long}}}, groundInput{today: today})
	if len(p.Questions) != 1 || len(p.Questions[0].Text) > MaxQuestionBytes || !utf8.ValidString(p.Questions[0].Text) {
		t.Fatalf("questions = %+v", p.Questions)
	}
}

func TestCarryIDs(t *testing.T) {
	prev := &models.BrandProposal{
		Voices: []models.ProposedVoice{{ProposalID: "v1", Name: "Warm"}, {ProposalID: "v2", Name: "Plain"}},
		Facts:  []models.ProposedFact{{ProposalID: "f1", Statement: "A"}, {ProposalID: "f2", Statement: "B"}},
		Guardrails: models.ProposedGuardrails{
			MayClaim: []models.ProposedLine{{ProposalID: "g:mayClaim:1", Text: "x"}},
		},
	}
	next := &models.BrandProposal{
		Voices: []models.ProposedVoice{{ProposalID: "v1", Name: "plain"}, {ProposalID: "v2", Name: "New"}},
		Facts:  []models.ProposedFact{{ProposalID: "f1", Statement: "B"}, {ProposalID: "f2", Statement: "C"}, {ProposalID: "f3", Statement: "A"}},
		Guardrails: models.ProposedGuardrails{
			MayClaim: []models.ProposedLine{{ProposalID: "g:mayClaim:1", Text: "y"}, {ProposalID: "g:mayClaim:2", Text: "X"}},
		},
	}
	carryIDs(prev, next)
	got := []string{next.Voices[0].ProposalID, next.Voices[1].ProposalID, next.Facts[0].ProposalID, next.Facts[1].ProposalID, next.Facts[2].ProposalID,
		next.Guardrails.MayClaim[0].ProposalID, next.Guardrails.MayClaim[1].ProposalID}
	want := []string{"v2", "v3", "f2", "f3", "f1", "g:mayClaim:2", "g:mayClaim:1"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ids = %v, want %v", got, want)
		}
	}
}

func TestOriginOf(t *testing.T) {
	run := &models.BrandDistillRun{
		ID:      "run-1",
		Website: &models.DistillWebsite{URL: "https://acme.com"},
		Sources: []models.DistillSource{
			{AssetID: "a1", Role: models.DistillRoleWebsite},
			{AssetID: "a2", Role: models.DistillRoleAsset, Title: "Brand book"},
		},
	}
	answer := models.DistillEvidence{Kind: models.EvidenceKindAnswer, QuestionID: "q"}
	for _, tt := range []struct {
		ev   []models.DistillEvidence
		want models.BrandOrigin
	}{
		{[]models.DistillEvidence{answer, {AssetID: "a1"}}, models.BrandOrigin{Kind: "website", URL: "https://acme.com", RunID: "run-1"}},
		{[]models.DistillEvidence{{AssetID: "a2"}}, models.BrandOrigin{Kind: "document", AssetID: "a2", Title: "Brand book", RunID: "run-1"}},
		{[]models.DistillEvidence{answer}, models.BrandOrigin{Kind: "questions", RunID: "run-1"}},
	} {
		if got := originOf(run, tt.ev); got != tt.want {
			t.Errorf("originOf = %+v, want %+v", got, tt.want)
		}
	}
}
