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
	"github.com/ogen-app/ogen/src/usecase/ideas"
)

// ideasRules tell the model what the <ideas> block is for.
const ideasRules = `The <ideas> block after the <view> is the ideas' state, read on the server with the view: how many sit in each pile, where the waiting ones come from, and the planned ideas no post was made from yet. Answer "how many", "what's waiting" and "what's planned" from it without searching; call findIdeas for anything it doesn't name. A planned idea is ready to compile into a post or draft.`

// originLabels say where an idea came from, by its origin key.
var originLabels = map[string]string{
	models.IdeaOriginKeyAnalysis:    "from asset analyses",
	models.IdeaOriginKeyContentPlan: "from content plans",
	models.IdeaOriginKeyAssistant:   "from the assistant",
	models.IdeaOriginKeyManual:      "written by hand",
}

// ideasBegin readies the turn's results and gives it the ideas' state: the
// thread's campaign's, else every idea's. A state that can't be read leaves
// the block out; the tools still work.
func ideasBegin(d Deps) func(ctx context.Context, t assistant.Turn) (context.Context, string, error) {
	begin := beginResults(ideasPackName)
	return func(ctx context.Context, t assistant.Turn) (context.Context, string, error) {
		ctx, _, err := begin(ctx, t)
		if err != nil {
			return ctx, "", err
		}
		campaignID, campaignName := "", ""
		if s := t.View.View.Scope; s.Kind == models.AssistantScopeCampaign {
			campaignID = s.ID
			if t.View.Scope != nil {
				campaignName = t.View.Scope.Title
			}
		}
		st, err := d.Ideas.ReadState(ctx, campaignID, time.Now())
		if err != nil {
			slog.WarnContext(ctx, "read ideas state failed", logging.AttrComponent, "assistant.packs", logging.AttrError, err)
			return ctx, "", nil
		}
		return ctx, renderIdeasState(st, campaignName, campaignNames(ctx, d, st.WaitingByCampaign)), nil
	}
}

// campaignNames names the campaigns of a breakdown; one that can't be read
// goes by its id.
func campaignNames(ctx context.Context, d Deps, counts []models.AssetFacetCount) map[string]string {
	names := map[string]string{}
	if d.Campaigns == nil {
		return names
	}
	for _, c := range counts {
		if c.Value == models.AssetFilterNone {
			continue
		}
		if camp, err := d.Campaigns.GetByID(ctx, c.Value); err == nil {
			names[c.Value] = camp.Name
		}
	}
	return names
}

// renderIdeasState is the <ideas> block: the piles' counts, where the
// waiting ideas come from, and the planned ideas with their ids so the model
// can act on them.
func renderIdeasState(st *ideas.State, campaignName string, names map[string]string) string {
	var b strings.Builder
	b.WriteString("<ideas>\n")
	where := "this workspace"
	if st.CampaignID != "" {
		where = fmt.Sprintf("campaign %q", campaignName)
	}
	fmt.Fprintf(&b, "Ideas in %s: %d", where, st.Total)
	if st.Total > 0 {
		fmt.Fprintf(&b, " — %d waiting for a decision, %d yes, %d later, %d no",
			st.Piles[models.IdeaPileWaiting], st.Piles[models.IdeaPileYes], st.Piles[models.IdeaPileLater], st.Piles[models.IdeaPileNo])
	}
	b.WriteString(".\n")
	if len(st.WaitingByOrigin) > 0 {
		parts := make([]string, len(st.WaitingByOrigin))
		for i, o := range st.WaitingByOrigin {
			parts[i] = fmt.Sprintf("%d %s", o.Count, originLabels[o.Value])
		}
		fmt.Fprintf(&b, "Waiting, by origin: %s.\n", strings.Join(parts, ", "))
	}
	if len(st.WaitingBySource) > 0 {
		parts := make([]string, len(st.WaitingBySource))
		for i, s := range st.WaitingBySource {
			parts[i] = fmt.Sprintf("%d from %q (%s)", s.Count, s.Title, s.AssetID)
		}
		fmt.Fprintf(&b, "Waiting, top sources: %s.\n", strings.Join(parts, ", "))
	}
	if len(st.WaitingByCampaign) > 0 {
		parts := make([]string, len(st.WaitingByCampaign))
		for i, c := range st.WaitingByCampaign {
			switch {
			case c.Value == models.AssetFilterNone:
				parts[i] = fmt.Sprintf("%d in the workspace backlog", c.Count)
			case names[c.Value] != "":
				parts[i] = fmt.Sprintf("%d in %q (%s)", c.Count, names[c.Value], c.Value)
			default:
				parts[i] = fmt.Sprintf("%d in campaign %s", c.Count, c.Value)
			}
		}
		fmt.Fprintf(&b, "Waiting, top campaigns: %s.\n", strings.Join(parts, ", "))
	}
	if st.PlannedTotal == 0 {
		b.WriteString("No planned idea is waiting to be made into a post.\n")
	} else {
		parts := make([]string, len(st.Planned))
		for i, idea := range st.Planned {
			parts[i] = fmt.Sprintf("%q (%s)%s", idea.Title, idea.ID, planText(idea.Plan))
		}
		s := strings.Join(parts, "; ")
		if more := st.PlannedTotal - len(st.Planned); more > 0 {
			s += fmt.Sprintf("; and %d more", more)
		}
		fmt.Fprintf(&b, "Planned, no post made yet: %d — %s.\n", st.PlannedTotal, s)
	}
	b.WriteString("</ideas>")
	return b.String()
}

// planText is an idea's plan in brief: " for 2026-12-02 on linkedin, a
// carousel"; "" for none.
func planText(p models.IdeaPlan) string {
	var parts []string
	if p.TargetDate != nil {
		parts = append(parts, "for "+p.TargetDate.String())
	}
	if p.PlatformID != nil {
		parts = append(parts, "on "+*p.PlatformID)
	}
	if p.PostType != nil {
		parts = append(parts, "as "+*p.PostType)
	}
	if p.PhaseID != nil {
		parts = append(parts, "in phase "+*p.PhaseID)
	}
	if len(parts) == 0 {
		return ""
	}
	return " " + strings.Join(parts, " ")
}
