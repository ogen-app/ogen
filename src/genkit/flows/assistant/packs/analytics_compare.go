package packs

import (
	"context"
	"math"

	"github.com/firebase/genkit/go/ai"
	"github.com/firebase/genkit/go/genkit"

	"github.com/ogen-app/ogen/src/analytics/overview"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/assistantkit"
)

// Period is one window of a comparison.
type Period struct {
	Window string `json:"window,omitempty" jsonschema:"description=Relative window like 28d, 12w, 6mo; ignored with from and to"`
	From   string `json:"from,omitempty"   jsonschema:"description=Inclusive start, YYYY-MM-DD"`
	To     string `json:"to,omitempty"     jsonschema:"description=Inclusive end, YYYY-MM-DD"`
}

// ComparePeriodsInput is two windows over one scope.
type ComparePeriodsInput struct {
	First      Period   `json:"first"                jsonschema:"description=The period compared from, e.g. March"`
	Second     Period   `json:"second"               jsonschema:"description=The period compared to it, e.g. April"`
	Platforms  []string `json:"platforms,omitempty"  jsonschema:"description=Platform slugs, e.g. linkedin, instagram"`
	CampaignID string   `json:"campaignId,omitempty" jsonschema:"description=Defaults to the campaign on screen, else the thread's; 'all' for the whole workspace"`
}

// MetricComparison is one metric over both periods.
type MetricComparison struct {
	Metric string  `json:"metric"`
	Label  string  `json:"label"`
	First  float64 `json:"first"`
	Second float64 `json:"second"`
	// Change is second minus first; ChangePct is that against first, nil
	// when first is zero.
	Change    float64  `json:"change"`
	ChangePct *float64 `json:"changePct"`
}

// ComparePeriodsOutput is both periods' figures side by side.
type ComparePeriodsOutput struct {
	First   Applied            `json:"first"`
	Second  Applied            `json:"second"`
	Metrics []MetricComparison `json:"metrics"`
	// Unavailable says why there are no figures.
	Unavailable string `json:"unavailable,omitempty"`
}

func comparePeriodsSpec(g *genkit.Genkit, d Deps) assistantkit.Spec {
	tool := genkit.DefineTool(g, "comparePeriods",
		"Compares two windows' analytics over one scope: reach, interactions, engagement rate, followers and posts published in each, side by side, with the change from the first to the second.",
		assistantkit.Wrap(recorded(analyticsPackName, "comparePeriods", nil, func(ctx context.Context, in ComparePeriodsInput) (*ComparePeriodsOutput, error) {
			return comparePeriods(ctx, d, in)
		})), ai.WithStrictSchema(false))
	return spec(analyticsPackName, tool, assistantkit.TierRead,
		`Call this when the user compares two periods — "March vs April", "this month against last month", "how did Q3 compare with Q2?". Pass the earlier or named-first period as first. Quote the figures and changes as given; don't compute your own.`)
}

func comparePeriods(ctx context.Context, d Deps, in ComparePeriodsInput) (*ComparePeriodsOutput, error) {
	if in.First == (Period{}) || in.Second == (Period{}) {
		return nil, assistantkit.Refused("pass both periods, each as from and to dates or a window")
	}
	query := func(p Period) AnalyticsQuery {
		return AnalyticsQuery{Window: p.Window, From: p.From, To: p.To, Platforms: in.Platforms, CampaignID: in.CampaignID}
	}
	rngA, sc, err := rangeAndScope(ctx, d, query(in.First), "")
	if err != nil {
		return nil, err
	}
	rngB, _, err := rangeAndScope(ctx, d, query(in.Second), "")
	if err != nil {
		return nil, err
	}
	out := &ComparePeriodsOutput{First: windowApplied(rngA, sc, ""), Second: windowApplied(rngB, sc, ""), Metrics: []MetricComparison{}}
	a, reason, err := d.Analytics.Overview(ctx, rngA, sc)
	if err != nil {
		return nil, err
	}
	b, reasonB, err := d.Analytics.Overview(ctx, rngB, sc)
	if err != nil {
		return nil, err
	}
	if a == nil || b == nil {
		out.Unavailable = reason
		if out.Unavailable == "" {
			out.Unavailable = reasonB
		}
		return out, nil
	}
	out.Metrics = compareCards(a.Cards, b.Cards)
	return out, nil
}

// compareCards pairs the two overviews' cards by metric, in the first's
// order.
func compareCards(a, b []overview.Card) []MetricComparison {
	out := []MetricComparison{}
	for _, ca := range a {
		for _, cb := range b {
			if cb.Metric != ca.Metric {
				continue
			}
			m := MetricComparison{Metric: ca.Metric, Label: ca.Label, First: ca.Value, Second: cb.Value, Change: cb.Value - ca.Value}
			if ca.Value != 0 {
				pct := math.Round(m.Change/ca.Value*1000) / 10
				m.ChangePct = &pct
			}
			out = append(out, m)
		}
	}
	return out
}
