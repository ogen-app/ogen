package packs

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/domain/viewcontext"
	"github.com/ogen-app/ogen/src/genkit/flows/brand_check"
	"github.com/ogen-app/ogen/src/genkit/flows/consistency"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/assistantkit"
	"github.com/ogen-app/ogen/src/infra/repository"
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
	first := &offers{}
	ctx = assistantkit.WithProposalSink(ctx, first)

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
	// p2 is in a voice of its own, so its voice finding offers the
	// campaign's, which is the workspace's default, without rewriting it.
	if fixes := out.Findings[1].Fixes; len(fixes) != 2 || fixes[0].Kind != FixUseVoice || fixes[0].Title != "Use Plain, the campaign's voice" ||
		fixes[0].ProposalID == "" || fixes[0].Card == nil || fixes[0].Card.Rows[0].Note != "Formal → Plain" {
		t.Fatalf("p2 fixes = %+v", fixes)
	}
	if args := first.got[0].Args.(SetPostVoiceInput); first.got[0].Tool != "setPostVoice" || args.PostID != "p2" || args.VoiceID != "" {
		t.Fatalf("offer = %+v", first.got[0])
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

// editedVersions names the posts a person has saved copy for.
type editedVersions struct {
	repository.PostVersionRepository
	edited []string
	asked  []string
}

func (v *editedVersions) EditedPostIDs(_ context.Context, ids []string) ([]string, error) {
	v.asked = append(v.asked, ids...)
	return slices.DeleteFunc(slices.Clone(ids), func(id string) bool { return !slices.Contains(v.edited, id) }), nil
}

// A finding's alternatives follow the rewrite: a guardrail broken by a dated
// post takes it off the calendar, a generated draft no one has edited can be
// deleted after asking, and opening the post comes last.
func TestCheckBrandAlternativeFixes(t *testing.T) {
	at := time.Date(2026, 12, 1, 9, 0, 0, 0, time.UTC)
	planned := new(models.GroundingGrounded)
	brand := &memBrand{facts: map[string]*models.BrandFact{}, data: &models.BrandData{
		Voices: []models.BrandVoice{{ID: "v1", Name: "Plain", IsDefault: true}}}}
	posts := &brandPosts{memPosts{posts: []models.Post{
		{ID: "sched", CampaignID: "c1", Content: "Cheap!", Status: models.PostStatusScheduled, ScheduledAt: &at},
		{ID: "fresh", CampaignID: "c1", Content: "Hey", Status: models.PostStatusDraft, GroundingMode: planned},
		{ID: "edited", CampaignID: "c1", Content: "Cheap rooms", Status: models.PostStatusDraft, GroundingMode: planned, ScheduledAt: &at},
		{ID: "byhand", CampaignID: "c1", Content: "Hi", Status: models.PostStatusDraft},
		{ID: "voice", CampaignID: "c1", Content: "Yo", Status: models.PostStatusReadyForPublish, ScheduledAt: &at},
	}}}
	versions := &editedVersions{edited: []string{"edited"}}
	d := Deps{Brand: brand, Posts: posts, Campaigns: &memCampaigns{c: &models.Campaign{ID: "c1"}},
		Versions: versions, Remove: &memRemover{posts: &posts.memPosts},
		Revise: func(context.Context, string, string, string) (Revised, error) { return Revised{}, nil },
		CheckBrand: func(context.Context, brand_check.Request) (*brand_check.Result, error) {
			return &brand_check.Result{Checked: 5, Findings: []brand_check.Finding{
				{PostID: "sched", Rule: brand_check.RuleBannedWord, Severity: "high", Quote: "Cheap", Issue: "Banned."},
				{PostID: "fresh", Rule: brand_check.RuleVoice, Severity: "medium", Issue: "Too loose."},
				{PostID: "edited", Rule: brand_check.RuleBannedWord, Severity: "medium", Quote: "Cheap", Issue: "Banned."},
				{PostID: "byhand", Rule: brand_check.RuleVoice, Severity: "low", Issue: "Too loose."},
				{PostID: "voice", Rule: brand_check.RuleVoice, Severity: "low", Issue: "Too loose."},
			}}, nil
		}}
	ctx, _ := turn(t, campaign1, nil)
	sink := &offers{}
	out, err := checkBrand(assistantkit.WithProposalSink(ctx, sink), d, CheckBrandInput{})
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string][]string{}
	for _, f := range out.Findings {
		for _, fix := range f.Fixes {
			kinds[f.PostID] = append(kinds[f.PostID], string(fix.Kind))
		}
	}
	want := map[string][]string{
		// Its copy is locked, so it can't be rewritten.
		"sched":  {"unschedule", "open_post"},
		"fresh":  {"revise_post", "delete", "open_post"},
		"edited": {"revise_post", "unschedule", "open_post"},
		"byhand": {"revise_post", "open_post"},
		// The voice isn't a guardrail: a dated post stays on the calendar.
		"voice": {"revise_post", "open_post"},
	}
	for id, w := range want {
		if !slices.Equal(kinds[id], w) {
			t.Errorf("%s fixes = %v, want %v", id, kinds[id], w)
		}
	}
	// Only the generated drafts are looked up, once.
	if slices.Sort(versions.asked); !slices.Equal(versions.asked, []string{"edited", "fresh"}) {
		t.Fatalf("asked = %v", versions.asked)
	}
	for _, f := range out.Findings {
		for _, fix := range f.Fixes {
			switch fix.Kind {
			case calendarcheck.FixDelete:
				if fix.ProposalID == "" || fix.Card == nil || fix.Card.Verb != "Delete post" || !slices.Equal(fix.PostIDs, []string{"fresh"}) {
					t.Fatalf("delete = %+v", fix)
				}
			case calendarcheck.FixUnschedule:
				if fix.ProposalID == "" || !slices.Equal(fix.PostIDs, []string{f.PostID}) {
					t.Fatalf("unschedule = %+v", fix)
				}
			}
		}
	}
	tools := map[string]int{}
	for _, p := range sink.got {
		tools[p.Tool]++
	}
	if tools["unschedulePosts"] != 2 || tools["deletePosts"] != 1 {
		t.Fatalf("offered = %v", tools)
	}

	// With nothing to delete with, a draft isn't offered for deleting.
	d.Remove = nil
	if out, err = checkBrand(assistantkit.WithProposalSink(ctx, sink), d, CheckBrandInput{PostIDs: []string{"fresh"}}); err != nil || len(out.Findings) == 0 ||
		slices.ContainsFunc(out.Findings[0].Fixes, func(f FixOffer) bool { return f.Kind == calendarcheck.FixDelete }) {
		t.Fatalf("no remover: %+v, %v", out, err)
	}
}
