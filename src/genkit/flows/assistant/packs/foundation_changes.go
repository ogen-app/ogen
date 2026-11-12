package packs

import (
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/ogen-app/ogen/src/domain/models"
)

// FieldChange is one field an edit changed: its value before and after, or,
// for a list, the lines added and removed. A channel note's field is
// channelNotes.<channel>; a voice rule's is the rule's name.
type FieldChange struct {
	Field   string   `json:"field"`
	Before  string   `json:"before,omitempty"`
	After   string   `json:"after,omitempty"`
	Added   []string `json:"added,omitempty"`
	Removed []string `json:"removed,omitempty"`
}

// changes collects the fields that differ.
type changes []FieldChange

func (c *changes) value(field, before, after string) {
	if before != after {
		*c = append(*c, FieldChange{Field: field, Before: before, After: after})
	}
}

func (c *changes) list(field string, before, after []string) {
	if added, removed := listDiff(before, after); len(added)+len(removed) > 0 {
		*c = append(*c, FieldChange{Field: field, Added: added, Removed: removed})
	}
}

// listDiff is the lines after has that before hasn't, and those it lost,
// ignoring case as the guardrail and sample lists do.
func listDiff(before, after []string) (added, removed []string) {
	in := func(list []string, s string) bool {
		return slices.ContainsFunc(list, func(x string) bool { return strings.EqualFold(x, s) })
	}
	for _, s := range after {
		if !in(before, s) {
			added = append(added, s)
		}
	}
	for _, s := range before {
		if !in(after, s) {
			removed = append(removed, s)
		}
	}
	return added, removed
}

func voiceChanges(before, after *models.BrandVoice) []FieldChange {
	var c changes
	c.value("name", before.Name, after.Name)
	c.value("whenToUse", before.WhenToUse, after.WhenToUse)
	c.value("isDefault", strconv.FormatBool(before.IsDefault), strconv.FormatBool(after.IsDefault))
	b, a := before.Rules, after.Rules
	c.value("formality", b.Formality, a.Formality)
	c.value("person", b.Person, a.Person)
	c.value("length", b.Length, a.Length)
	c.value("emoji", b.Emoji, a.Emoji)
	c.value("hashtags", b.Hashtags, a.Hashtags)
	c.value("opening", b.Opening, a.Opening)
	c.value("closing", b.Closing, a.Closing)
	c.list("samples", before.Samples, after.Samples)
	channels := map[string]string{}
	maps.Copy(channels, before.ChannelNotes)
	maps.Copy(channels, after.ChannelNotes)
	for _, k := range slices.Sorted(maps.Keys(channels)) {
		c.value("channelNotes."+k, before.ChannelNotes[k], after.ChannelNotes[k])
	}
	return c
}

func audienceChanges(before, after *models.BrandAudience) []FieldChange {
	var c changes
	c.value("name", before.Name, after.Name)
	c.value("who", before.Who, after.Who)
	c.value("readsOn", before.ReadsOn, after.ReadsOn)
	c.value("scrollsPastWhen", before.ScrollsPastWhen, after.ScrollsPastWhen)
	c.value("believesWhen", before.BelievesWhen, after.BelievesWhen)
	return c
}

// guardrailsChanges compares before, nil for none, with after.
func guardrailsChanges(before, after *models.BrandGuardrails) []FieldChange {
	if before == nil {
		before = &models.BrandGuardrails{}
	}
	var c changes
	c.list("mayClaim", before.MayClaim, after.MayClaim)
	c.list("neverClaim", before.NeverClaim, after.NeverClaim)
	c.list("bannedWords", before.BannedWords, after.BannedWords)
	c.value("disclaimer", before.Disclaimer, after.Disclaimer)
	return c
}
