// Package post_quality implements the Post quality assessment agent
// (CON-85): a platform-aware evaluation that scores a Post across four
// dimensions (Correctness, Clarity, Engagement, Delivery) on a configurable
// Anthropic model (Sonnet 4.5 by default) and deterministically composes an
// overall percentage from a weight profile keyed by the Post's PlatformPostType.
package post_quality

import (
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"strings"

	"github.com/ogen-app/ogen/src/domain/models"
)

// Profile is the weight each of the four dimensions carries toward a
// Post's overall score, for one PlatformPostType. Weights are fractions
// in [0,1]; a well-formed profile's four weights sum to 1.0 — the spec's
// "must sum to 100% per row".
type Profile struct {
	Correctness float64
	Clarity     float64
	Engagement  float64
	Delivery    float64
}

// Sum returns the total weight. A valid profile sums to 1.0; callers and
// tests use this to guard against mistyped configuration.
func (p Profile) Sum() float64 {
	return p.Correctness + p.Clarity + p.Engagement + p.Delivery
}

// Weight returns the weight for a single dimension, or 0 for an unknown
// key.
func (p Profile) Weight(dim models.EvaluationDimensionKey) float64 {
	switch dim {
	case models.DimensionCorrectness:
		return p.Correctness
	case models.DimensionClarity:
		return p.Clarity
	case models.DimensionEngagement:
		return p.Engagement
	case models.DimensionDelivery:
		return p.Delivery
	}
	return 0
}

// defaultTextProfile is the fallback for any PlatformPostType without a
// specific profile — the spec's "unrecognized Type falls back to a
// defined default profile (proposed: the text profile)".
var defaultTextProfile = Profile{Correctness: 0.30, Clarity: 0.30, Engagement: 0.20, Delivery: 0.20}

// defaultProfiles maps a PlatformPostType slug to its weight Profile.
//
// Only the three profiles the spec specifies are seeded, under their real
// platform slugs. The full slug vocabulary (from the seed_platforms
// migration) is much larger — text-post, image-post, carousel, video,
// article, poll, newsletter, event, live-video, reel, story, link-post,
// long-form-post, space, thread, gif-post, broadcast-channel,
// collaborative-post, guide — and every slug not listed here resolves to
// defaultTextProfile. The numbers are the spec's illustrative starting
// points; "final weight profiles per Type ... to be tuned" is an explicit
// open question, and Weights can be overridden via config without code
// changes.
var defaultProfiles = map[string]Profile{
	"text-post":  {Correctness: 0.30, Clarity: 0.30, Engagement: 0.20, Delivery: 0.20},
	"image-post": {Correctness: 0.20, Clarity: 0.15, Engagement: 0.35, Delivery: 0.30},
	"carousel":   {Correctness: 0.20, Clarity: 0.30, Engagement: 0.25, Delivery: 0.25},
}

// Weights resolves a Profile for a PlatformPostType slug, falling back to
// Default for any slug without a specific profile. It is the tunable
// configuration surface: build a custom one from config, or use
// DefaultWeights for the built-in profiles.
type Weights struct {
	byType  map[string]Profile
	Default Profile
}

// NewWeights builds a Weights from a slug->Profile map and a default
// profile. The map is copied so the caller may mutate its input freely. A
// nil/empty map is fine — every lookup then resolves to def.
func NewWeights(profiles map[string]Profile, def Profile) Weights {
	cp := make(map[string]Profile, len(profiles))
	maps.Copy(cp, profiles)
	return Weights{byType: cp, Default: def}
}

// DefaultWeights returns the built-in profiles plus the text default.
func DefaultWeights() Weights {
	return NewWeights(defaultProfiles, defaultTextProfile)
}

// For returns the Profile for a PlatformPostType slug, or Default when the
// slug has no specific profile (including the empty slug).
func (w Weights) For(slug string) Profile {
	if p, ok := w.byType[slug]; ok {
		return p
	}
	return w.Default
}

// WeightsConfig is the JSON shape accepted by the QUALITY_WEIGHT_PROFILES
// env var. Profiles overlay the built-in per-slug profiles (adding new
// slugs or replacing existing ones); Default, when present, overrides the
// fallback used for unconfigured slugs. Each profile's four weights must
// sum to 1.0.
type WeightsConfig struct {
	Profiles map[string]Profile `json:"profiles"`
	Default  *Profile           `json:"default"`
}

// weightSumTolerance is the slack allowed when checking that a profile's
// weights sum to 1.0 — enough to absorb human-entered thirds (0.333…).
const weightSumTolerance = 1e-6

// WeightsFromJSON builds a Weights from the QUALITY_WEIGHT_PROFILES env
// value. An empty string yields the built-in DefaultWeights(). Provided
// profiles overlay the built-in defaults; every resulting profile —
// including the fallback — is validated to sum to 1.0, so a mistyped
// override fails fast at boot rather than silently skewing scores.
func WeightsFromJSON(s string) (Weights, error) {
	if strings.TrimSpace(s) == "" {
		return DefaultWeights(), nil
	}

	var wc WeightsConfig
	if err := json.Unmarshal([]byte(s), &wc); err != nil {
		return Weights{}, fmt.Errorf("parse quality weight profiles: %w", err)
	}

	profiles := make(map[string]Profile, len(defaultProfiles)+len(wc.Profiles))
	maps.Copy(profiles, defaultProfiles)
	maps.Copy(profiles, wc.Profiles)

	def := defaultTextProfile
	if wc.Default != nil {
		def = *wc.Default
	}

	for slug, p := range profiles {
		if err := validateProfileSum(slug, p); err != nil {
			return Weights{}, err
		}
	}
	if err := validateProfileSum("default", def); err != nil {
		return Weights{}, err
	}

	return NewWeights(profiles, def), nil
}

func validateProfileSum(slug string, p Profile) error {
	// Each weight must be a fraction in [0,1]. A profile can sum to 1.0 yet
	// hold an out-of-range value (e.g. {1.5, -0.5, 0, 0}); a negative weight
	// would make a higher dimension score lower the overall, so reject it
	// with the offending dimension and value before the sum check.
	for _, dim := range []models.EvaluationDimensionKey{
		models.DimensionCorrectness,
		models.DimensionClarity,
		models.DimensionEngagement,
		models.DimensionDelivery,
	} {
		if w := p.Weight(dim); w < 0 || w > 1 {
			return fmt.Errorf("weight profile %q dimension %q is %.4f, must be in [0,1]", slug, dim, w)
		}
	}
	if math.Abs(p.Sum()-1.0) > weightSumTolerance {
		return fmt.Errorf("weight profile %q sums to %.4f, must sum to 1.0", slug, p.Sum())
	}
	return nil
}
