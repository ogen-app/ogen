package packs

import (
	"context"
	"slices"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/genkit/flows/brand_check"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/assistantkit"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/usecase/assistantundo"
	"github.com/ogen-app/ogen/src/usecase/brandresolve"
	"github.com/ogen-app/ogen/src/usecase/campaign_actions/calendarcheck"
)

// FixUseVoice reads a post in another voice: its campaign's, in place of
// the post's own.
const FixUseVoice calendarcheck.FixKind = "use_voice"

// SetPostVoiceInput is a post of the campaign and the voice to read it in.
type SetPostVoiceInput struct {
	CampaignID string `json:"campaignId,omitempty" jsonschema:"description=Defaults to the thread's campaign"`
	PostID     string `json:"postId"`
	VoiceID    string `json:"voiceId,omitempty"    jsonschema:"description=A voice's id from getBrand; omit for the campaign's voice"`
}

// SetPostVoiceOutput is the post with its voice before and after. To is
// the voice the post reads in now, which is its campaign's when Own is
// false.
type SetPostVoiceOutput struct {
	PostID     string    `json:"postId"`
	CampaignID string    `json:"campaignId"`
	Title      string    `json:"title,omitempty"`
	From       *BrandRef `json:"from,omitempty"`
	To         *BrandRef `json:"to,omitempty"`
	Own        bool      `json:"own"`
	// UndoID gives the post its voice back.
	UndoID string `json:"undoId,omitempty"`
	Note   string `json:"note"`
}

// readsIn is the voice a post reads in, nil when the brand has none.
func readsIn(ctx context.Context, d Deps, c *models.Campaign, p models.Post) *BrandRef {
	r, err := brandresolve.Resolve(ctx, d.Brand, c, &p)
	if err != nil || r.Voice == nil {
		return nil
	}
	return &BrandRef{ID: r.Voice.ID, Name: r.Voice.Name}
}

// campaignVoice is the voice a post would read in without its own: its
// campaign's, else the workspace's default.
func campaignVoice(ctx context.Context, d Deps, c *models.Campaign, p models.Post) *BrandRef {
	p.BrandVoiceID = nil
	return readsIn(ctx, d, c, p)
}

// setPostVoice binds a post to a voice, or, with none named, unbinds it so
// it reads in its campaign's. The copy stays as it is.
func setPostVoice(ctx context.Context, d Deps, in SetPostVoiceInput) (*SetPostVoiceOutput, error) {
	c, posts, skipped, err := campaignPosts(ctx, d, in.CampaignID, []string{in.PostID})
	if err != nil {
		return nil, err
	}
	if len(skipped) > 0 {
		return nil, assistantkit.Refused("that post isn't in this campaign any more")
	}
	was := posts[0]
	if was.Status == models.PostStatusPublished {
		return nil, assistantkit.Refused("the post is published, so there's nothing left to write in another voice")
	}
	var to *string
	if in.VoiceID != "" {
		data, err := d.Brand.GetAll(ctx)
		if err != nil {
			return nil, err
		}
		if !slices.ContainsFunc(data.Voices, func(v models.BrandVoice) bool { return v.ID == in.VoiceID }) {
			return nil, assistantkit.Refused("voice " + in.VoiceID + " doesn't exist; getBrand lists the voices")
		}
		to = &in.VoiceID
	}
	if (was.BrandVoiceID == nil && to == nil) || (was.BrandVoiceID != nil && to != nil && *was.BrandVoiceID == *to) {
		return nil, assistantkit.Refused("the post already reads in that voice")
	}
	ok, err := d.Posts.SetVoiceIf(ctx, nil, repository.VoiceChange{PostID: was.ID, From: was.BrandVoiceID, To: to}, time.Now().UTC())
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, assistantkit.Refused("the post's voice changed since; check it again")
	}
	after := was
	after.BrandVoiceID = to
	out := &SetPostVoiceOutput{PostID: was.ID, CampaignID: c.ID, Title: was.Title, From: readsIn(ctx, d, c, was), To: readsIn(ctx, d, c, after),
		Own: to != nil, Note: "the copy stays as it is; checks and rewrites read it in this voice"}
	out.UndoID = recordUndo(ctx, d, models.AssistantActionPostVoice, was.ID, assistantundo.PostVoice(was.BrandVoiceID), assistantundo.PostVoice(to))
	if out.UndoID != "" {
		out.Note += "; it can be undone"
	}
	return out, nil
}

// voiceFix offers to read a post in its campaign's voice when a voice
// finding read it in a voice of its own that the campaign doesn't use.
func voiceFix(ctx context.Context, d Deps, c *models.Campaign, p *models.Post, f brand_check.Finding) (*FixOffer, error) {
	if f.Rule != brand_check.RuleVoice || p.BrandVoiceID == nil || p.Status == models.PostStatusPublished {
		return nil, nil
	}
	own, theirs := readsIn(ctx, d, c, *p), campaignVoice(ctx, d, c, *p)
	if own == nil || theirs == nil || own.ID == theirs.ID {
		return nil, nil
	}
	title := "Use " + theirs.Name + ", the campaign's voice"
	card := &assistantkit.Card{Verb: "Use " + theirs.Name,
		Rows:     []assistantkit.CardRow{{Kind: assistantkit.CardRowPost, Value: p.Title, Note: own.Name + " → " + theirs.Name}},
		Footnote: "The copy stays as it is. Checks and rewrites read it in " + theirs.Name + "."}
	id, err := assistantkit.Offer(ctx, assistantkit.Proposal{Tool: "setPostVoice",
		Args: SetPostVoiceInput{CampaignID: c.ID, PostID: p.ID}, Summary: title, Card: card})
	if err != nil {
		return nil, err
	}
	return &FixOffer{Fix: calendarcheck.Fix{Kind: FixUseVoice, Title: title, PostID: p.ID}, ProposalID: id, Card: card}, nil
}
