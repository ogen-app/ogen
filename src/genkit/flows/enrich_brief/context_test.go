package enrich_brief

import (
	"context"
	"errors"
	"strings"
	"testing"
	"text/template"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
)

// fakeBrands serves GetAll, the only method brandresolve.Resolve calls.
type fakeBrands struct {
	repository.BrandRepository
	data *models.BrandData
	err  error
}

func (f fakeBrands) GetAll(context.Context) (*models.BrandData, error) { return f.data, f.err }

func templates(t *testing.T) (system, contextTmpl *template.Template) {
	t.Helper()
	raw, err := promptFS.ReadFile("prompts/enrich_brief.tmpl")
	if err != nil {
		t.Fatal(err)
	}
	tmpl := template.Must(template.New("enrich_brief").Parse(string(raw)))
	return tmpl.Lookup("system"), tmpl.Lookup("context")
}

func retentionCampaign() *models.Campaign {
	return &models.Campaign{
		ID:             "c1",
		Name:           "Practitioner AI",
		Language:       "en-GB",
		CampaignTypeID: "t1",
		CampaignType: &models.CampaignType{
			Name:  "retention",
			Label: "Retention",
			Phases: []models.CampaignTypePhase{
				{Sequence: 1, Name: "Activate & Embed"},
				{Sequence: 2, Name: "Deepen & Expand"},
			},
		},
	}
}

func render(t *testing.T, c *models.Campaign, instruction string, repos EnrichBriefRepos) *briefContext {
	t.Helper()
	system, contextTmpl := templates(t)
	bctx, err := assembleContext(context.Background(), c, instruction, repos, system, contextTmpl)
	if err != nil {
		t.Fatal(err)
	}
	return bctx
}

func TestAssembleContext_EmptyBriefGeneratesFromType(t *testing.T) {
	bctx := render(t, retentionCampaign(), "", EnrichBriefRepos{})

	if !strings.Contains(bctx.SystemPrompt, "draft a complete, on-brand campaign brief") {
		t.Errorf("empty brief should use generate mode:\n%s", bctx.SystemPrompt)
	}
	if strings.Contains(bctx.ContextBlock, "## Current brief") {
		t.Errorf("empty brief should not render a current brief:\n%s", bctx.ContextBlock)
	}
	if !strings.Contains(bctx.ContextBlock, "1. **Activate & Embed**") {
		t.Errorf("phases missing:\n%s", bctx.ContextBlock)
	}
}

func TestAssembleContext_ExistingBriefIsEdited(t *testing.T) {
	c := retentionCampaign()
	c.Description = "Position Alec as a practitioner's voice on AI integration."
	c.KeyMessages = "We ship, we don't speculate."
	c.ToneGuidelines = "Anti-hype. No second-person address. British English."

	bctx := render(t, c, "Tighten it; keep the voice.", EnrichBriefRepos{})

	if !strings.Contains(bctx.SystemPrompt, "Improve that brief — do not replace it") {
		t.Errorf("non-empty brief should use edit mode:\n%s", bctx.SystemPrompt)
	}
	for _, want := range []string{
		"## Current brief",
		c.Description,
		c.KeyMessages,
		c.ToneGuidelines,
		"### targetPersona\n(empty)",
		"## Additional instruction\nTighten it; keep the voice.",
	} {
		if !strings.Contains(bctx.ContextBlock, want) {
			t.Errorf("context block missing %q:\n%s", want, bctx.ContextBlock)
		}
	}
}

func TestAssembleContext_BrandBlockWithoutLegacyDuplicates(t *testing.T) {
	c := retentionCampaign()
	c.ToneGuidelines = "Anti-hype."
	c.TargetPersona = "CTOs"
	brands := fakeBrands{data: &models.BrandData{
		Voices:     []models.BrandVoice{{ID: "v1", Name: "Practitioner", IsDefault: true}},
		Guardrails: &models.BrandGuardrails{BannedWords: models.StringSlice{"revolutionary"}},
	}}

	bctx := render(t, c, "", EnrichBriefRepos{Brands: brands})

	for _, want := range []string{"## Brand voice — write in this voice\n**Practitioner**", "revolutionary"} {
		if !strings.Contains(bctx.ContextBlock, want) {
			t.Errorf("context block missing %q:\n%s", want, bctx.ContextBlock)
		}
	}
	for _, unwanted := range []string{"## Tone guidelines", "## Target persona"} {
		if strings.Contains(bctx.ContextBlock, unwanted) {
			t.Errorf("legacy fallback %q rendered alongside the current brief:\n%s", unwanted, bctx.ContextBlock)
		}
	}
}

func TestAssembleContext_BrandFailureFailsOpen(t *testing.T) {
	c := retentionCampaign()
	c.ToneGuidelines = "Anti-hype."

	bctx := render(t, c, "", EnrichBriefRepos{Brands: fakeBrands{err: errors.New("db down")}})

	if strings.Contains(bctx.ContextBlock, "## Brand voice") {
		t.Errorf("brand block rendered despite resolve failure:\n%s", bctx.ContextBlock)
	}
	if !strings.Contains(bctx.ContextBlock, "Anti-hype.") {
		t.Errorf("current brief missing:\n%s", bctx.ContextBlock)
	}
}
