package enrich_brief

import (
	"context"
	"errors"
	"slices"
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

type sourceAssets struct {
	repository.AssetRepository
	all []models.Asset
}

func (f sourceAssets) ListMeta(_ context.Context, ids []string) ([]models.Asset, error) {
	var out []models.Asset
	for _, a := range f.all {
		if slices.Contains(ids, a.ID) {
			out = append(out, a)
		}
	}
	return out, nil
}

type sourceLenses struct {
	repository.AssetAnalysisRepository
	digests map[string]repository.LensDigest
	lens    *string
}

func (f *sourceLenses) LensDigests(_ context.Context, _ []string, campaignID *string) (map[string]repository.LensDigest, error) {
	f.lens = campaignID
	return f.digests, nil
}

// The brief is given the campaign's analysed sources, by key, through the
// campaign's lens; a pending analysis or one with nothing to say is left
// out. The keys the model names back are the sources it drew on.
func TestAssembleContext_Sources(t *testing.T) {
	c := retentionCampaign()
	c.AssetIDs = models.StringSlice{"a1", "a2", "a3"}
	lenses := &sourceLenses{digests: map[string]repository.LensDigest{
		"a1": {ID: "an1", Status: models.AssetAnalysisStatusReady, Gist: "Reopens 14 March with a rooftop bar."},
		"a2": {ID: "an2", Status: models.AssetAnalysisStatusPending},
		"a3": {ID: "an3", Status: models.AssetAnalysisStatusReady, SummaryHead: "## Spa\n\nThe spa opens at ten. More follows."},
	}}
	repos := EnrichBriefRepos{
		Assets:   sourceAssets{all: []models.Asset{{ID: "a1", Title: "Brochure"}, {ID: "a2", Title: "Press kit"}, {ID: "a3", Title: "Spa menu"}}},
		Analyses: lenses,
	}
	system, ctxTmpl := templates(t)
	bctx, err := assembleContext(t.Context(), c, "", repos, system, ctxTmpl)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"## The campaign's sources", "- [S1] Brochure: Reopens 14 March with a rooftop bar.", "- [S2] Spa menu: The spa opens at ten."} {
		if !strings.Contains(bctx.ContextBlock, want) {
			t.Errorf("context lacks %q:\n%s", want, bctx.ContextBlock)
		}
	}
	if strings.Contains(bctx.ContextBlock, "Press kit") || lenses.lens == nil || *lenses.lens != "c1" {
		t.Fatalf("pending source listed, or lens %v", lenses.lens)
	}
	got := usedSources(`"S2", s9`, bctx.Sources)
	if len(got) != 1 || got[0] != (Source{ID: "a3", Title: "Spa menu", AnalysisID: "an3"}) {
		t.Fatalf("used = %+v", got)
	}
	if usedSources("", bctx.Sources) != nil {
		t.Fatal("no keys named, yet sources used")
	}

	// Without sources the block is left out.
	bare, err := assembleContext(t.Context(), retentionCampaign(), "", EnrichBriefRepos{}, system, ctxTmpl)
	if err != nil || strings.Contains(bare.ContextBlock, "campaign's sources") {
		t.Fatalf("bare context = %v / %s", err, bare.ContextBlock)
	}
}
