package enrich_brief

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"text/template"
	"unicode"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/flowkit"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/kernel/textgist"
	"github.com/ogen-app/ogen/src/usecase/brandresolve"
)

// briefContext holds the rendered prompts ready for the model call.
type briefContext struct {
	SystemPrompt string // system instructions; one variant per mode
	ContextBlock string // campaign + current brief + brand + sources + type + language + instruction
	// Sources are the ones the context block lists, by their key.
	Sources []briefSource
}

// briefSource is one of the campaign's analysed assets as the prompt lists
// it: Key ("S1") is how the model names it back.
type briefSource struct {
	Source
	Key  string
	Gist string
}

// maxBriefSources bounds the sources a brief is given, in the campaign's
// order.
const maxBriefSources = 8

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
		Sources:    briefSources(ctx, repos, campaign),
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
		Sources:      data.Sources,
	}, nil
}

// briefSources are the campaign's attached assets whose analysis for the
// campaign is finished, each with its gist. It fails open: a read error
// leaves the sources out, not the call.
func briefSources(ctx context.Context, repos EnrichBriefRepos, campaign *models.Campaign) []briefSource {
	if repos.Assets == nil || repos.Analyses == nil || len(campaign.AssetIDs) == 0 {
		return nil
	}
	digests, err := repos.Analyses.LensDigests(ctx, campaign.AssetIDs, &campaign.ID)
	if err != nil {
		slog.WarnContext(ctx, "read sources failed; enriching without them",
			logging.AttrComponent, logComponent, "campaign_id", campaign.ID, logging.AttrError, err)
		return nil
	}
	metas, err := repos.Assets.ListMeta(ctx, campaign.AssetIDs)
	if err != nil {
		slog.WarnContext(ctx, "read sources failed; enriching without them",
			logging.AttrComponent, logComponent, "campaign_id", campaign.ID, logging.AttrError, err)
		return nil
	}
	titles := make(map[string]string, len(metas))
	for _, m := range metas {
		titles[m.ID] = m.Title
	}
	var out []briefSource
	for _, id := range campaign.AssetIDs {
		d, ok := digests[id]
		title, known := titles[id]
		if !ok || !known || d.Status != models.AssetAnalysisStatusReady {
			continue
		}
		gist := cmp.Or(d.Gist, textgist.FromSummary(d.SummaryHead))
		if gist == "" {
			continue
		}
		out = append(out, briefSource{Source: Source{ID: id, Title: title, AnalysisID: d.ID}, Key: fmt.Sprintf("S%d", len(out)+1), Gist: gist})
		if len(out) == maxBriefSources {
			break
		}
	}
	return out
}

// usedSources are the sources the model named, by key, in the order given;
// a key it made up is ignored.
func usedSources(named string, given []briefSource) []Source {
	keys := strings.FieldsFunc(strings.ToUpper(named), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
	var out []Source
	for _, s := range given {
		if slices.Contains(keys, s.Key) {
			out = append(out, s.Source)
		}
	}
	return out
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
	Sources    []briefSource
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
