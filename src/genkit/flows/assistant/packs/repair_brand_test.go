package packs

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/domain/viewcontext"
	"github.com/ogen-app/ogen/src/genkit/flows/brand_check"
	"github.com/ogen-app/ogen/src/genkit/flows/consistency"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/assistantkit"
	"github.com/ogen-app/ogen/src/usecase/campaign_actions/calendarcheck"
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
	if len(out.Findings) != 2 || out.Findings[0].PostID != "p1" || out.Findings[0].Title != "Rooms" || len(out.Findings[0].Fixes) != 1 ||
		out.Findings[0].Fixes[0].PostID != "p1" || out.Findings[0].Fixes[0].Kind != "open_post" || out.Findings[0].CampaignID != "c1" {
		t.Fatalf("findings = %+v; want the high one first, opening its post", out.Findings)
	}
	if want := []BrandRef{{ID: "v1", Name: "Plain"}, {ID: "v2", Name: "Formal"}}; out.Clean != 0 ||
		!slices.Equal(out.CheckedAgainst.Voices, want) || !out.CheckedAgainst.Guardrails {
		t.Fatalf("clean = %d, against = %+v", out.Clean, out.CheckedAgainst)
	}
	if k := out.Findings[0].Key; !strings.HasPrefix(k, "brand_banned_word:") || k == out.Findings[1].Key {
		t.Fatalf("keys = %q, %q", k, out.Findings[1].Key)
	}
	// The key holds however the issue is worded; another quote is another
	// break.
	p1 := &posts.posts[0]
	same := brand_check.Finding{PostID: "p1", Rule: "banned_word", Quote: " cheap ", Issue: "Uses a banned word."}
	if brandFindingKey(p1, same) != out.Findings[0].Key {
		t.Fatal("rewording the issue changed the key")
	}
	if same.Quote = "rooms"; brandFindingKey(p1, same) == out.Findings[0].Key {
		t.Fatal("another quote kept the key")
	}

	// With the writer, a finding offers a rewrite first; a dismissed one is
	// left out and its post counts as clean.
	dismissals := &memDismissals{keys: map[string]bool{out.Findings[1].Key: true}}
	d.Dismissals = dismissals
	var revised []string
	actions := &memActions{}
	d.Actions = actions
	d.Revise = func(_ context.Context, id, instruction, _ string) (Revised, error) {
		revised = append(revised, id+": "+instruction)
		return Revised{Content: "Rooms, kindly priced.", Status: models.PostStatusDraft, Versioned: true}, nil
	}
	sink := &offers{}
	out, err = checkBrand(assistantkit.WithProposalSink(ctx, sink), d, CheckBrandInput{})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Findings) != 1 || out.Dismissed != 1 || out.Clean != 1 {
		t.Fatalf("findings = %+v, dismissed %d, clean %d", out.Findings, out.Dismissed, out.Clean)
	}
	fixes := out.Findings[0].Fixes
	if len(fixes) != 2 || fixes[0].Kind != consistency.FixRevisePost || fixes[0].ProposalID == "" || fixes[1].Kind != calendarcheck.FixOpenPost {
		t.Fatalf("fixes = %+v", fixes)
	}
	rewrite := sink.got[0].Args.(RevisePostInput)
	done, err := revisePost(ctx, d, rewrite)
	if err != nil || !slices.Equal(revised, []string{"p1: Bring the post in line with the brand. The issue: Banned."}) {
		t.Fatalf("revised = %v, %v", revised, err)
	}
	// The rewrite names its post and voice and can be undone.
	if done.Title != "Rooms" || done.VoiceName != "Plain" || done.CampaignID != "c1" || done.UndoID == "" || !strings.Contains(done.Note, "undone") {
		t.Fatalf("rewrite = %+v", done)
	}
	// The undo logs the copy the rewrite replaced and the copy it wrote.
	if a := actions.rows[0]; a.Kind != models.AssistantActionPostRewritten || a.Before["content"] != "Cheap rooms!" || a.After["content"] != "Rooms, kindly priced." {
		t.Fatalf("undo log = %+v", a)
	}
	// Without an action log nothing promises an undo.
	d.Actions = nil
	if done, err := revisePost(ctx, d, rewrite); err != nil || done.UndoID != "" || strings.Contains(done.Note, "undone") {
		t.Fatalf("no log = %+v, %v", done, err)
	}
	d.Revise, d.Dismissals = nil, nil

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
