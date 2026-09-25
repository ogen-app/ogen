package brandresolve

import (
	"context"
	"strings"
	"testing"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
)

// fakeBrandRepo implements repository.BrandRepository; only GetAll is functional
// (the only method Resolve calls). The rest are inert stubs.
type fakeBrandRepo struct{ data *models.BrandData }

func (f *fakeBrandRepo) GetAll(context.Context) (*models.BrandData, error) { return f.data, nil }
func (f *fakeBrandRepo) GetVoice(context.Context, string) (*models.BrandVoice, error) {
	return nil, nil
}
func (f *fakeBrandRepo) CreateVoice(context.Context, *models.BrandVoice) error { return nil }
func (f *fakeBrandRepo) UpdateVoice(context.Context, *models.BrandVoice) error { return nil }
func (f *fakeBrandRepo) DeleteVoice(context.Context, string) (bool, error)     { return false, nil }
func (f *fakeBrandRepo) GetAudience(context.Context, string) (*models.BrandAudience, error) {
	return nil, nil
}
func (f *fakeBrandRepo) CreateAudience(context.Context, *models.BrandAudience) error { return nil }
func (f *fakeBrandRepo) UpdateAudience(context.Context, *models.BrandAudience) error { return nil }
func (f *fakeBrandRepo) DeleteAudience(context.Context, string) (bool, error)        { return false, nil }
func (f *fakeBrandRepo) GetGuardrails(context.Context) (*models.BrandGuardrails, error) {
	return f.data.Guardrails, nil
}
func (f *fakeBrandRepo) SaveGuardrails(context.Context, *models.BrandGuardrails, []string, *string) (repository.FactsReconciled, error) {
	return repository.FactsReconciled{}, nil
}
func (f *fakeBrandRepo) ListFacts(context.Context) ([]models.BrandFact, error) {
	return f.data.Facts, nil
}
func (f *fakeBrandRepo) GetFact(context.Context, string) (*models.BrandFact, error) {
	return nil, nil
}
func (f *fakeBrandRepo) CreateFact(context.Context, *models.BrandFact) error { return nil }
func (f *fakeBrandRepo) UpdateFact(context.Context, *models.BrandFact) (*models.BrandFact, error) {
	return nil, nil
}
func (f *fakeBrandRepo) DeleteFact(context.Context, string) (*models.BrandFact, error) {
	return nil, nil
}
func (f *fakeBrandRepo) GetGuardrailsStance(context.Context) (*models.BrandGuardrailsStanceRecord, error) {
	return nil, nil
}
func (f *fakeBrandRepo) SetGuardrailsStance(context.Context, *models.BrandGuardrailsStanceRecord) (*models.BrandGuardrailsStanceRecord, error) {
	return nil, nil
}
func (f *fakeBrandRepo) DeleteGuardrailsStance(context.Context) error                { return nil }
func (f *fakeBrandRepo) DeleteGuardrails(context.Context) (bool, error)              { return false, nil }
func (f *fakeBrandRepo) GetLook(context.Context) (*models.BrandLook, error)          { return nil, nil }
func (f *fakeBrandRepo) UpsertLook(context.Context, *models.BrandLook) error         { return nil }
func (f *fakeBrandRepo) DeleteLook(context.Context) (bool, error)                    { return false, nil }
func (f *fakeBrandRepo) CreateTemplate(context.Context, *models.BrandTemplate) error { return nil }
func (f *fakeBrandRepo) UpdateTemplate(context.Context, *models.BrandTemplate) error { return nil }
func (f *fakeBrandRepo) DeleteTemplate(context.Context, string) (bool, error)        { return false, nil }

func strptr(s string) *string { return &s }

func data() *models.BrandData {
	return &models.BrandData{
		Voices: []models.BrandVoice{
			{ID: "v-def", Name: "Default", IsDefault: true, Samples: models.StringSlice{"deadpan one-liner"}},
			{ID: "v-camp", Name: "Campaign voice"},
			{ID: "v-post", Name: "Post voice"},
		},
		Audiences: []models.BrandAudience{
			{ID: "a-camp", Name: "Campaign audience", Who: "sceptics"},
			{ID: "a-post", Name: "Post audience", Who: "advisers"},
		},
		Guardrails: &models.BrandGuardrails{
			NeverClaim:  models.StringSlice{"any future return"},
			BannedWords: models.StringSlice{"guaranteed"},
			Disclaimer:  "Capital at risk.",
		},
	}
}

func TestResolveVoicePrecedence(t *testing.T) {
	repo := &fakeBrandRepo{data: data()}
	ctx := t.Context()

	cases := []struct {
		name    string
		camp    *models.Campaign
		post    *models.Post
		wantVID string
	}{
		{"post ref wins", &models.Campaign{BrandVoiceID: strptr("v-camp")}, &models.Post{BrandVoiceID: strptr("v-post")}, "v-post"},
		{"campaign ref when no post ref", &models.Campaign{BrandVoiceID: strptr("v-camp")}, &models.Post{}, "v-camp"},
		{"default voice when no refs", &models.Campaign{}, &models.Post{}, "v-def"},
		{"default voice when post nil", &models.Campaign{}, nil, "v-def"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, err := Resolve(ctx, repo, tc.camp, tc.post)
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if r.Voice == nil || r.Voice.ID != tc.wantVID {
				t.Fatalf("voice = %v, want %s", r.Voice, tc.wantVID)
			}
		})
	}
}

func TestResolveAudiencePrecedence(t *testing.T) {
	repo := &fakeBrandRepo{data: data()}
	ctx := t.Context()

	r, _ := Resolve(ctx, repo, &models.Campaign{BrandAudienceID: strptr("a-camp")}, &models.Post{BrandAudienceID: strptr("a-post")})
	if r.Audience == nil || r.Audience.ID != "a-post" {
		t.Fatalf("post audience ref should win, got %v", r.Audience)
	}
	r, _ = Resolve(ctx, repo, &models.Campaign{BrandAudienceID: strptr("a-camp")}, &models.Post{})
	if r.Audience == nil || r.Audience.ID != "a-camp" {
		t.Fatalf("campaign audience ref should apply, got %v", r.Audience)
	}
	// No audience ref anywhere → nil audience (no workspace default).
	r, _ = Resolve(ctx, repo, &models.Campaign{}, &models.Post{})
	if r.Audience != nil {
		t.Fatalf("expected no audience, got %v", r.Audience)
	}
}

func TestResolveNilRepoFailsOpen(t *testing.T) {
	r, err := Resolve(t.Context(), nil, &models.Campaign{ToneGuidelines: "dry and factual", TargetPersona: "CTOs"}, nil)
	if err != nil {
		t.Fatalf("nil repo should not error: %v", err)
	}
	block := r.PromptBlock("")
	if !strings.Contains(block, "dry and factual") || !strings.Contains(block, "CTOs") {
		t.Fatalf("nil-repo block should carry legacy prose, got:\n%s", block)
	}
}

func TestPromptBlockRendersVoiceAudienceGuardrails(t *testing.T) {
	repo := &fakeBrandRepo{data: data()}
	r, _ := Resolve(t.Context(), repo, &models.Campaign{BrandVoiceID: strptr("v-def"), BrandAudienceID: strptr("a-camp")}, nil)
	block := r.PromptBlock("")

	for _, want := range []string{"deadpan one-liner", "Default", "sceptics", "NEVER claim", "guaranteed", "Capital at risk."} {
		if !strings.Contains(block, want) {
			t.Fatalf("block missing %q:\n%s", want, block)
		}
	}
}

func TestChannelNotesBlock(t *testing.T) {
	r := &Resolved{Voice: &models.BrandVoice{ChannelNotes: models.StringMap{
		"linkedin": "dial it down", "x-twitter": "punchier",
	}}}
	out := r.ChannelNotesBlock([]string{"linkedin", "instagram"})
	if !strings.Contains(out, "linkedin: dial it down") {
		t.Fatalf("expected linkedin note, got: %q", out)
	}
	if strings.Contains(out, "punchier") {
		t.Fatalf("x-twitter note should not render — not in the platform list: %q", out)
	}
	if got := r.ChannelNotesBlock([]string{"tiktok"}); got != "" {
		t.Fatalf("no matching platform should yield empty block, got %q", got)
	}
	if got := (&Resolved{}).ChannelNotesBlock([]string{"linkedin"}); got != "" {
		t.Fatalf("no voice should yield empty block, got %q", got)
	}
}

func TestVoiceIDNilWhenLegacy(t *testing.T) {
	// No brand material and nil repo → VoiceID nil (legacy path), so nothing is
	// stamped on the post.
	r, _ := Resolve(t.Context(), nil, &models.Campaign{}, nil)
	if r.VoiceID() != nil {
		t.Fatalf("expected nil VoiceID on legacy path, got %v", *r.VoiceID())
	}
}
