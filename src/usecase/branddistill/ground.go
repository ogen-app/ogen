package branddistill

import (
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ogen-app/ogen/src/domain/brandcheck"
	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/genkit/flows/brand_distill"
)

// Proposal limits; the prompt asks for them and grounding enforces them.
const (
	maxVoices         = 3
	maxAudiences      = 3
	maxSamples        = 6
	minSamples        = 3
	maxClaims         = 15
	maxBannedWords    = 30
	maxBannedRunes    = 60
	maxProposedFacts  = 40
	maxQuestions      = 5
	maxNameRunes      = 80
	maxWhyRunes       = 300
	maxEvidenceQuotes = 5
	answerSourceLabel = "Your answer"
)

var (
	voiceEmoji     = []string{"never", "sparingly", "freely"}
	voiceHashtags  = []string{"never", "few", "many"}
	voiceFormality = []string{"casual", "neutral", "formal"}
	voicePerson    = []string{"i", "we", "third"}
	voiceLength    = []string{"short", "medium", "long"}
	sections       = []string{SectionVoices, SectionAudiences, SectionGuardrails, SectionFacts}
	levels         = []string{"found", "thin", "none"}
)

type groundInput struct {
	candidates []models.DistillCandidate
	answers    []models.DistillAnswer
	brand      *models.BrandData
	today      time.Time
	// depth numbers this run's gap questions apart from earlier rounds'.
	depth int
}

// grounder holds what a draft is checked against.
type grounder struct {
	in       groundInput
	byRef    map[string][]models.DistillCandidate
	answered map[string]bool
	dropped  models.DistillDropped
}

// ground keeps what the draft can back up: samples and the disclaimer quoted
// word for word from a passage, every other item with at least one passage
// or answer behind it, nothing the workspace already has, within the limits.
// Every kept item gets a proposalId.
func ground(d brand_distill.Draft, in groundInput) models.BrandProposal {
	g := &grounder{in: in, byRef: map[string][]models.DistillCandidate{}, answered: map[string]bool{}}
	for _, c := range in.candidates {
		g.byRef[c.Ref] = append(g.byRef[c.Ref], c)
	}
	for _, a := range in.answers {
		g.answered[a.QuestionID] = true
	}
	brand := in.brand
	if brand == nil {
		brand = &models.BrandData{}
	}
	p := models.BrandProposal{
		Voices:    g.voices(d.Voices, brand),
		Audiences: g.audiences(d.Audiences, brand),
		Facts:     g.facts(d.Facts, brand),
		Questions: g.questions(d.Questions),
		Coverage:  coverage(d.Coverage),
	}
	p.Guardrails = g.guardrails(d.Guardrails, brand.Guardrails)
	p.Dropped = g.dropped
	return p
}

// evidence checks one cited passage or answer. A passage needs a known ref
// and a quote found in it word for word; an empty quote takes the passage's
// own.
func (g *grounder) evidence(e brand_distill.DraftEvidence) (models.DistillEvidence, bool) {
	if q := strings.TrimSpace(e.QuestionID); q != "" && strings.TrimSpace(e.Ref) == "" {
		if !g.answered[q] {
			return models.DistillEvidence{}, false
		}
		return models.DistillEvidence{Kind: models.EvidenceKindAnswer, QuestionID: q}, true
	}
	ref, quote := strings.TrimSpace(e.Ref), strings.TrimSpace(e.Quote)
	for _, c := range g.byRef[ref] {
		if quote == "" {
			quote = c.Quote
		}
		if brand_distill.Verbatim(c.Quote, quote) {
			return models.DistillEvidence{Ref: ref, AssetID: c.AssetID, Label: c.Label, Quote: quote}, true
		}
	}
	return models.DistillEvidence{}, false
}

// evidences keeps the evidence that checks out, without repeats.
func (g *grounder) evidences(in []brand_distill.DraftEvidence) []models.DistillEvidence {
	var out []models.DistillEvidence
	for _, e := range in {
		if len(out) == maxEvidenceQuotes {
			break
		}
		if ev, ok := g.evidence(e); ok && !slices.Contains(out, ev) {
			out = append(out, ev)
		}
	}
	return out
}

// quoted finds a passage of ref holding text word for word.
func (g *grounder) quoted(ref, text string) (models.DistillCandidate, bool) {
	for _, c := range g.byRef[strings.TrimSpace(ref)] {
		if brand_distill.Verbatim(c.Quote, text) {
			return c, true
		}
	}
	return models.DistillCandidate{}, false
}

func (g *grounder) voices(in []brand_distill.DraftVoice, brand *models.BrandData) []models.ProposedVoice {
	out := []models.ProposedVoice{}
	hasDefault := len(brand.Voices) > 0
	for _, v := range in {
		name := clampRunes(strings.TrimSpace(v.Name), maxNameRunes)
		var samples []models.ProposedSample
		for _, sm := range v.Samples {
			text := strings.TrimSpace(sm.Text)
			c, ok := g.quoted(sm.Ref, text)
			if !ok || len(text) > brandcheck.MaxSampleBytes {
				g.dropped.Ungrounded++
				continue
			}
			if slices.ContainsFunc(samples, func(x models.ProposedSample) bool { return lineKey(x.Text) == lineKey(text) }) {
				continue
			}
			samples = append(samples, models.ProposedSample{Text: text, Evidence: models.DistillEvidence{Ref: c.Ref, AssetID: c.AssetID, Label: c.Label, Quote: text}})
		}
		switch {
		case name == "" || len(samples) == 0:
			g.dropped.Ungrounded++
			continue
		case len(out) == maxVoices:
			g.dropped.Limit++
			continue
		}
		pv := models.ProposedVoice{
			ProposalID:   fmt.Sprintf("v%d", len(out)+1),
			Name:         name,
			WhenToUse:    clampBytes(strings.TrimSpace(v.WhenToUse), brandcheck.MaxLineBytes),
			Samples:      samples[:min(len(samples), maxSamples)],
			Rules:        cleanRules(v.Rules),
			ChannelNotes: cleanNotes(v.ChannelNotes),
			Evidence:     g.evidences(v.Evidence),
		}
		pv.Thin = len(pv.Samples) < minSamples
		if v.IsDefault && !hasDefault {
			pv.IsDefault, hasDefault = true, true
		}
		out = append(out, pv)
	}
	if len(brand.Voices) == 0 && len(out) > 0 && !hasDefault {
		out[0].IsDefault = true
	}
	return out
}

func (g *grounder) audiences(in []brand_distill.DraftAudience, brand *models.BrandData) []models.ProposedAudience {
	out := []models.ProposedAudience{}
	for _, a := range in {
		name := clampRunes(strings.TrimSpace(a.Name), maxNameRunes)
		ev := g.evidences(a.Evidence)
		switch {
		case name == "" || len(ev) == 0:
			g.dropped.Ungrounded++
			continue
		case slices.ContainsFunc(brand.Audiences, func(x models.BrandAudience) bool { return lineKey(x.Name) == lineKey(name) }),
			slices.ContainsFunc(out, func(x models.ProposedAudience) bool { return lineKey(x.Name) == lineKey(name) }):
			g.dropped.Duplicate++
			continue
		case len(out) == maxAudiences:
			g.dropped.Limit++
			continue
		}
		line := func(s string) string { return clampBytes(strings.TrimSpace(s), brandcheck.MaxLineBytes) }
		out = append(out, models.ProposedAudience{
			ProposalID: fmt.Sprintf("a%d", len(out)+1), Name: name, Who: line(a.Who), ReadsOn: line(a.ReadsOn),
			ScrollsPastWhen: line(a.ScrollsPastWhen), BelievesWhen: line(a.BelievesWhen), Evidence: ev,
		})
	}
	return out
}

// lines grounds one guardrail list against what the workspace has.
func (g *grounder) lines(in []brand_distill.DraftLine, have []string, list string, limit int, lower bool, maxLen int) []models.ProposedLine {
	out := []models.ProposedLine{}
	seen := map[string]bool{}
	for _, h := range have {
		seen[lineKey(h)] = true
	}
	for _, l := range in {
		text := strings.TrimSpace(l.Text)
		if lower {
			text = strings.ToLower(text)
		}
		ev := g.evidences(l.Evidence)
		switch {
		case text == "" || len(ev) == 0 || len(text) > maxLen:
			g.dropped.Ungrounded++
			continue
		case seen[lineKey(text)]:
			g.dropped.Duplicate++
			continue
		case len(out) == limit:
			g.dropped.Limit++
			continue
		}
		seen[lineKey(text)] = true
		out = append(out, models.ProposedLine{ProposalID: fmt.Sprintf("g:%s:%d", list, len(out)+1), Text: text, Evidence: ev})
	}
	return out
}

func (g *grounder) guardrails(d brand_distill.DraftGuardrails, have *models.BrandGuardrails) models.ProposedGuardrails {
	if have == nil {
		have = &models.BrandGuardrails{}
	}
	out := models.ProposedGuardrails{
		MayClaim:    g.lines(d.MayClaim, have.MayClaim, "mayClaim", maxClaims, false, brandcheck.MaxStatementBytes),
		NeverClaim:  g.lines(d.NeverClaim, have.NeverClaim, "neverClaim", maxClaims, false, brandcheck.MaxStatementBytes),
		BannedWords: g.lines(d.BannedWords, have.BannedWords, "bannedWords", maxBannedWords, true, maxBannedRunes),
	}
	if dc := d.Disclaimer; dc != nil && strings.TrimSpace(dc.Text) != "" {
		text := strings.TrimSpace(dc.Text)
		c, ok := g.quoted(dc.Ref, text)
		switch {
		case !ok || len(text) > brandcheck.MaxDisclaimerBytes:
			g.dropped.Ungrounded++
		case lineKey(text) == lineKey(have.Disclaimer):
			g.dropped.Duplicate++
		default:
			pd := &models.ProposedDisclaimer{
				ProposalID: "g:disclaimer", Text: text,
				Evidence: []models.DistillEvidence{{Ref: c.Ref, AssetID: c.AssetID, Label: c.Label, Quote: text}},
			}
			if have.Disclaimer != "" {
				pd.Current = new(have.Disclaimer)
			}
			out.Disclaimer = pd
		}
	}
	return out
}

func (g *grounder) facts(in []brand_distill.DraftFact, brand *models.BrandData) []models.ProposedFact {
	out := []models.ProposedFact{}
	seen := map[string]bool{}
	for _, f := range brand.Facts {
		seen[lineKey(f.Statement)] = true
	}
	room := min(maxProposedFacts, brandcheck.MaxFacts-len(brand.Facts))
	for _, f := range in {
		statement := strings.TrimSpace(f.Statement)
		ev := g.evidences(f.Evidence)
		switch {
		case statement == "" || len(ev) == 0 || len(statement) > brandcheck.MaxStatementBytes:
			g.dropped.Ungrounded++
			continue
		case seen[lineKey(statement)]:
			g.dropped.Duplicate++
			continue
		case len(out) >= room:
			g.dropped.Limit++
			continue
		}
		seen[lineKey(statement)] = true
		pf := models.ProposedFact{
			ProposalID: fmt.Sprintf("f%d", len(out)+1),
			Statement:  statement,
			Subject:    models.FactSubject(strings.TrimSpace(f.Subject)),
			Kind:       models.FactKind(strings.TrimSpace(f.Kind)),
			Source:     clampRunes(sourceOf(ev[0]), brandcheck.MaxFactSourceRunes),
			Evidence:   ev,
		}
		if !pf.Subject.Valid() {
			pf.Subject = models.FactSubjectUs
		}
		if !pf.Kind.Valid() {
			pf.Kind = models.FactKindDocumented
		}
		if d, err := models.ParseCalendarDate(strings.TrimSpace(f.ExpiresAt)); err == nil && !d.Before(g.in.today) {
			pf.ExpiresAt = &d
		}
		out = append(out, pf)
	}
	return out
}

// sourceOf is how a fact's source reads in the ledger.
func sourceOf(e models.DistillEvidence) string {
	if e.Kind == models.EvidenceKindAnswer {
		return answerSourceLabel
	}
	return e.Label
}

func (g *grounder) questions(in []brand_distill.DraftQuestion) []models.DistillQuestion {
	out := []models.DistillQuestion{}
	for _, q := range in {
		text := clampRunes(strings.TrimSpace(q.Text), MaxQuestionBytes)
		if text == "" || len(out) == maxQuestions {
			continue
		}
		section := strings.TrimSpace(q.Section)
		if !slices.Contains(sections, section) {
			section = ""
		}
		out = append(out, models.DistillQuestion{
			ID:      fmt.Sprintf("r%d_q%d", g.in.depth, len(out)+1),
			Text:    text,
			Section: section,
			Why:     clampRunes(strings.TrimSpace(q.Why), maxWhyRunes),
		})
	}
	return out
}

func coverage(in map[string]models.DistillCoverage) map[string]models.DistillCoverage {
	out := map[string]models.DistillCoverage{}
	for _, section := range sections {
		c, ok := in[section]
		if !ok || !slices.Contains(levels, c.Level) {
			continue
		}
		out[section] = models.DistillCoverage{Level: c.Level, Note: clampRunes(strings.TrimSpace(c.Note), maxWhyRunes)}
	}
	return out
}

// cleanRules keeps the rule values a voice may have; anything else is unset.
func cleanRules(r models.VoiceRules) models.VoiceRules {
	pick := func(v string, set []string) string {
		v = strings.ToLower(strings.TrimSpace(v))
		if slices.Contains(set, v) {
			return v
		}
		return ""
	}
	return models.VoiceRules{
		Emoji: pick(r.Emoji, voiceEmoji), Hashtags: pick(r.Hashtags, voiceHashtags), Formality: pick(r.Formality, voiceFormality),
		Person: pick(r.Person, voicePerson), Length: pick(r.Length, voiceLength),
		Opening: clampBytes(strings.TrimSpace(r.Opening), brandcheck.MaxLineBytes),
		Closing: clampBytes(strings.TrimSpace(r.Closing), brandcheck.MaxLineBytes),
	}
}

func cleanNotes(in map[string]string) map[string]string {
	out := map[string]string{}
	for platform, note := range in {
		platform, note = strings.ToLower(strings.TrimSpace(platform)), strings.TrimSpace(note)
		if platform == "" || note == "" || len(out) == brandcheck.MaxChannelNotes {
			continue
		}
		out[platform] = clampBytes(note, brandcheck.MaxLineBytes)
	}
	return out
}

// carryIDs gives the items of a refined proposal that the previous one also
// had their previous ids, so the user's choices follow them; new items are
// numbered after the highest id of their kind.
func carryIDs(prev *models.BrandProposal, next *models.BrandProposal) {
	if prev == nil {
		return
	}
	c := idCarrier{old: map[string]string{}, top: map[string]int{}, taken: map[string]bool{}}
	for _, v := range prev.Voices {
		c.had("v", lineKey(v.Name), v.ProposalID)
	}
	for _, a := range prev.Audiences {
		c.had("a", lineKey(a.Name), a.ProposalID)
	}
	for _, f := range prev.Facts {
		c.had("f", lineKey(f.Statement), f.ProposalID)
	}
	for prefix, list := range guardrailLists(prev) {
		for _, l := range list {
			c.had(prefix, lineKey(l.Text), l.ProposalID)
		}
	}
	for i := range next.Voices {
		c.give("v", lineKey(next.Voices[i].Name), &next.Voices[i].ProposalID)
	}
	for i := range next.Audiences {
		c.give("a", lineKey(next.Audiences[i].Name), &next.Audiences[i].ProposalID)
	}
	for i := range next.Facts {
		c.give("f", lineKey(next.Facts[i].Statement), &next.Facts[i].ProposalID)
	}
	for prefix, list := range guardrailLists(next) {
		for i := range list {
			c.give(prefix, lineKey(list[i].Text), &list[i].ProposalID)
		}
	}
}

// guardrailLists are a proposal's guardrail lists by id prefix.
func guardrailLists(p *models.BrandProposal) map[string][]models.ProposedLine {
	return map[string][]models.ProposedLine{
		"g:mayClaim:":    p.Guardrails.MayClaim,
		"g:neverClaim:":  p.Guardrails.NeverClaim,
		"g:bannedWords:": p.Guardrails.BannedWords,
	}
}

// idCarrier hands out proposal ids per prefix: an item seen before keeps its
// id, a new one takes the next number.
type idCarrier struct {
	old   map[string]string // prefix+key → id
	top   map[string]int    // prefix → highest number in use
	taken map[string]bool
}

func (c *idCarrier) had(prefix, key, id string) {
	c.old[prefix+key] = id
	var n int
	if _, err := fmt.Sscanf(strings.TrimPrefix(id, prefix), "%d", &n); err == nil {
		c.top[prefix] = max(c.top[prefix], n)
	}
}

func (c *idCarrier) give(prefix, key string, id *string) {
	if prev, ok := c.old[prefix+key]; ok && !c.taken[prev] {
		*id, c.taken[prev] = prev, true
		return
	}
	c.top[prefix]++
	*id = fmt.Sprintf("%s%d", prefix, c.top[prefix])
}

// lineKey is how two items are told apart: case and spacing do not count.
func lineKey(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}

func clampRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// clampBytes cuts s to at most n bytes on a rune boundary.
func clampBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
