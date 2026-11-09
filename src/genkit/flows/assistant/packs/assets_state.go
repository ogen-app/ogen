package packs

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/genkit/flows/assistant"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/usecase/assets"
)

// assetsRules tell the model what the <assets> block is for.
const assetsRules = `The <assets> block after the <view> is the asset bank's state, read on the server with the view: how many assets there are, how their analyses stand and how many ideas wait for a decision. Answer "what's left", "how many" and "what needs doing" from it without searching. Waiting analyses run on their own; offer analyseAsset only for failed ones, and offer analysing many at once before doing it.

On an asset page the open asset is the default source: questions search it alone and generation draws on it alone, unless the user names other assets or asks for all of them. Say so when you answer ("from the brochure only"), so the narrower source is never hidden.`

// assetsBegin readies the turn's results and gives it the bank's state: the
// thread's campaign's sources, else the workspace's. A state that can't be
// read leaves the block out; the tools still work.
func assetsBegin(d Deps) func(ctx context.Context, t assistant.Turn) (context.Context, string, error) {
	return func(ctx context.Context, t assistant.Turn) (context.Context, string, error) {
		ctx = withAssetsTurn(ctx)
		if d.Analyses == nil {
			return ctx, "", nil
		}
		campaignID, campaignName := "", ""
		if s := t.View.View.Scope; s.Kind == models.AssistantScopeCampaign {
			campaignID = s.ID
			if t.View.Scope != nil {
				campaignName = t.View.Scope.Title
			}
		}
		st, err := assets.ReadState(ctx, assets.StateReaders{Assets: d.Assets, Analyses: d.Analyses}, campaignID, time.Now())
		if err != nil {
			slog.WarnContext(ctx, "read asset bank state failed", logging.AttrComponent, "assistant.packs", logging.AttrError, err)
			return ctx, "", nil
		}
		return ctx, renderAssetsState(st, campaignName), nil
	}
}

// renderAssetsState is the <assets> block: the bank's counts, with the ids
// of the assets it names so the model can act on them.
func renderAssetsState(st *assets.State, campaignName string) string {
	var b strings.Builder
	b.WriteString("<assets>\n")
	where, lens := "this workspace's bank", "the workspace lens"
	if st.CampaignID != "" {
		where, lens = fmt.Sprintf("the sources of campaign %q", campaignName), "this campaign's lens"
	}
	fmt.Fprintf(&b, "%s: %s", capitalize(where), plural(st.Total, "asset", "assets"))
	var reading []string
	if st.Processing > 0 {
		reading = append(reading, fmt.Sprintf("%d still being read in", st.Processing))
	}
	if st.ProcessingFailed > 0 {
		reading = append(reading, fmt.Sprintf("%d couldn't be read", st.ProcessingFailed))
	}
	if len(reading) > 0 {
		fmt.Fprintf(&b, " (%s)", strings.Join(reading, ", "))
	}
	b.WriteString(".\n")
	fmt.Fprintf(&b, "Analyses for %s: %d done", lens, st.Analysed)
	if st.Waiting > 0 {
		fmt.Fprintf(&b, ", %d waiting in the queue%s", st.Waiting, named(st.WaitingAssets, st.Waiting))
	}
	if st.Analysing > 0 {
		fmt.Fprintf(&b, ", %d running", st.Analysing)
	}
	if st.AnalysisFailed > 0 {
		fmt.Fprintf(&b, ", %d failed%s", st.AnalysisFailed, named(st.FailedAssets, st.AnalysisFailed))
	}
	if st.Skipped > 0 {
		fmt.Fprintf(&b, ", %d skipped", st.Skipped)
	}
	b.WriteString(".\n")
	if total := st.UndecidedTotal(); total == 0 {
		b.WriteString("No ideas from the analyses are waiting for a decision.\n")
	} else {
		parts := make([]string, len(st.Undecided))
		for i, l := range st.Undecided {
			from := "the workspace lens"
			if l.CampaignID != "" {
				from = fmt.Sprintf("campaign %q", l.CampaignName)
			}
			parts[i] = fmt.Sprintf("%d from %s", l.Ideas, from)
		}
		fmt.Fprintf(&b, "Undecided ideas from the analyses: %d (%s).\n", total, strings.Join(parts, ", "))
	}
	b.WriteString("</assets>")
	return b.String()
}

// named lists the first assets of a count, and how many more there are.
func named(as []assets.AssetName, total int) string {
	if len(as) == 0 {
		return ""
	}
	parts := make([]string, len(as))
	for i, a := range as {
		parts[i] = fmt.Sprintf("%q (%s)", a.Title, a.ID)
	}
	s := strings.Join(parts, ", ")
	if more := total - len(as); more > 0 {
		s += fmt.Sprintf(" and %d more", more)
	}
	return ": " + s
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
