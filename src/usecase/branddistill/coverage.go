package branddistill

import (
	"fmt"
	"strings"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/genkit/flows/brand_distill"
)

// Coverage levels.
const (
	LevelFound = "found"
	LevelThin  = "thin"
	LevelNone  = "none"
)

// sectionOfKind is the section a passage of each kind is evidence for.
var sectionOfKind = map[string]string{
	brand_distill.KindVoice:       SectionVoices,
	brand_distill.KindAudience:    SectionAudiences,
	brand_distill.KindFact:        SectionFacts,
	brand_distill.KindRestriction: SectionGuardrails,
	brand_distill.KindDisclaimer:  SectionGuardrails,
	brand_distill.KindTerm:        SectionGuardrails,
	brand_distill.KindClaim:       SectionPositioning,
}

// foundAt is how much evidence a section needs to be found rather than thin:
// a voice needs as many passages as a voice needs samples.
var foundAt = map[string]int{
	SectionVoices:      minSamples,
	SectionAudiences:   2,
	SectionGuardrails:  2,
	SectionFacts:       3,
	SectionPositioning: 2,
}

// countCoverage says, before anything is composed, how much the reads give
// each section: the passages read of its kinds and the starter questions
// answered for it. It is a count, not the composed verdict, so it moves as
// each source finishes.
func countCoverage(reads []models.BrandDistillRun) map[string]models.DistillCoverage {
	type tally struct {
		passages, answers int
		sources           map[string]bool
	}
	tallies := map[string]*tally{}
	at := func(section string) *tally {
		t, ok := tallies[section]
		if !ok {
			t = &tally{sources: map[string]bool{}}
			tallies[section] = t
		}
		return t
	}
	sectionOf := map[string]string{}
	for _, q := range starterQuestions {
		sectionOf[q.ID] = q.Section
	}
	// A later answer to a question replaces the earlier one when the run
	// composes, so each question counts once.
	answered := map[string]bool{}
	for _, r := range reads {
		for _, c := range r.Candidates {
			if section, ok := sectionOfKind[c.Kind]; ok {
				t := at(section)
				t.passages++
				t.sources[c.AssetID] = true
			}
		}
		for _, a := range r.Answers {
			if section, ok := sectionOf[a.QuestionID]; ok && !answered[a.QuestionID] {
				answered[a.QuestionID] = true
				at(section).answers++
			}
		}
	}
	out := make(map[string]models.DistillCoverage, len(foundAt))
	for section, need := range foundAt {
		t, ok := tallies[section]
		if !ok {
			out[section] = models.DistillCoverage{Level: LevelNone, Note: "Nothing yet."}
			continue
		}
		level := LevelThin
		if t.passages+t.answers >= need {
			level = LevelFound
		}
		out[section] = models.DistillCoverage{Level: level, Note: tallyNote(t.passages, len(t.sources), t.answers)}
	}
	return out
}

// tallyNote says what a section rests on: "4 passages from 2 sources, 1 answer".
func tallyNote(passages, sources, answers int) string {
	var parts []string
	if passages > 0 {
		parts = append(parts, fmt.Sprintf("%s from %s", plural(passages, "passage"), plural(sources, "source")))
	}
	if answers > 0 {
		parts = append(parts, plural(answers, "answer"))
	}
	return strings.Join(parts, ", ") + "."
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
