package enrich_brief

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"text/template"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/flowkit"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/usecase/brandresolve"
)

// briefContext holds the rendered prompts ready for the model call.
type briefContext struct {
	SystemPrompt string // system instructions; one variant per mode
	ContextBlock string // campaign + current brief + brand + type + language + instruction
}

// assembleContext renders the prompts from the live campaign. It is rendered
// on every call: the current brief is part of the prompt, and a brief the
// assistant has just written must be what the next edit starts from.
func assembleContext(
	ctx context.Context,
	campaign *models.Campaign,
	instruction string,
	repos EnrichBriefRepos,
	systemTmpl, contextTmpl *template.Template,
) (*briefContext, error) {
	// CampaignRepository.GetByID hydrates CampaignType (with phases). Fall
	// back to a direct lookup if a caller handed us an un-hydrated campaign.
	ct := campaign.CampaignType
	if ct == nil && campaign.CampaignTypeID != "" {
		loaded, err := repos.CampaignTypes.GetByID(ctx, campaign.CampaignTypeID)
		if err != nil {
			return nil, fmt.Errorf("load campaign type: %w", err)
		}
		ct = loaded
	}

	data := contextTemplateData{
		CampaignName: campaign.Name,
		Language:     campaign.Language,
		Instruction:  instruction,
		Brief: currentBrief{
			Description:    strings.TrimSpace(campaign.Description),
			TargetPersona:  strings.TrimSpace(campaign.TargetPersona),
			KeyMessages:    strings.TrimSpace(campaign.KeyMessages),
			ToneGuidelines: strings.TrimSpace(campaign.ToneGuidelines),
		},
		BrandBlock: brandBlock(ctx, repos.Brands, campaign),
	}
	data.HasBrief = data.Brief != currentBrief{}
	if ct != nil {
		data.TypeName = ct.Name
		data.TypeLabel = ct.Label
		data.TypeDescription = ct.Description
		data.Phases = make([]phaseInfo, 0, len(ct.Phases))
		for _, p := range ct.Phases {
			data.Phases = append(data.Phases, phaseInfo{
				Sequence: p.Sequence,
				Name:     p.Name,
				Purpose:  p.Purpose,
			})
		}
	}

	systemPrompt, err := flowkit.RenderTemplate(systemTmpl, data)
	if err != nil {
		return nil, fmt.Errorf("render system prompt: %w", err)
	}
	contextBlock, err := flowkit.RenderTemplate(contextTmpl, data)
	if err != nil {
		return nil, fmt.Errorf("render context block: %w", err)
	}

	return &briefContext{
		SystemPrompt: systemPrompt,
		ContextBlock: contextBlock,
	}, nil
}

// brandBlock renders the workspace brand material the brief must stay inside.
// The campaign's own tone guidelines and persona are already in the current
// brief, so the legacy fallback is cleared to avoid rendering them twice.
// Resolution fails open: a brand lookup error drops the block, not the call.
func brandBlock(ctx context.Context, brands repository.BrandRepository, campaign *models.Campaign) string {
	brand, err := brandresolve.Resolve(ctx, brands, campaign, nil)
	if err != nil {
		slog.WarnContext(ctx, "brand resolve failed; enriching without brand voice",
			logging.AttrComponent, logComponent, "campaign_id", campaign.ID, logging.AttrError, err)
	}
	brand.LegacyTone = ""
	brand.LegacyPersona = ""
	return brand.PromptBlock("")
}

type contextTemplateData struct {
	CampaignName    string
	TypeName        string
	TypeLabel       string
	TypeDescription string
	Phases          []phaseInfo
	Language        string
	Instruction     string
	// HasBrief selects edit mode: any non-empty brief field is edited in
	// place rather than regenerated from the campaign type.
	HasBrief   bool
	Brief      currentBrief
	BrandBlock string
}

// currentBrief is the brief as stored on the campaign.
type currentBrief struct {
	Description    string
	TargetPersona  string
	KeyMessages    string
	ToneGuidelines string
}

type phaseInfo struct {
	Sequence int
	Name     string
	Purpose  string
}
