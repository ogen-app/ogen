package asset_analysis

import (
	"cmp"
	"encoding/json"
	"slices"
	"strings"
	"unicode"

	"github.com/ogen-app/ogen/src/genkit/jsonstream"
	"github.com/ogen-app/ogen/src/kernel/textgist"
)

// Output limits. The prompt asks for these; the server enforces them.
const (
	maxSummaryRunes  = 4000
	maxIdeaTitle     = 120
	maxIdeaText      = 600
	maxIdeas         = 10
	maxKeyPoints     = 10
	maxTopics        = 8
	maxEntities      = 25
	maxMeetingItems  = 30
	maxItemRunes     = 500
	unattributed     = "Unattributed"
	confidenceStated = "stated"
	confidenceInfer  = "inferred"
)

var entityKinds = []string{"person", "org", "product", "place", "other"}

// parseJSON decodes a model reply into v, tolerating code fences and prose
// around the object.
func parseJSON(text string, v any) error {
	body := jsonstream.StripFences(text)
	if start, end := strings.IndexByte(body, '{'), strings.LastIndexByte(body, '}'); start >= 0 && end > start {
		body = body[start : end+1]
	}
	return json.Unmarshal([]byte(body), v)
}

// cleaner applies the output limits to one analysis.
type cleaner struct {
	labels    map[string]bool
	recording bool
	campaign  bool
	// existing holds normalised idea titles already in scope.
	existing map[string]bool
}

func (c cleaner) clean(a Analysis) Analysis {
	out := Analysis{
		Language:  strings.ToLower(strings.TrimSpace(a.Language)),
		Gist:      textgist.Clean(a.Gist),
		Summary:   clampRunes(strings.TrimSpace(a.Summary), maxSummaryRunes),
		Ideas:     c.ideas(a.Ideas),
		KeyPoints: cleanStrings(a.KeyPoints, maxKeyPoints),
		Topics:    cleanStrings(a.Topics, maxTopics),
		Entities:  cleanEntities(a.Entities),
	}
	if c.recording {
		out.Participants = cleanParticipants(a.Participants)
		out.Commitments = c.commitments(a.Commitments)
		out.Decisions = c.decisions(a.Decisions)
		out.OpenQuestions = cleanStrings(a.OpenQuestions, maxMeetingItems)
	}
	if c.campaign && a.Relevance != nil {
		out.Relevance = &Relevance{
			Score: min(max(a.Relevance.Score, 0), 100),
			Why:   clampRunes(strings.TrimSpace(a.Relevance.Why), maxItemRunes),
		}
	}
	return out
}

func (c cleaner) ideas(in []Idea) []Idea {
	seen := make(map[string]bool, len(in))
	out := make([]Idea, 0, min(len(in), maxIdeas))
	for _, idea := range in {
		title := clampRunes(strings.TrimSpace(idea.Title), maxIdeaTitle)
		key := normalizeTitle(title)
		if key == "" || seen[key] || c.existing[key] {
			continue
		}
		seen[key] = true
		out = append(out, Idea{
			Title:        title,
			Angle:        clampRunes(strings.TrimSpace(idea.Angle), maxIdeaText),
			Rationale:    clampRunes(strings.TrimSpace(idea.Rationale), maxIdeaText),
			SourceLabels: c.knownLabels(idea.SourceLabels),
		})
		if len(out) == maxIdeas {
			break
		}
	}
	return out
}

func (c cleaner) commitments(in []Commitment) []Commitment {
	out := make([]Commitment, 0, min(len(in), maxMeetingItems))
	for _, cm := range in {
		what := clampRunes(strings.TrimSpace(cm.Commitment), maxItemRunes)
		if what == "" {
			continue
		}
		out = append(out, Commitment{
			Owner:       cmp.Or(strings.TrimSpace(cm.Owner), unattributed),
			Commitment:  what,
			Due:         strings.TrimSpace(cm.Due),
			SourceLabel: c.knownLabel(cm.SourceLabel),
			Confidence:  confidence(cm.Confidence),
		})
		if len(out) == maxMeetingItems {
			break
		}
	}
	return out
}

func (c cleaner) decisions(in []Decision) []Decision {
	out := make([]Decision, 0, min(len(in), maxMeetingItems))
	for _, d := range in {
		text := clampRunes(strings.TrimSpace(d.Text), maxItemRunes)
		if text == "" {
			continue
		}
		out = append(out, Decision{Text: text, SourceLabel: c.knownLabel(d.SourceLabel)})
		if len(out) == maxMeetingItems {
			break
		}
	}
	return out
}

// knownLabel returns label when the source has it, else empty: a made-up
// citation is worse than none.
func (c cleaner) knownLabel(label string) string {
	label = strings.Trim(strings.TrimSpace(label), "[]")
	if c.labels[label] {
		return label
	}
	return ""
}

func (c cleaner) knownLabels(in []string) []string {
	out := []string{}
	for _, l := range in {
		if l = c.knownLabel(l); l != "" && !slices.Contains(out, l) {
			out = append(out, l)
		}
	}
	return out
}

func cleanParticipants(in []Participant) []Participant {
	seen := map[string]bool{}
	out := make([]Participant, 0, min(len(in), maxMeetingItems))
	for _, p := range in {
		name := strings.TrimSpace(p.Name)
		key := strings.ToLower(name)
		if name == "" || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, Participant{Name: name, Role: strings.TrimSpace(p.Role), Confidence: confidence(p.Confidence)})
		if len(out) == maxMeetingItems {
			break
		}
	}
	return out
}

func cleanEntities(in []Entity) []Entity {
	seen := map[string]bool{}
	out := make([]Entity, 0, min(len(in), maxEntities))
	for _, e := range in {
		name := strings.TrimSpace(e.Name)
		key := strings.ToLower(name)
		if name == "" || seen[key] {
			continue
		}
		seen[key] = true
		kind := strings.ToLower(strings.TrimSpace(e.Kind))
		if !slices.Contains(entityKinds, kind) {
			kind = "other"
		}
		out = append(out, Entity{Name: name, Kind: kind})
		if len(out) == maxEntities {
			break
		}
	}
	return out
}

func cleanStrings(in []string, limit int) []string {
	out := make([]string, 0, min(len(in), limit))
	for _, s := range in {
		if s = clampRunes(strings.TrimSpace(s), maxItemRunes); s != "" && !slices.Contains(out, s) {
			out = append(out, s)
		}
		if len(out) == limit {
			break
		}
	}
	return out
}

func confidence(s string) string {
	if strings.EqualFold(strings.TrimSpace(s), confidenceStated) {
		return confidenceStated
	}
	return confidenceInfer
}

// normalizeTitle folds case, punctuation and spacing so near-identical idea
// titles compare equal.
func normalizeTitle(s string) string {
	var b strings.Builder
	space := false
	for _, r := range strings.ToLower(s) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			if space && b.Len() > 0 {
				b.WriteByte(' ')
			}
			b.WriteRune(r)
			space = false
		default:
			space = true
		}
	}
	return b.String()
}

// NormalizeIdeaTitle exposes the dedupe key so callers can build the
// existing-ideas set the same way.
func NormalizeIdeaTitle(s string) string { return normalizeTitle(s) }

func clampRunes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return strings.TrimSpace(string(r[:n]))
}
