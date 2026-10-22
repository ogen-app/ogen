package branddistill

import "github.com/ogen-app/ogen/src/domain/models"

// Sections a question can fill.
const (
	SectionVoices     = "voices"
	SectionAudiences  = "audiences"
	SectionGuardrails = "guardrails"
	SectionFacts      = "facts"
)

// starterQuestions are asked when nothing is written down. Ids are stable so
// the ui can translate them.
var starterQuestions = []models.DistillQuestion{
	{ID: "q_offer", Text: "What do you sell, and who buys it?", Section: SectionAudiences},
	{ID: "q_three_words", Text: "Three words a customer would use about you.", Section: SectionVoices},
	{ID: "q_never_post", Text: "A sentence you would never post.", Section: SectionVoices},
	{ID: "q_not_for", Text: "Who are you not for?", Section: SectionAudiences},
	{ID: "q_proof", Text: "What can you prove?", Section: SectionFacts},
	{ID: "q_legal", Text: "What does legal not let you say?", Section: SectionGuardrails},
	{ID: "q_disclaimer", Text: "The disclaimer you already run, if any.", Section: SectionGuardrails},
}

// StarterQuestions returns the questions for a run with no sources.
func StarterQuestions() []models.DistillQuestion {
	return append([]models.DistillQuestion(nil), starterQuestions...)
}
