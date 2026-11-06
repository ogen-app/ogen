package campaign_assistant

import (
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/flowkit"
	"github.com/ogen-app/ogen/src/usecase/campaign_actions/state"
)

// assistantContext holds the rendered prompts ready for the model call.
type assistantContext struct {
	SystemPrompt string // stable system instructions (cacheable prefix)
	ContextBlock string // the campaign's current brief (stable within a turn)
	// StateBlock is the per-turn campaign state prepended to the user's
	// message.
	StateBlock string
}

// renderState renders the <campaign_state> block for this turn.
func renderState(p *prompts, s state.Snapshot, open []openProposal) (string, error) {
	return flowkit.RenderTemplate(p.state, stateTemplateData{Snapshot: s, OpenProposals: open})
}

// contextTemplateData is the view model for the prompt template. It carries
// only fields already loaded on the campaign, so context assembly costs no
// extra query — the planner stays fast (CON-112 execution-time optimisation).
type contextTemplateData struct {
	CampaignName   string
	Status         string
	CampaignType   string
	Language       string
	Description    string
	TargetPersona  string
	KeyMessages    string
	ToneGuidelines string
	// BrandBlock is the resolved brand voice/audience/guardrails block,
	// which supersedes TargetPersona/ToneGuidelines in the template.
	BrandBlock string
	// Today is the current date (YYYY-MM-DD), so the planner can resolve
	// relative timeframes like "upcoming weeks" into concrete dates.
	Today string
	// Phases lists the campaign type's phases. Already hydrated on
	// the campaign, so surfacing them here costs no extra query.
	Phases []phaseContext
}

// stateTemplateData is the campaign as it is at the start of a turn. It
// changes from turn to turn, so it opens the user's message instead of
// living in the cached system prompt.
type stateTemplateData struct {
	state.Snapshot
	// OpenProposals are offers waiting for the user's answer, so a "yes"
	// in this turn can be matched to one.
	OpenProposals []openProposal
}

// openProposal is one pending offer surfaced to the planner.
type openProposal struct {
	ID      string
	Summary string
}

// phaseContext is one campaign phase surfaced to the planner.
type phaseContext struct {
	Name    string
	Purpose string
}

// assembleContext pairs the pre-rendered system prompt with the context block
// rendered from the campaign. The brief is placed in the context block so
// grounded Q&A ("summarise the brief") needs no tool round-trip. The rendered
// strings are deterministic, so an unchanged brief and date produce a
// byte-identical system prefix across the turns of a conversation. That determinism is what makes the ephemeral
// cache_control breakpoint (added at the wire for the planning model — see
// server.InstallAnthropicToolOrderStabilizer) actually hit: without an explicit
// breakpoint Anthropic caches nothing at the token level. The dynamic context
// (today's date, the live brief) is concatenated after the static prompt, so
// the cached prefix rotates daily and on brief edits — fine, since consecutive
// turns within the cache TTL share it. Note this is separate from the
// strict-tool grammar cache the tool-order stabilizer keeps warm.
func assembleContext(campaign *models.Campaign, brandBlock string, today time.Time, p *prompts) (*assistantContext, error) {
	campaignType := campaign.CampaignTypeID
	var phases []phaseContext
	if campaign.CampaignType != nil {
		if campaign.CampaignType.Name != "" {
			campaignType = campaign.CampaignType.Name
		}
		// Phases are hydrated in sequence order (repository/campaign_types.go).
		phases = make([]phaseContext, 0, len(campaign.CampaignType.Phases))
		for _, ph := range campaign.CampaignType.Phases {
			phases = append(phases, phaseContext{Name: ph.Name, Purpose: ph.Purpose})
		}
	}

	data := contextTemplateData{
		CampaignName:   campaign.Name,
		Status:         string(campaign.Status),
		CampaignType:   campaignType,
		Language:       campaign.Language,
		Description:    campaign.Description,
		TargetPersona:  campaign.TargetPersona,
		KeyMessages:    campaign.KeyMessages,
		ToneGuidelines: campaign.ToneGuidelines,
		BrandBlock:     brandBlock,
		Today:          today.Format("2006-01-02"),
		Phases:         phases,
	}

	contextBlock, err := flowkit.RenderTemplate(p.context, data)
	if err != nil {
		return nil, err
	}
	return &assistantContext{SystemPrompt: p.system, ContextBlock: contextBlock}, nil
}
