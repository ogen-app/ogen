package campaign_assistant

import (
	"context"
	"fmt"
	"time"

	"github.com/ogen-app/ogen/src/usecase/campaign_actions/state"
)

// maxStarters caps the starters a campaign's state offers.
const maxStarters = 3

// The kinds of starter, for the client's icon.
const (
	starterFill     = "fill"
	starterDraft    = "draft"
	starterCalendar = "calendar"
	starterSources  = "sources"
)

// CampaignState is what the assistant knows of a campaign without being
// asked: the snapshot it reads at the top of every campaign turn, and the
// starters that lead with its numbers.
type CampaignState struct {
	State    state.Snapshot `json:"state"`
	Starters []Starter      `json:"starters"`
}

// Starter is a message the client offers to send: Label on the chip,
// Instruction as the turn.
type Starter struct {
	Kind        string `json:"kind" enums:"fill,draft,calendar,sources"`
	Label       string `json:"label"`
	Instruction string `json:"instruction"`
}

// ReadState reads the campaign's state as a turn would, with its starters.
// An unknown campaign is sql.ErrNoRows.
func ReadState(ctx context.Context, repos CampaignAssistantRepos, campaignID string, now time.Time) (*CampaignState, error) {
	c, err := repos.Campaigns.GetByID(ctx, campaignID)
	if err != nil {
		return nil, err
	}
	snap := snapshot(ctx, repos, c, now)
	return &CampaignState{State: snap, Starters: starters(snap)}, nil
}

// starters lead with what the snapshot says is left to do, most pressing
// first: the slots left in the current goal period, the room left under the
// goal, the calendar's problems, and sources not ready to write from.
func starters(s state.Snapshot) []Starter {
	out := []Starter{}
	if g := s.Goal; g != nil && !g.Unknown {
		if p := g.Period; p != nil && p.Left > 0 {
			out = append(out, Starter{Kind: starterFill, Label: "Fill " + p.Label,
				Instruction: fmt.Sprintf("Draft %d %s for %s, the slots left in this goal period.", p.Left, plural(p.Left, "post", "posts"), p.Label)})
		}
		if g.Room > 0 && (g.Period == nil || g.Room != g.Period.Left) {
			out = append(out, Starter{Kind: starterDraft, Label: fmt.Sprintf("Draft %d %s", g.Room, plural(g.Room, "post", "posts")),
				Instruction: fmt.Sprintf("Draft %d %s to reach the campaign's goal.", g.Room, plural(g.Room, "post", "posts"))})
		}
	}
	if cal := s.Calendar; cal != nil && cal.Total > 0 {
		out = append(out, Starter{Kind: starterCalendar, Label: fmt.Sprintf("Fix %d calendar %s", cal.Total, plural(cal.Total, "problem", "problems")),
			Instruction: "Review the campaign's calendar problems and fix them."})
	}
	if src := s.Sources; src != nil && !src.WholeBank && src.NotReady > 0 {
		n := src.NotReady
		out = append(out, Starter{Kind: starterSources, Label: fmt.Sprintf("%d %s not ready", n, plural(n, "source", "sources")),
			Instruction: "Which of the campaign's sources aren't ready to write from, and why?"})
	}
	return out[:min(len(out), maxStarters)]
}
