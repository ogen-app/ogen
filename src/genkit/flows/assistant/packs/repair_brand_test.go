package packs

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"testing"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/domain/viewcontext"
	"github.com/ogen-app/ogen/src/genkit/flows/brand_check"
)

// brandPosts serves memPosts' posts by id too.
type brandPosts struct{ memPosts }

func (r *brandPosts) GetByID(_ context.Context, id string) (*models.Post, error) {
	for _, p := range r.posts {
		if p.ID == id {
			return &p, nil
		}
	}
	return nil, sql.ErrNoRows
}

func TestCheckBrand(t *testing.T) {
	voices := []models.BrandVoice{{ID: "v1", Name: "Plain", IsDefault: true}, {ID: "v2", Name: "Formal"}}
	brand := &memBrand{facts: map[string]*models.BrandFact{}, data: &models.BrandData{Voices: voices,
		Guardrails: &models.BrandGuardrails{BannedWords: models.StringSlice{"cheap"}}}}
	posts := &brandPosts{memPosts{posts: []models.Post{
		{ID: "p1", CampaignID: "c1", Title: "Rooms", Content: "Cheap rooms!", Status: models.PostStatusDraft},
		{ID: "p2", CampaignID: "c1", Content: "Formal note", Status: models.PostStatusDraft, BrandVoiceID: new("v2")},
		{ID: "p3", CampaignID: "c1", Content: "Out already", Status: models.PostStatusPublished},
		{ID: "p4", CampaignID: "c1", Content: " ", Status: models.PostStatusPlanned},
	}}}
	var got brand_check.Request
	d := Deps{Brand: brand, Posts: posts, Campaigns: &memCampaigns{c: &models.Campaign{ID: "c1"}},
		CheckBrand: func(_ context.Context, req brand_check.Request) (*brand_check.Result, error) {
			got = req
			return &brand_check.Result{Checked: 2, Findings: []brand_check.Finding{
				{PostID: "p2", Rule: "voice", Severity: "low", Issue: "A bit loose."},
				{PostID: "p1", Rule: "banned_word", Severity: "high", Quote: "Cheap", Issue: "Banned."},
			}}, nil
		}}
	ctx, _ := turn(t, campaign1, nil)

	out, err := checkBrand(ctx, d, CheckBrandInput{})
	if err != nil {
		t.Fatal(err)
	}
	// The campaign's unpublished, written posts, grouped by the voice each
	// was written in.
	if len(got.Groups) != 2 || got.Groups[0].Posts[0].ID != "p1" || got.Groups[1].Posts[0].ID != "p2" ||
		!slices.Equal(got.Groups[0].BannedWords, []string{"cheap"}) {
		t.Fatalf("request = %+v", got)
	}
	if len(out.Findings) != 2 || out.Findings[0].PostID != "p1" || out.Findings[0].Title != "Rooms" ||
		out.Findings[0].Fix.PostID != "p1" || out.Findings[0].Fix.Kind != "open_post" || out.Findings[0].CampaignID != "c1" {
		t.Fatalf("findings = %+v; want the high one first, opening its post", out.Findings)
	}

	// Named posts; in a post's thread, that post.
	if _, err := checkBrand(ctx, d, CheckBrandInput{PostIDs: []string{"p2", "p2"}}); err != nil || len(got.Groups) != 1 || len(got.Groups[0].Posts) != 1 {
		t.Fatalf("named: %+v, %v", got, err)
	}
	post, _ := turn(t, viewcontext.Scope{Kind: models.AssistantScopePost, ID: "p1"}, nil)
	if _, err := checkBrand(post, d, CheckBrandInput{}); err != nil || got.Groups[0].Posts[0].ID != "p1" {
		t.Fatalf("post thread: %+v, %v", got, err)
	}
	_, err = checkBrand(ctx, d, CheckBrandInput{PostIDs: []string{"gone"}})
	refused(t, err)

	// Past the cap, the rest are counted, not read.
	posts.posts = nil
	for i := range maxBrandCheck + 3 {
		posts.posts = append(posts.posts, models.Post{ID: fmt.Sprint("q", i), CampaignID: "c1", Content: "x", Status: models.PostStatusDraft})
	}
	if out, err := checkBrand(ctx, d, CheckBrandInput{}); err != nil || out.Left != 3 || len(got.Groups[0].Posts) != maxBrandCheck {
		t.Fatalf("capped: %+v, %v", out, err)
	}
}
