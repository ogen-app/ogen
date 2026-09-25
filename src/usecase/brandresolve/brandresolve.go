// Package brandresolve turns a (campaign, post) pair into the Brand material a
// content-writing Genkit flow should write under (CON-245), and renders it as a
// compact prompt block. It is the single place the resolution precedence of the
// PRD §5 lives, so content_plan, draft_post, post_assistant and campaign_assistant
// all agree on which voice/audience/guardrails apply.
//
// Resolution fails open: any repository error, or simply an empty Brand library,
// yields a Resolved that falls back to the campaign's legacy tone_guidelines /
// target_persona prose — so a workspace without Brand material generates exactly
// as it did before this feature.
package brandresolve

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
)

// Resolved is the outcome of resolution. Any of Voice/Audience/Guardrails may be
// nil; the legacy strings carry the campaign's prose for the fallback path.
type Resolved struct {
	Voice      *models.BrandVoice
	Audience   *models.BrandAudience
	Guardrails *models.BrandGuardrails
	// Facts are the ledger's current facts (CON-316): expired ones are
	// already dropped. Independent of Guardrails, which may be nil.
	Facts         []models.BrandFact
	LegacyTone    string // campaign.ToneGuidelines — used when Voice is nil
	LegacyPersona string // campaign.TargetPersona — used when Audience is nil
}

// Resolve applies the PRD §5 precedence:
//
//	voice:    post.BrandVoiceID → campaign.BrandVoiceID → workspace default → (legacy prose)
//	audience: post.BrandAudienceID → campaign.BrandAudienceID → (legacy prose)
//	guardrails: always the workspace singleton.
//	facts: the workspace ledger minus facts expired before today (UTC).
//
// post may be nil (campaign-level resolution, e.g. draft_post batches). It never
// returns a nil *Resolved on a nil error.
func Resolve(ctx context.Context, repo repository.BrandRepository, campaign *models.Campaign, post *models.Post) (*Resolved, error) {
	r := &Resolved{}
	if campaign != nil {
		r.LegacyTone = campaign.ToneGuidelines
		r.LegacyPersona = campaign.TargetPersona
	}
	if repo == nil {
		return r, nil
	}

	data, err := repo.GetAll(ctx)
	if err != nil {
		return r, err // fail open: r still carries the legacy prose fallback
	}

	// Voice: explicit ref (post, then campaign), else the workspace default.
	if id := firstRef(refOf(post), refOf(campaign)); id != "" {
		r.Voice = findVoice(data.Voices, id)
	}
	if r.Voice == nil {
		r.Voice = defaultVoice(data.Voices)
	}

	// Audience: explicit ref (post, then campaign). No workspace default.
	if id := firstRef(audRefOf(post), audRefOf(campaign)); id != "" {
		r.Audience = findAudience(data.Audiences, id)
	}

	r.Guardrails = data.Guardrails
	r.Facts = currentFacts(ctx, data.Facts, models.CalendarDateOf(now()))
	return r, nil
}

// now is the clock expiry is judged against; tests replace it.
var now = time.Now

// currentFacts drops facts that expired before today. A fact expiring today, or
// soon ("due"), is still used. It logs how many it skipped, so expiry taking
// effect is visible.
func currentFacts(ctx context.Context, facts []models.BrandFact, today models.CalendarDate) []models.BrandFact {
	out := make([]models.BrandFact, 0, len(facts))
	for _, f := range facts {
		if !f.ExpiredOn(today) {
			out = append(out, f)
		}
	}
	if skipped := len(facts) - len(out); skipped > 0 {
		slog.InfoContext(ctx, "brand_facts_expired_skipped", "count", skipped)
	}
	return out
}

// VoiceID returns the resolved voice's id, or nil when resolution landed on the
// legacy-prose path — used to stamp posts.brand_voice_id at generation time.
func (r *Resolved) VoiceID() *string {
	if r == nil || r.Voice == nil {
		return nil
	}
	id := r.Voice.ID
	return &id
}

// ChannelNotesBlock renders the resolved voice's per-channel notes for the given
// platforms. content_plan generates across several platforms in one prompt, so —
// unlike the single-platform flows, which pass their one platform to PromptBlock —
// it needs every relevant note, not just one. Returns "" when there is no voice
// or none of the platforms has a note.
func (r *Resolved) ChannelNotesBlock(platformIDs []string) string {
	if r == nil || r.Voice == nil || len(r.Voice.ChannelNotes) == 0 {
		return ""
	}
	var b strings.Builder
	for _, id := range platformIDs {
		if note := r.Voice.ChannelNotes[id]; note != "" {
			fmt.Fprintf(&b, "- %s: %s\n", id, note)
		}
	}
	if b.Len() == 0 {
		return ""
	}
	return "## Per-channel voice notes\n" + b.String()
}

// PromptBlock renders the resolved material as a markdown block for injection
// into a generation prompt. platformID selects the voice's per-channel note.
// Returns "" only when there is genuinely nothing to say (no voice, no audience,
// no guardrails and no legacy prose).
func (r *Resolved) PromptBlock(platformID string) string {
	if r == nil {
		return ""
	}
	var b strings.Builder

	// ── Voice ──
	if r.Voice != nil {
		v := r.Voice
		fmt.Fprintf(&b, "## Brand voice — write in this voice\n**%s**", v.Name)
		if v.WhenToUse != "" {
			fmt.Fprintf(&b, " — %s", v.WhenToUse)
		}
		b.WriteString("\n")
		if len(v.Samples) > 0 {
			b.WriteString("\nThe samples ARE the voice — match their register, rhythm and habits:\n")
			for _, s := range v.Samples {
				fmt.Fprintf(&b, "- %s\n", oneLine(s))
			}
		}
		if rules := renderRules(v.Rules); rules != "" {
			fmt.Fprintf(&b, "\nRules: %s\n", rules)
		}
		if note := v.ChannelNotes[platformID]; note != "" {
			fmt.Fprintf(&b, "On this channel: %s\n", note)
		}
	} else if strings.TrimSpace(r.LegacyTone) != "" {
		fmt.Fprintf(&b, "## Tone guidelines\n%s\n", strings.TrimSpace(r.LegacyTone))
	}

	// ── Audience ──
	if r.Audience != nil {
		a := r.Audience
		fmt.Fprintf(&b, "\n## Audience — write for this reader\n**%s**", a.Name)
		if a.Who != "" {
			fmt.Fprintf(&b, ": %s", a.Who)
		}
		b.WriteString("\n")
		writeLine(&b, "Reads on", a.ReadsOn)
		writeLine(&b, "Scrolls past when", a.ScrollsPastWhen)
		writeLine(&b, "Believes a claim when", a.BelievesWhen)
	} else if strings.TrimSpace(r.LegacyPersona) != "" {
		fmt.Fprintf(&b, "\n## Target persona\n%s\n", strings.TrimSpace(r.LegacyPersona))
	}

	// ── Guardrails (always, voice-independent) ──
	// Facts come from the ledger (CON-316) and render with or without a
	// guardrails row.
	g := r.Guardrails
	if g != nil || len(r.Facts) > 0 {
		b.WriteString("\n## Guardrails — non-negotiable, whichever voice writes\n")
		writeFacts(&b, r.Facts)
	}
	if g != nil {
		writeList(&b, "May claim", g.MayClaim)
		writeList(&b, "NEVER claim", g.NeverClaim)
		if len(g.BannedWords) > 0 {
			fmt.Fprintf(&b, "- Never use these words: %s\n", strings.Join(g.BannedWords, ", "))
		}
		if strings.TrimSpace(g.Disclaimer) != "" {
			fmt.Fprintf(&b, "- Carry this disclaimer verbatim: %s\n", strings.TrimSpace(g.Disclaimer))
		}
	}

	return strings.TrimSpace(b.String())
}

// ── helpers ──

func refOf(x any) string {
	switch v := x.(type) {
	case *models.Post:
		if v != nil && v.BrandVoiceID != nil {
			return *v.BrandVoiceID
		}
	case *models.Campaign:
		if v != nil && v.BrandVoiceID != nil {
			return *v.BrandVoiceID
		}
	}
	return ""
}

func audRefOf(x any) string {
	switch v := x.(type) {
	case *models.Post:
		if v != nil && v.BrandAudienceID != nil {
			return *v.BrandAudienceID
		}
	case *models.Campaign:
		if v != nil && v.BrandAudienceID != nil {
			return *v.BrandAudienceID
		}
	}
	return ""
}

func firstRef(ids ...string) string {
	for _, id := range ids {
		if id != "" {
			return id
		}
	}
	return ""
}

func findVoice(vs []models.BrandVoice, id string) *models.BrandVoice {
	for i := range vs {
		if vs[i].ID == id {
			return &vs[i]
		}
	}
	return nil
}

func defaultVoice(vs []models.BrandVoice) *models.BrandVoice {
	for i := range vs {
		if vs[i].IsDefault {
			return &vs[i]
		}
	}
	return nil
}

func findAudience(as []models.BrandAudience, id string) *models.BrandAudience {
	for i := range as {
		if as[i].ID == id {
			return &as[i]
		}
	}
	return nil
}

func renderRules(r models.VoiceRules) string {
	var parts []string
	add := func(label, val string) {
		if val != "" {
			parts = append(parts, label+" "+val)
		}
	}
	add("emoji", r.Emoji)
	add("hashtags", r.Hashtags)
	add("formality", r.Formality)
	add("person", r.Person)
	add("length", r.Length)
	if r.Opening != "" {
		parts = append(parts, "opening: "+r.Opening)
	}
	if r.Closing != "" {
		parts = append(parts, "closing: "+r.Closing)
	}
	return strings.Join(parts, "; ")
}

func writeLine(b *strings.Builder, label, val string) {
	if strings.TrimSpace(val) != "" {
		fmt.Fprintf(b, "- %s: %s\n", label, strings.TrimSpace(val))
	}
}

func writeList(b *strings.Builder, label string, items models.StringSlice) {
	if len(items) == 0 {
		return
	}
	fmt.Fprintf(b, "- %s:\n", label)
	for _, it := range items {
		fmt.Fprintf(b, "  - %s\n", oneLine(it))
	}
}

// factGroups is the order and heading facts render under, by subject.
var factGroups = []struct {
	subject models.FactSubject
	label   string
}{
	{models.FactSubjectUs, "True about us (rest claims on these facts)"},
	{models.FactSubjectProblem, "Problems our audience has"},
	{models.FactSubjectOpportunity, "Openings in the market"},
}

// factHints tells the model how far a fact of each kind can be pushed.
// measured and documented facts need no hint.
var factHints = map[models.FactKind]string{
	models.FactKindJudgement:  " (our view: never state as a figure or a statistic)",
	models.FactKindCommitment: " (a commitment: state it as a promise, not a measurement)",
}

// writeFacts renders facts grouped by subject, skipping empty groups.
func writeFacts(b *strings.Builder, facts []models.BrandFact) {
	for _, grp := range factGroups {
		wrote := false
		for _, f := range facts {
			if f.Subject != grp.subject {
				continue
			}
			if !wrote {
				fmt.Fprintf(b, "- %s:\n", grp.label)
				wrote = true
			}
			fmt.Fprintf(b, "  - %s%s\n", oneLine(f.Statement), factHints[f.Kind])
		}
	}
}

// oneLine collapses newlines so a pasted multi-line sample stays a single bullet.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
