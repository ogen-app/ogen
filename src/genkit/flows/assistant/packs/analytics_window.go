package packs

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/firebase/genkit/go/ai"
	"github.com/firebase/genkit/go/genkit"

	"github.com/ogen-app/ogen/src/analytics/timeframe"
	"github.com/ogen-app/ogen/src/domain/viewcontext"
	"github.com/ogen-app/ogen/src/genkit/flows/assistant"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/assistantkit"
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
	Note    string  `json:"note"`
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
	var w viewcontext.Window
	if cur := assistant.Window(ctx); cur != nil {
		w = *cur
	}
	if in.Window != "" || in.From != "" || in.To != "" {
		rng, err := timeframe.Resolve(in.From, in.To, in.Window, "", time.Now().UTC())
		if errors.Is(err, timeframe.ErrWindowTooLarge) {
			return nil, assistantkit.Refused("that window is too long; pick a shorter one")
		}
		if err != nil {
			return nil, assistantkit.Refused("the window is a relative span like 28d, or from and to dates")
		}
		w.From, w.To = rng.From.Format(time.DateOnly), rng.To.AddDate(0, 0, -1).Format(time.DateOnly)
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
	// The scope check refuses an unknown platform or campaign before the
	// screen changes.
	sc, err := resolveScope(ctx, d, w.Platforms, w.CampaignID)
	if err != nil {
		return nil, err
	}
	screen, err := assistant.SetWindow(ctx, w)
	if errors.Is(err, assistant.ErrNoAnalyticsScreen) {
		return nil, assistantkit.Refused(err.Error())
	}
	if err != nil {
		return nil, err
	}
	a := Applied{From: w.From, To: w.To, Platforms: sc.Platforms()}
	if id := sc.Campaign(); id != "" {
		a.Campaign = &AppliedCampaign{ID: id, Name: sc.CampaignName()}
	}
	return &SetWindowOutput{Done: "showing " + describeWindow(a) + " on " + string(screen), Applied: a,
		Note: "The user's screen changes when your reply arrives; describe it as done."}, nil
}

// describeWindow says a window in words.
func describeWindow(a Applied) string {
	parts := []string{"every platform"}
	if len(a.Platforms) > 0 {
		parts[0] = strings.Join(a.Platforms, ", ")
	}
	if a.From != "" || a.To != "" {
		parts = append(parts, fmt.Sprintf("%s to %s", a.From, a.To))
	}
	if a.Campaign != nil {
		parts = append(parts, fmt.Sprintf("campaign %q", a.Campaign.Name))
	} else {
		parts = append(parts, "the whole workspace")
	}
	return strings.Join(parts, ", ")
}
