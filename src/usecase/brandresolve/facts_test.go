package brandresolve

import (
	"strings"
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
)

// CON-316 FR5: the generator reads the facts ledger, drops expired facts,
// groups the rest by subject and hints judgement / commitment kinds.

func day(s string) *models.CalendarDate {
	d, err := models.ParseCalendarDate(s)
	if err != nil {
		panic(err)
	}
	return &d
}

func withClock(t *testing.T, at string) {
	t.Helper()
	fixed, err := time.Parse(time.RFC3339, at)
	if err != nil {
		t.Fatal(err)
	}
	prev := now
	now = func() time.Time { return fixed }
	t.Cleanup(func() { now = prev })
}

func ledger() []models.BrandFact {
	return []models.BrandFact{
		{ID: "f1", Statement: "Support answers 94% of tickets within a day.", Subject: models.FactSubjectUs, Kind: models.FactKindMeasured},
		{ID: "f2", Statement: "Old launch offer runs to August.", Subject: models.FactSubjectUs, Kind: models.FactKindDocumented, ExpiresAt: day("2026-09-24")},
		{ID: "f3", Statement: "Pricing page lists the Q3 rates.", Subject: models.FactSubjectUs, Kind: models.FactKindDocumented, ExpiresAt: day("2026-09-26")},
		{ID: "f4", Statement: "We think onboarding is the best in the category.", Subject: models.FactSubjectUs, Kind: models.FactKindJudgement},
		{ID: "f5", Statement: "We reply within one working day.", Subject: models.FactSubjectUs, Kind: models.FactKindCommitment},
		{ID: "f6", Statement: "Finance teams rebuild the same report monthly.", Subject: models.FactSubjectProblem, Kind: models.FactKindDocumented},
		{ID: "f7", Statement: "Offer ends today.", Subject: models.FactSubjectUs, Kind: models.FactKindDocumented, ExpiresAt: day("2026-09-25")},
	}
}

func TestPromptBlockFactsGolden(t *testing.T) {
	withClock(t, "2026-09-25T23:30:00Z")
	repo := &fakeBrandRepo{data: &models.BrandData{
		Facts: ledger(),
		Guardrails: &models.BrandGuardrails{
			NeverClaim: models.StringSlice{"any future return"},
			Disclaimer: "Capital at risk.",
		},
	}}
	r, err := Resolve(t.Context(), repo, &models.Campaign{}, nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	got := r.PromptBlock("")
	want := `## Guardrails — non-negotiable, whichever voice writes
- True about us (rest claims on these facts):
  - Support answers 94% of tickets within a day.
  - Pricing page lists the Q3 rates.
  - We think onboarding is the best in the category. (our view: never state as a figure or a statistic)
  - We reply within one working day. (a commitment: state it as a promise, not a measurement)
  - Offer ends today.
- Problems our audience has:
  - Finance teams rebuild the same report monthly.
- NEVER claim:
  - any future return
- Carry this disclaimer verbatim: Capital at risk.`
	if got != want {
		t.Fatalf("block mismatch.\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestExpiredFactIsAbsentAndTomorrowsIsPresent(t *testing.T) {
	withClock(t, "2026-09-25T08:00:00Z")
	repo := &fakeBrandRepo{data: &models.BrandData{Facts: ledger()}}
	r, _ := Resolve(t.Context(), repo, &models.Campaign{}, nil)
	block := r.PromptBlock("")
	if strings.Contains(block, "Old launch offer") {
		t.Fatalf("a fact that expired yesterday must not render:\n%s", block)
	}
	if !strings.Contains(block, "Pricing page lists the Q3 rates.") {
		t.Fatalf("a fact expiring tomorrow must render:\n%s", block)
	}
	if len(r.Facts) != 6 {
		t.Fatalf("want 6 current facts, got %d", len(r.Facts))
	}
}

func TestFactsRenderWithoutGuardrailsRow(t *testing.T) {
	withClock(t, "2026-09-25T08:00:00Z")
	repo := &fakeBrandRepo{data: &models.BrandData{Facts: []models.BrandFact{
		{ID: "o1", Statement: "No competitor ships an EU-hosted option.", Subject: models.FactSubjectOpportunity, Kind: models.FactKindDocumented},
	}}}
	r, _ := Resolve(t.Context(), repo, &models.Campaign{}, nil)
	got := r.PromptBlock("")
	want := `## Guardrails — non-negotiable, whichever voice writes
- Openings in the market:
  - No competitor ships an EU-hosted option.`
	if got != want {
		t.Fatalf("block mismatch.\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestOnlyExpiredFactsAndNoGuardrailsRendersNothing(t *testing.T) {
	withClock(t, "2026-09-25T08:00:00Z")
	repo := &fakeBrandRepo{data: &models.BrandData{Facts: []models.BrandFact{
		{ID: "x", Statement: "Gone.", Subject: models.FactSubjectUs, Kind: models.FactKindMeasured, ExpiresAt: day("2026-01-01")},
	}}}
	r, _ := Resolve(t.Context(), repo, &models.Campaign{}, nil)
	if got := r.PromptBlock(""); got != "" {
		t.Fatalf("expected an empty block, got:\n%s", got)
	}
}

func TestGuardrailsWithoutCurrentFactsOmitFactsSection(t *testing.T) {
	withClock(t, "2026-09-25T08:00:00Z")
	repo := &fakeBrandRepo{data: &models.BrandData{
		Guardrails: &models.BrandGuardrails{BannedWords: models.StringSlice{"guaranteed"}},
	}}
	r, _ := Resolve(t.Context(), repo, &models.Campaign{}, nil)
	block := r.PromptBlock("")
	if strings.Contains(block, "True about us") {
		t.Fatalf("no facts → no facts group:\n%s", block)
	}
	if !strings.Contains(block, "Never use these words: guaranteed") {
		t.Fatalf("rules must still render:\n%s", block)
	}
}
