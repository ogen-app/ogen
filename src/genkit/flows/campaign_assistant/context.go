package campaign_assistant

import (
	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/flowkit"
	"github.com/ogen-app/ogen/src/usecase/campaign_actions/state"
)

// renderState renders the <campaign_state> block for this turn.
func renderState(p *prompts, s state.Snapshot) (string, error) {
	return flowkit.RenderTemplate(p.state, s)
}

// contextTemplateData is the view model of the <campaign> block. It carries
// only fields already loaded on the campaign, so it costs no extra query.
type contextTemplateData struct {
	CampaignName   string
	CampaignID     string
	Status         string
	CampaignType   string
	Language       string
	Description    string
	KeyMessages    string
	ToneGuidelines string
	// BrandBlock is the resolved brand voice/audience/guardrails block,
	// which supersedes the campaign's own persona and tone.
	BrandBlock string
	// Phases lists the campaign type's phases, already hydrated on the
	// campaign.
	Phases []phaseContext
}

// phaseContext is one campaign phase surfaced to the model.
type phaseContext struct {
	Name    string
	Purpose string
}

// renderContext renders the <campaign> block: the brief and the phases, so
// grounded Q&A ("summarise the brief") needs no tool round-trip.
func renderContext(campaign *models.Campaign, brandBlock string, p *prompts) (string, error) {
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
	return flowkit.RenderTemplate(p.context, contextTemplateData{
		CampaignName:   campaign.Name,
		CampaignID:     campaign.ID,
		Status:         string(campaign.Status),
		CampaignType:   campaignType,
		Language:       campaign.Language,
		Description:    campaign.Description,
		KeyMessages:    campaign.KeyMessages,
		ToneGuidelines: campaign.ToneGuidelines,
		BrandBlock:     brandBlock,
		Phases:         phases,
	})
}
