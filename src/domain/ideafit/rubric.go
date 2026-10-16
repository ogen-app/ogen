// Package ideafit owns the fit rubric of analysis ideas: the fixed criteria an
// idea is rated on per lens kind, the caps that force it into the Low band, the
// default weights, and the formula that turns stored ratings into a 10–100
// score.
//
// The model only rates (a level 1–5 per criterion); the score is always
// computed here. Ratings are stored with the idea, so a later weight change
// recomputes scores without asking the model again.
package ideafit

import (
	"math"
	"slices"
	"strings"
	"unicode/utf8"
)

// RubricVersion identifies the criteria and weights below. Bump it when either
// changes; stored ideas keep the version they were scored with.
const RubricVersion = 1

// Score bounds and band thresholds.
const (
	MinScore = 10
	MaxScore = 100
	// CappedMax is the highest score a capped idea can have: the top of Low.
	CappedMax = 39
	highFrom  = 70
	mediumAt  = 40
)

// Clean-up limits for model-written text.
const (
	maxWhyRunes  = 500
	maxHintRunes = 120
)

// LensKind is the lens an idea is rated against.
type LensKind string

const (
	LensCampaign  LensKind = "campaign"
	LensWorkspace LensKind = "workspace"
)

// Criterion is one thing an idea is rated on.
type Criterion struct {
	Key string
	// Question is what the level answers, as the prompt states it.
	Question string
	// Levels describes each level; Levels[0] is level 1, Levels[4] level 5.
	Levels [5]string
	Weight int
	// Hint names an extra field the rating carries ("phase" or "campaign"),
	// empty for none.
	Hint string
}

// Rating hints.
const (
	HintPhase    = "phase"
	HintCampaign = "campaign"
)

// Cap is a flag that forces an idea's score into the Low band.
type Cap string

const (
	CapGuardrailBreach Cap = "guardrail_breach"
	CapOffBrief        Cap = "off_brief"
	CapUnsupported     Cap = "unsupported"
)

// CapRule is a cap with the meaning the prompt gives it.
type CapRule struct {
	Cap     Cap
	Meaning string
}

var (
	evidence = Criterion{
		Key:      "evidence",
		Question: "How concretely the source backs the idea",
		Levels: [5]string{
			"loosely inspired by the source",
			"the source only hints at it",
			"the source supports it in general terms",
			"a clear claim or example from the source supports it",
			"a specific stat, quote or story from the source carries it",
		},
	}
	brandFit = Criterion{
		Key:      "brand_fit",
		Question: "On-voice and inside what the brand may claim",
		Levels: [5]string{
			"awkward for this brand",
			"needs heavy reworking to sound like the brand",
			"neutral for the brand",
			"fits the voice with light editing",
			"natural for this voice",
		},
	}
	audienceLevels = [5]string{
		"written for someone else",
		"mostly for a different reader",
		"of general interest to them",
		"clearly relevant to them",
		"speaks to their stated pain or interest",
	}
)

func weighted(c Criterion, w int) Criterion {
	c.Weight = w
	return c
}

var campaignCriteria = []Criterion{
	{
		Key:      "message_fit",
		Question: "Carries a key message or the description's core promise",
		Levels: [5]string{
			"unrelated to every key message",
			"only loosely connected to the campaign's theme",
			"touches the theme without carrying a key message",
			"a key message is clearly present but shares the stage",
			"a key message is the point of the post",
		},
		Weight: 25,
	},
	{
		Key:      "audience_fit",
		Question: "Would the campaign's target persona stop scrolling for it",
		Levels:   audienceLevels,
		Weight:   20,
	},
	weighted(evidence, 15),
	{
		Key:      "gap_fill",
		Question: "Adds what planned posts, existing ideas and sibling assets don't cover",
		Levels: [5]string{
			"near-duplicate of a planned post or idea",
			"mostly covered already",
			"overlaps partly with something planned",
			"a fresh take on a covered subject",
			"a clearly uncovered angle",
		},
		Weight: 15,
	},
	{
		Key:      "phase_fit",
		Question: "Serves one of the campaign type's phases and the posting goal",
		Levels: [5]string{
			"fits no phase",
			"awkward in every phase",
			"could fit a phase with some framing",
			"fits a phase well",
			"obvious home in a phase",
		},
		Weight: 10,
		Hint:   HintPhase,
	},
	{
		Key:      "timeliness",
		Question: "Fits the campaign window",
		Levels: [5]string{
			"dated, or out of the window",
			"better before or after the window",
			"fine any time in the window",
			"works well inside the window",
			"tied to the campaign's dates or a current moment",
		},
		Weight: 10,
	},
	weighted(brandFit, 5),
}

var workspaceCriteria = []Criterion{
	{
		Key:      "brand_relevance",
		Question: "A subject this brand credibly posts about, within what it may claim",
		Levels: [5]string{
			"off-topic for this brand",
			"a stretch for this brand",
			"adjacent to the brand's field",
			"clearly within the brand's field",
			"core to what the brand is known for",
		},
		Weight: 25,
	},
	{
		Key:      "audience_fit",
		Question: "Would the brand's audience (the persona when no audience is set) stop scrolling for it",
		Levels:   audienceLevels,
		Weight:   20,
	},
	weighted(evidence, 20),
	{
		Key:      "gap_fill",
		Question: "Adds what the workspace ideas already captured don't cover",
		Levels: [5]string{
			"near-duplicate of a captured idea",
			"mostly covered already",
			"overlaps partly with a captured idea",
			"a fresh take on a covered subject",
			"a clearly uncovered angle",
		},
		Weight: 15,
	},
	{
		Key:      "campaign_potential",
		Question: "Would it feed one of the active campaigns",
		Levels: [5]string{
			"fits no active campaign",
			"a weak match for any active campaign",
			"could serve a campaign with reframing",
			"fits an active campaign well",
			"made for one of the active campaigns",
		},
		Weight: 10,
		Hint:   HintCampaign,
	},
	{
		Key:      "evergreen",
		Question: "How long it stays worth posting",
		Levels: [5]string{
			"goes stale fast",
			"stale within weeks",
			"good for a season",
			"holds up for most of a year",
			"useful any month",
		},
		Weight: 5,
	},
	weighted(brandFit, 5),
}

var caps = []CapRule{
	{CapGuardrailBreach, "would need a NEVER-claim, a banned word, or a claim outside what the brand may claim"},
	{CapOffBrief, "contradicts the campaign brief or tone"},
	{CapUnsupported, "relies on a fact neither the source nor the brand facts contain"},
}

// Criteria returns the criteria of a lens kind in prompt order. A campaign
// whose type has no phases is not rated on phase_fit. Unknown kinds have none.
func Criteria(kind LensKind, hasPhases bool) []Criterion {
	switch kind {
	case LensCampaign:
		if hasPhases {
			return slices.Clone(campaignCriteria)
		}
		return slices.DeleteFunc(slices.Clone(campaignCriteria), func(c Criterion) bool { return c.Hint == HintPhase })
	case LensWorkspace:
		return slices.Clone(workspaceCriteria)
	default:
		return nil
	}
}

// Caps returns the caps a lens kind may flag; off_brief needs a brief.
func Caps(kind LensKind) []CapRule {
	switch kind {
	case LensCampaign:
		return slices.Clone(caps)
	case LensWorkspace:
		return slices.DeleteFunc(slices.Clone(caps), func(c CapRule) bool { return c.Cap == CapOffBrief })
	default:
		return nil
	}
}

// Rating is the model's level on one criterion with its one-line reason.
type Rating struct {
	Level int    `json:"level"`
	Why   string `json:"why"`
	// Phase is the phase_fit phase name; Campaign the campaign_potential hint.
	Phase    string `json:"phase,omitempty"`
	Campaign string `json:"campaign,omitempty"`
}

// Breakdown is what an idea was rated on and where. It is stored with the idea
// so the score can be recomputed and the UI can say which lens scored it.
type Breakdown struct {
	Lens LensKind `json:"lens"`
	// CampaignID is the campaign of a campaign lens, nil for the workspace lens.
	CampaignID *string           `json:"campaign_id"`
	Criteria   map[string]Rating `json:"criteria"`
	Caps       []Cap             `json:"caps"`
}

// Clean keeps the ratings and caps the lens asked for (see Criteria): unknown
// criterion keys and caps are dropped, a level above 5 or below 1 is clamped,
// and a level of 0 (the model left it out) drops the rating. Reasons and hints
// are trimmed and clamped; a hint the criterion doesn't carry is dropped. The
// returned map and slice are never nil, so the stored JSON always has both.
func Clean(kind LensKind, hasPhases bool, criteria map[string]Rating, flagged []Cap) (map[string]Rating, []Cap) {
	known := Criteria(kind, hasPhases)
	outR := make(map[string]Rating, len(known))
	for _, c := range known {
		r, ok := criteria[c.Key]
		if !ok || r.Level == 0 {
			continue
		}
		r.Level = min(max(r.Level, 1), 5)
		r.Why = clampRunes(strings.TrimSpace(r.Why), maxWhyRunes)
		r.Phase = hint(c, HintPhase, r.Phase)
		r.Campaign = hint(c, HintCampaign, r.Campaign)
		outR[c.Key] = r
	}
	rules := Caps(kind)
	outC := []Cap{}
	for _, c := range flagged {
		c = Cap(strings.TrimSpace(string(c)))
		if slices.ContainsFunc(rules, func(r CapRule) bool { return r.Cap == c }) && !slices.Contains(outC, c) {
			outC = append(outC, c)
		}
	}
	return outR, outC
}

func hint(c Criterion, name, v string) string {
	if c.Hint != name {
		return ""
	}
	return clampRunes(strings.TrimSpace(v), maxHintRunes)
}

// Score turns a breakdown into a 10–100 fit score with the default weights:
// each level maps onto 0..1, the weighted mean onto 10..100, and any cap pins
// the result at CappedMax. Criteria missing from the breakdown are left out of
// the mean. It returns nil when less than half of the lens kind's total weight
// was rated, or for an unknown lens kind.
func Score(b Breakdown) *int {
	var total, rated, sum float64
	for _, c := range Criteria(b.Lens, true) {
		w := float64(c.Weight)
		total += w
		r, ok := b.Criteria[c.Key]
		if !ok || r.Level < 1 || r.Level > 5 {
			continue
		}
		rated += w
		sum += w * float64(r.Level-1) / 4
	}
	if total == 0 || rated*2 < total {
		return nil
	}
	fit := int(math.Round(MinScore + (MaxScore-MinScore)*sum/rated))
	if len(b.Caps) > 0 {
		fit = min(fit, CappedMax)
	}
	return &fit
}

// Band is the UI's reading of a score.
type Band string

const (
	BandHigh     Band = "high"
	BandMedium   Band = "medium"
	BandLow      Band = "low"
	BandUnscored Band = "unscored"
)

// BandOf returns the band of a score; nil is unscored.
func BandOf(score *int) Band {
	switch {
	case score == nil:
		return BandUnscored
	case *score >= highFrom:
		return BandHigh
	case *score >= mediumAt:
		return BandMedium
	default:
		return BandLow
	}
}

func clampRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return strings.TrimSpace(string([]rune(s)[:n]))
}
