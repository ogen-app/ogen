package asset_analysis

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/ogen-app/ogen/src/domain/ideafit"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/flowkit"
)

func renderSystem(t *testing.T, campaign, phases bool) string {
	t.Helper()
	tpl, err := loadPrompts()
	if err != nil {
		t.Fatal(err)
	}
	out, err := flowkit.RenderTemplate(tpl.system, systemData{Campaign: campaign, Rubric: rubric(campaign, phases)})
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv("PRINT_PROMPTS") != "" {
		t.Log(out)
	}
	return out
}

// exampleIdea is the idea object of the response format the prompt shows.
func exampleIdea(t *testing.T, system string) Idea {
	t.Helper()
	_, after, ok := strings.Cut(system, `"ideas": [`)
	if !ok {
		t.Fatal("system prompt has no ideas example")
	}
	line, _, _ := strings.Cut(after, "\n  \"keyPoints\"")
	line = strings.TrimSuffix(strings.TrimSpace(line), "],")
	var idea Idea
	if err := json.Unmarshal([]byte(line), &idea); err != nil {
		t.Fatalf("ideas example is not valid JSON: %v\n%s", err, line)
	}
	return idea
}

func TestSystemPromptRubricPerLens(t *testing.T) {
	tests := []struct {
		name             string
		campaign, phases bool
		want, absent     []string
	}{
		{
			name:     "campaign with phases",
			campaign: true, phases: true,
			want:   []string{"message_fit", "phase_fit", "timeliness", `"phase": "…"`, "off_brief", "guardrail_breach", "no key messages"},
			absent: []string{"brand_relevance", "campaign_potential", "evergreen"},
		},
		{
			name:     "campaign without phases",
			campaign: true,
			want:     []string{"message_fit", "timeliness", "off_brief"},
			absent:   []string{"phase_fit", `"phase"`},
		},
		{
			name:   "workspace",
			want:   []string{"brand_relevance", "campaign_potential", "evergreen", `"campaign": "…"`, "unsupported", "no active campaigns"},
			absent: []string{"message_fit", "phase_fit", "timeliness", "off_brief"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			system := renderSystem(t, tt.campaign, tt.phases)
			for _, s := range tt.want {
				if !strings.Contains(system, s) {
					t.Errorf("system prompt is missing %q", s)
				}
			}
			for _, s := range tt.absent {
				if strings.Contains(system, s) {
					t.Errorf("system prompt carries %q", s)
				}
			}
			if !strings.Contains(system, "5 = ") || !strings.Contains(system, "1 = ") {
				t.Error("system prompt lacks the level anchors")
			}

			// The response format must list exactly the criteria the lens rates.
			kind := ideafit.LensWorkspace
			if tt.campaign {
				kind = ideafit.LensCampaign
			}
			idea := exampleIdea(t, system)
			if idea.Fit == nil {
				t.Fatal("ideas example has no fit")
			}
			criteria := ideafit.Criteria(kind, tt.phases)
			if len(idea.Fit.Criteria) != len(criteria) {
				t.Errorf("example rates %d criteria, want %d", len(idea.Fit.Criteria), len(criteria))
			}
			for _, c := range criteria {
				if _, ok := idea.Fit.Criteria[c.Key]; !ok {
					t.Errorf("example is missing %s", c.Key)
				}
			}
		})
	}
}

func TestLensPromptCarriesDatesAndActiveCampaigns(t *testing.T) {
	tpl, err := loadPrompts()
	if err != nil {
		t.Fatal(err)
	}
	render := func(l Lens) string {
		t.Helper()
		out, err := flowkit.RenderTemplate(tpl.lens, lensData{Lens: l})
		if err != nil {
			t.Fatal(err)
		}
		return out
	}

	ws := render(Lens{
		WorkspaceName:   "Acme",
		ActiveCampaigns: []CampaignHint{{Name: "Spring launch", Description: "New app"}, {Name: "Hiring"}},
		Today:           "2026-10-16",
	})
	for _, want := range []string{"## Active campaigns", "- **Spring launch**: New app", "- **Hiring**\n", "## Today\n2026-10-16"} {
		if !strings.Contains(ws+"\n", want) {
			t.Errorf("workspace lens is missing %q:\n%s", want, ws)
		}
	}

	for _, tt := range []struct {
		start, end, want string
	}{
		{"2026-11-01", "2026-11-30", "**Runs**: from 2026-11-01 until 2026-11-30"},
		{"2026-11-01", "", "**Runs**: from 2026-11-01\n"},
		{"", "2026-11-30", "**Runs**: until 2026-11-30"},
	} {
		got := render(Lens{Campaign: &CampaignBrief{Name: "Launch", StartDate: tt.start, EndDate: tt.end}})
		if !strings.Contains(got, tt.want) {
			t.Errorf("campaign lens is missing %q:\n%s", tt.want, got)
		}
	}
	if got := render(Lens{Campaign: &CampaignBrief{Name: "Launch"}}); strings.Contains(got, "Runs") || strings.Contains(got, "Active campaigns") {
		t.Errorf("undated campaign lens = %s", got)
	}
}

func TestIdeaFitDecodesWhatItCan(t *testing.T) {
	var idea Idea
	err := json.Unmarshal([]byte(`{"title": "T", "fit": {"criteria": {
		"evidence": {"level": 4.4, "why": "stat"},
		"gap_fill": {"level": "high"},
		"brand_fit": {"level": 1e300}
	}, "caps": ["unsupported", 7]}}`), &idea)
	if err != nil {
		t.Fatalf("a malformed rating failed the decode: %v", err)
	}
	f := idea.Fit
	if f == nil || len(f.Criteria) != 2 || f.Criteria["evidence"].Level != 4 || f.Criteria["brand_fit"].Level != 6 {
		t.Fatalf("fit = %+v", f)
	}
	if len(f.Caps) != 1 || f.Caps[0] != ideafit.CapUnsupported {
		t.Errorf("caps = %v", f.Caps)
	}

	for _, raw := range []string{`{"fit": "great"}`, `{"fit": [1, 2]}`, `{"fit": {"criteria": "x", "caps": "y"}}`} {
		idea = Idea{}
		if err := json.Unmarshal([]byte(raw), &idea); err != nil {
			t.Errorf("%s: decode failed: %v", raw, err)
		}
		if idea.Fit == nil || len(idea.Fit.Criteria) != 0 || len(idea.Fit.Caps) != 0 {
			t.Errorf("%s: fit = %+v, want empty", raw, idea.Fit)
		}
	}
}
