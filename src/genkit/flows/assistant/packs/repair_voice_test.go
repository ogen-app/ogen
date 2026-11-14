package packs

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/assistantkit"
	"github.com/ogen-app/ogen/src/infra/repository"
)

func (r *memPosts) SetVoiceIf(_ context.Context, _ bun.IDB, c repository.VoiceChange, _ time.Time) (bool, error) {
	same := func(a, b *string) bool { return (a == nil && b == nil) || (a != nil && b != nil && *a == *b) }
	for i := range r.posts {
		if p := &r.posts[i]; p.ID == c.PostID {
			if r.raced[p.ID] || !same(p.BrandVoiceID, c.From) {
				return false, nil
			}
			p.BrandVoiceID = c.To
			return true, nil
		}
	}
	return false, nil
}

// A post is read in its campaign's voice, or one of its own, and can be
// given its voice back; its copy stays.
func TestSetPostVoice(t *testing.T) {
	brand := &memBrand{facts: map[string]*models.BrandFact{}, data: &models.BrandData{Voices: []models.BrandVoice{
		{ID: "v1", Name: "Plain", IsDefault: true}, {ID: "v2", Name: "Formal"}}}}
	posts := &memPosts{posts: []models.Post{
		{ID: "p1", CampaignID: "c1", Title: "Rooms", Content: "Dear guest", Status: models.PostStatusDraft, BrandVoiceID: new("v2")},
		{ID: "out", CampaignID: "c1", Content: "Gone", Status: models.PostStatusPublished, BrandVoiceID: new("v2")},
	}}
	actions := &memActions{}
	d := Deps{Brand: brand, Posts: posts, Campaigns: &memCampaigns{c: &models.Campaign{ID: "c1"}}, Actions: actions}
	ctx, touched := turn(t, campaign1, nil)

	out, err := setPostVoice(ctx, d, SetPostVoiceInput{PostID: "p1"})
	if err != nil {
		t.Fatal(err)
	}
	// An open post picks up its new voice.
	if !slices.Equal(touched(), []assistantkit.Touched{{Kind: "post", ID: "p1"}}) {
		t.Fatalf("touched = %v", touched())
	}
	if out.Own || out.From == nil || out.From.Name != "Formal" || out.To == nil || out.To.Name != "Plain" || out.Title != "Rooms" ||
		out.UndoID == "" || !strings.Contains(out.Note, "undone") {
		t.Fatalf("out = %+v", out)
	}
	if p := posts.posts[0]; p.BrandVoiceID != nil || p.Content != "Dear guest" {
		t.Fatalf("post = %v %q", p.BrandVoiceID, p.Content)
	}
	if a := actions.rows[0]; a.Kind != models.AssistantActionPostVoice || a.EntityID != "p1" || a.Before["voiceId"] != "v2" || a.After["voiceId"] != nil {
		t.Fatalf("undo log = %+v", a)
	}

	// It reads in the campaign's already.
	_, err = setPostVoice(ctx, d, SetPostVoiceInput{PostID: "p1"})
	refused(t, err)
	_, err = setPostVoice(ctx, d, SetPostVoiceInput{PostID: "p1", VoiceID: "v9"})
	refused(t, err)
	if out, err = setPostVoice(ctx, d, SetPostVoiceInput{PostID: "p1", VoiceID: "v2"}); err != nil || !out.Own || out.To.Name != "Formal" {
		t.Fatalf("own voice = %+v, %v", out, err)
	}
	_, err = setPostVoice(ctx, d, SetPostVoiceInput{PostID: "out"})
	refused(t, err)
	_, err = setPostVoice(ctx, d, SetPostVoiceInput{PostID: "gone"})
	refused(t, err)
	// Changed between the read and the write.
	posts.raced = map[string]bool{"p1": true}
	_, err = setPostVoice(ctx, d, SetPostVoiceInput{PostID: "p1"})
	refused(t, err)
}
