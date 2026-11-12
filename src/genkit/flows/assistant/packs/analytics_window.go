package packs

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/firebase/genkit/go/ai"
	"github.com/firebase/genkit/go/genkit"

	"github.com/ogen-app/ogen/src/analytics/timeframe"
	"github.com/ogen-app/ogen/src/domain/viewcontext"
	"github.com/ogen-app/ogen/src/genkit/flows/assistant"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/assistantkit"
	"github.com/ogen-app/ogen/src/usecase/analytics"
)

// SetWindowInput is the analytics window to show; what it leaves out stays
// as it is on screen.
type SetWindowInput struct {
	Window     string   `json:"window,omitempty"     jsonschema:"description=Relative window like 7d, 28d, 12w, 6mo; ignored with from and to"`
	From       string   `json:"from,omitempty"       jsonschema:"description=Inclusive start, YYYY-MM-DD"`
	To         string   `json:"to,omitempty"         jsonschema:"description=Inclusive end, YYYY-MM-DD"`
	Platforms  []string `json:"platforms,omitempty"  jsonschema:"description=Platform slugs to show, e.g. linkedin; [\"all\"] for every platform"`
	CampaignID string   `json:"campaignId,omitempty" jsonschema:"description=The campaign to narrow the dashboard to; 'all' for the whole workspace"`
}

// SetWindowOutput says what the screen will show.
type SetWindowOutput struct {
	Done    string  `json:"done"`
	Applied Applied `json:"applied"`
	// Changed names what the call changed: period, platforms, campaign.
	Changed []string `json:"changed"`
	Note    string   `json:"note"`
}

func setWindowSpec(g *genkit.Genkit, d Deps) assistantkit.Spec {
	tool := genkit.DefineTool(g, "setAnalyticsWindow",
		"Sets the analytics dashboard's window on the user's screen: its dates, platforms and campaign. What it leaves out stays as it is.",
		assistantkit.Wrap(func(ctx context.Context, in SetWindowInput) (*SetWindowOutput, error) {
			return setAnalyticsWindow(ctx, d, in)
		}),
		ai.WithStrictSchema(false))
	return spec(analyticsPackName, tool, assistantkit.TierRead,
		`Call this when the user asks the dashboard to show another period, platform or campaign — "show me only LinkedIn", "switch to March", "narrow it to the Spring campaign". It applies on the analytics screen, or the one you just navigated to. Reads after it in this message use the new window.`)
}

func setAnalyticsWindow(ctx context.Context, d Deps, in SetWindowInput) (*SetWindowOutput, error) {
	var sc *analytics.Scope
	var changed []string
	screen, w, err := assistant.SetWindow(ctx, func(cur *viewcontext.Window) (viewcontext.Window, error) {
		w, err := mergeWindow(cur, in)
		changed = w.Changes(cur)
		if err != nil {
			return w, err
		}
		// The scope check refuses an unknown platform or campaign before the
		// screen changes.
		sc, err = resolveScope(ctx, d, w.Platforms, w.CampaignID)
		return w, err
	})
	if errors.Is(err, assistant.ErrNoAnalyticsScreen) {
		return nil, assistantkit.Refused(err.Error())
	}
	if err != nil {
		return nil, err
	}
	a := Applied{From: w.From, To: w.To, Platforms: sc.Platforms(), Preset: w.Preset}
	if id := sc.Campaign(); id != "" {
		a.Campaign = &AppliedCampaign{ID: id, Name: sc.CampaignName()}
	}
	done := string(screen) + " already shows " + describeWindow(a, nil)
	if len(changed) > 0 {
		done = "set " + string(screen) + " to " + describeWindow(a, changed)
	}
	return &SetWindowOutput{Done: done, Applied: a, Changed: changed,
		Note: "The user's screen changes when your reply arrives; describe it as done, naming only what changed."}, nil
}

// mergeWindow is cur with what in sets: its dates, its platforms ("all"
// clears them), its campaign ("all" clears it).
func mergeWindow(cur *viewcontext.Window, in SetWindowInput) (viewcontext.Window, error) {
	var w viewcontext.Window
	if cur != nil {
		w = *cur
	}
	if in.Window != "" || in.From != "" || in.To != "" {
		rng, err := timeframe.Resolve(in.From, in.To, in.Window, "", time.Now().UTC())
		if errors.Is(err, timeframe.ErrWindowTooLarge) {
			return w, assistantkit.Refused("that window is too long; pick a shorter one")
		}
		if err != nil {
			return w, assistantkit.Refused("the window is a relative span like 28d, or from and to dates")
		}
		w.From, w.To = rng.From.Format(time.DateOnly), rng.To.AddDate(0, 0, -1).Format(time.DateOnly)
		// Dates picked by hand have no span; a window keeps its own, so the
		// picker shows it as a preset.
		w.Preset = ""
		if in.From == "" && in.To == "" {
			w.Preset = in.Window
		}
	}
	switch {
	case len(in.Platforms) == 1 && strings.EqualFold(in.Platforms[0], "all"):
		w.Platforms = nil
	case len(in.Platforms) > 0:
		w.Platforms = nil
		for _, p := range in.Platforms {
			w.Platforms = append(w.Platforms, strings.ToLower(strings.TrimSpace(p)))
		}
	}
	switch {
	case strings.EqualFold(in.CampaignID, "all"):
		w.CampaignID = ""
	case in.CampaignID != "":
		w.CampaignID = in.CampaignID
	}
	return w, nil
}

// describeWindow says a window in words: only the parts in changed, or
// every part when changed is nil.
func describeWindow(a Applied, changed []string) string {
	says := func(part string) bool { return changed == nil || slices.Contains(changed, part) }
	var parts []string
	switch {
	case !says(viewcontext.WindowPeriod):
	case a.Preset != "":
		parts = append(parts, "the last "+a.Preset)
	case a.From != "" || a.To != "":
		parts = append(parts, fmt.Sprintf("%s to %s", a.From, a.To))
	}
	switch {
	case !says(viewcontext.WindowPlatforms):
	case len(a.Platforms) > 0:
		parts = append(parts, strings.Join(a.Platforms, ", "))
	default:
		parts = append(parts, "every platform")
	}
	switch {
	case !says(viewcontext.WindowCampaign):
	case a.Campaign != nil:
		parts = append(parts, fmt.Sprintf("campaign %q", a.Campaign.Name))
	default:
		parts = append(parts, "the whole workspace")
	}
	return strings.Join(parts, ", ")
}
