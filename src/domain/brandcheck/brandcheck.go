// Package brandcheck holds the rules every write of brand material obeys,
// whether it comes from the Brand editor or from a distillation commit.
package brandcheck

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/ogen-app/ogen/src/domain/models"
)

// Size caps: prose slots invite a pasted brand book, and every one is read on
// each generation turn.
const (
	MaxSampleBytes     = 2 << 10 // 2 KB
	MaxSamples         = 20
	MaxStatementBytes  = 1 << 10 // 1 KB
	MaxStatements      = 50
	MaxBannedWords     = 100
	MaxDisclaimerBytes = 2 << 10 // 2 KB
	MaxLineBytes       = 1 << 10 // 1 KB
	MaxChannelNotes    = 50
	MaxFactSourceRunes = 500
	// MaxFacts caps the ledger per workspace. Every current fact is read on
	// each generation turn.
	MaxFacts = 200
)

var (
	voiceEmoji     = map[string]bool{"never": true, "sparingly": true, "freely": true}
	voiceHashtags  = map[string]bool{"never": true, "few": true, "many": true}
	voiceFormality = map[string]bool{"casual": true, "neutral": true, "formal": true}
	voicePerson    = map[string]bool{"i": true, "we": true, "third": true}
	voiceLength    = map[string]bool{"short": true, "medium": true, "long": true}
)

// Error is brand material that breaks a rule. Its message is safe to show.
type Error struct{ Msg string }

func (e *Error) Error() string { return e.Msg }

func fail(format string, args ...any) error {
	return &Error{Msg: fmt.Sprintf(format, args...)}
}

// checkEnum accepts an empty value (unset) or a member of the set.
func checkEnum(field, val string, set map[string]bool) error {
	if val == "" || set[val] {
		return nil
	}
	return fail("invalid %s: %q", field, val)
}

// Voice checks a voice's name, sizes and rule values.
func Voice(v *models.BrandVoice) error {
	if strings.TrimSpace(v.Name) == "" {
		return fail("name is required")
	}
	if len(v.WhenToUse) > MaxLineBytes {
		return fail("whenToUse exceeds %d KB", MaxLineBytes>>10)
	}
	if len(v.Samples) > MaxSamples {
		return fail("at most %d samples", MaxSamples)
	}
	for _, s := range v.Samples {
		if len(s) > MaxSampleBytes {
			return fail("a sample exceeds %d KB", MaxSampleBytes>>10)
		}
	}
	if len(v.ChannelNotes) > MaxChannelNotes {
		return fail("at most %d channel notes", MaxChannelNotes)
	}
	for platform, note := range v.ChannelNotes {
		if len(note) > MaxLineBytes {
			return fail("channel note for %q exceeds %d KB", platform, MaxLineBytes>>10)
		}
	}
	for _, rule := range []struct {
		field, val string
		set        map[string]bool
	}{
		{"rules.emoji", v.Rules.Emoji, voiceEmoji},
		{"rules.hashtags", v.Rules.Hashtags, voiceHashtags},
		{"rules.formality", v.Rules.Formality, voiceFormality},
		{"rules.person", v.Rules.Person, voicePerson},
		{"rules.length", v.Rules.Length, voiceLength},
	} {
		if err := checkEnum(rule.field, rule.val, rule.set); err != nil {
			return err
		}
	}
	return nil
}

// Audience checks an audience's name and line sizes.
func Audience(a *models.BrandAudience) error {
	if strings.TrimSpace(a.Name) == "" {
		return fail("name is required")
	}
	for _, line := range []string{a.Who, a.ReadsOn, a.ScrollsPastWhen, a.BelievesWhen} {
		if len(line) > MaxLineBytes {
			return fail("a field exceeds %d KB", MaxLineBytes>>10)
		}
	}
	return nil
}

// Guardrails checks the rules, and facts when the key was present (non-nil).
// Rules and facts all empty is an error: deleting is the way to clear them.
func Guardrails(g *models.BrandGuardrails, facts []string) error {
	if len(facts) == 0 && len(g.MayClaim) == 0 && len(g.NeverClaim) == 0 &&
		len(g.BannedWords) == 0 && strings.TrimSpace(g.Disclaimer) == "" {
		return fail("guardrails are empty — use DELETE to clear the section")
	}
	if len(facts) > MaxFacts {
		return fail("at most %d facts per workspace", MaxFacts)
	}
	for _, s := range facts {
		if len(s) > MaxStatementBytes {
			return fail("a statement exceeds %d KB", MaxStatementBytes>>10)
		}
	}
	for _, list := range []models.StringSlice{g.MayClaim, g.NeverClaim} {
		if len(list) > MaxStatements {
			return fail("at most %d statements per list", MaxStatements)
		}
		for _, s := range list {
			if len(s) > MaxStatementBytes {
				return fail("a statement exceeds %d KB", MaxStatementBytes>>10)
			}
		}
	}
	if len(g.BannedWords) > MaxBannedWords {
		return fail("at most %d banned words", MaxBannedWords)
	}
	if len(g.Disclaimer) > MaxDisclaimerBytes {
		return fail("disclaimer exceeds %d KB", MaxDisclaimerBytes>>10)
	}
	return nil
}

// Fact checks the editable fields of a ledger fact. The statement and source
// are expected trimmed.
func Fact(f *models.BrandFact) error {
	if f.Statement == "" {
		return fail("statement is required")
	}
	if len(f.Statement) > MaxStatementBytes {
		return fail("statement exceeds %d KB", MaxStatementBytes>>10)
	}
	if !f.Subject.Valid() {
		return fail("invalid subject: %q", f.Subject)
	}
	if !f.Kind.Valid() {
		return fail("invalid kind: %q", f.Kind)
	}
	if utf8.RuneCountInString(f.Source) > MaxFactSourceRunes {
		return fail("source exceeds %d characters", MaxFactSourceRunes)
	}
	return nil
}
