package campaign_assistant

import (
	"context"
	"maps"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/genkit/flows/assistant"
	"github.com/ogen-app/ogen/src/genkit/flows/consistency"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/assistantkit"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/usecase/campaign_actions/calendarcheck"
)

// FixOffer is a finding's fix with the offer that applies it.
type FixOffer struct {
	calendarcheck.Fix
	// ProposalID is the offer that applies the fix; empty for a post to
	// open, which navigate does.
	ProposalID string `json:"proposalId,omitempty"`
}

// RevisePostInput is a post of the campaign and how to rewrite it.
type RevisePostInput struct {
	CampaignID  string `json:"campaignId,omitempty" jsonschema:"description=Defaults to the thread's campaign"`
	PostID      string `json:"postId"`
	Instruction string `json:"instruction"`
}

// ReviewFinding is a brief or post finding with its fixes.
type ReviewFinding struct {
	// Kind is brief or post.
	Kind string `json:"kind"`
	// Key dismisses the finding while the brief, or its post, stays the
	// same.
	Key        string     `json:"key"`
	Severity   string     `json:"severity"`
	Aspect     string     `json:"aspect,omitempty"`
	PostID     string     `json:"postId,omitempty"`
	Title      string     `json:"title,omitempty"`
	Issue      string     `json:"issue"`
	Suggestion string     `json:"suggestion,omitempty"`
	Fixes      []FixOffer `json:"fixes"`
}

// The material a review reads, for its "read from" line.
const (
	ReadBrief = "brief"
	ReadPosts = "posts"
)

// ReviewOutput is a review of the campaign's brief, its posts against the
// brief, or both.
type ReviewOutput struct {
	CampaignID   string `json:"campaignId"`
	BriefSummary string `json:"briefSummary,omitempty"`
	PostsSummary string `json:"postsSummary,omitempty"`
	PostsChecked int    `json:"postsChecked"`
	// PostsTotal is how many posts could be checked; more than PostsChecked
	// when the check was capped.
	PostsTotal int             `json:"postsTotal,omitzero"`
	Findings   []ReviewFinding `json:"findings"`
	// Dismissed counts findings left out because the user dismissed them;
	// DismissedFindings are those findings, without their fixes, for the
	// card to show and restore.
	Dismissed         int             `json:"dismissed,omitempty"`
	DismissedFindings []ReviewFinding `json:"dismissedFindings"`
	// ReadFrom is what the review read: the brief, the posts, or both.
	ReadFrom []string `json:"readFrom"`
	// CheckedAt is when the review ran.
	CheckedAt time.Time `json:"checkedAt"`
	// Note says why nothing was reviewed, e.g. another review already ran
	// this turn.
	Note string `json:"note,omitempty"`
}

// ReviewOffers says which fixes a review can offer: improving the brief
// runs enrichBrief, rewriting a post runs revisePost.
type ReviewOffers struct {
	Brief  bool
	Revise bool
}

// NewReview starts a review of the campaign, as of now.
func NewReview(campaignID string) *ReviewOutput {
	return &ReviewOutput{CampaignID: campaignID, Findings: []ReviewFinding{}, DismissedFindings: []ReviewFinding{},
		ReadFrom: []string{}, CheckedAt: time.Now().UTC()}
}

// AddBrief adds the brief's review: its open findings, each offered its own
// brief edit, and the dismissed ones set aside.
func (o *ReviewOutput) AddBrief(ctx context.Context, r *consistency.BriefReview, dismissed map[string]bool, can ReviewOffers) error {
	o.BriefSummary = r.Summary
	o.ReadFrom = append(o.ReadFrom, ReadBrief)
	for _, f := range r.Findings {
		rf := ReviewFinding{Kind: "brief", Key: f.Key, Severity: f.Severity, Aspect: f.Aspect, Issue: f.Issue, Suggestion: f.Suggestion, Fixes: []FixOffer{}}
		if dismissed[f.Key] {
			o.dismiss(rf)
			continue
		}
		for _, fix := range f.Fixes {
			if fix.Kind != consistency.FixImproveBrief || !can.Brief {
				continue
			}
			id, err := assistantkit.Offer(ctx, assistantkit.Proposal{Tool: "enrichBrief",
				Args: EnrichBriefInput{CampaignID: o.CampaignID, Instruction: fix.Instruction}, Summary: fix.Title})
			if err != nil {
				return err
			}
			rf.Fixes = append(rf.Fixes, FixOffer{Fix: calendarcheck.Fix{Kind: calendarcheck.FixKind(fix.Kind), Title: fix.Title}, ProposalID: id})
		}
		o.Findings = append(o.Findings, rf)
	}
	return nil
}

// AddPosts adds the posts' review: each drifting post offered a rewrite,
// when the writer is here, and opening it; the dismissed ones set aside.
func (o *ReviewOutput) AddPosts(ctx context.Context, r *consistency.PostsReview, dismissed map[string]bool, can ReviewOffers) error {
	o.PostsSummary, o.PostsChecked, o.PostsTotal = r.Summary, r.Checked, r.Total
	o.ReadFrom = append(o.ReadFrom, ReadPosts)
	for _, f := range r.Findings {
		rf := ReviewFinding{Kind: "post", Key: f.Key, Severity: f.Severity, PostID: f.PostID, Title: f.Title,
			Issue: f.Issue, Suggestion: f.Suggestion, Fixes: []FixOffer{}}
		if dismissed[f.Key] {
			o.dismiss(rf)
			continue
		}
		for _, fix := range f.Fixes {
			offer := FixOffer{Fix: calendarcheck.Fix{Kind: calendarcheck.FixKind(fix.Kind), Title: fix.Title, PostID: fix.PostID}}
			if fix.Kind == consistency.FixRevisePost {
				if !can.Revise {
					continue
				}
				var err error
				if offer.ProposalID, err = assistantkit.Offer(ctx, assistantkit.Proposal{Tool: "revisePost",
					Args: RevisePostInput{CampaignID: o.CampaignID, PostID: fix.PostID, Instruction: fix.Instruction}, Summary: fix.Title}); err != nil {
					return err
				}
			}
			rf.Fixes = append(rf.Fixes, offer)
		}
		o.Findings = append(o.Findings, rf)
	}
	return nil
}

func (o *ReviewOutput) dismiss(f ReviewFinding) {
	o.Dismissed++
	o.DismissedFindings = append(o.DismissedFindings, f)
}

// DismissedKeys are the campaign's dismissed findings: the ones its
// Overview and any thread dismissed, and those dismissed in this thread
// before dismissals followed the campaign. A nil repository has none.
func DismissedKeys(ctx context.Context, repo repository.AssistantDismissalRepository, campaignID string) (map[string]bool, error) {
	if repo == nil {
		return map[string]bool{}, nil
	}
	keys, err := repo.Keys(ctx, models.AssistantScopeCampaign, campaignID)
	if err != nil {
		return nil, err
	}
	scope, _ := assistant.Scope(ctx)
	if scope.Kind == models.AssistantScopeCampaign && scope.ID == campaignID {
		return keys, nil
	}
	thread, err := repo.Keys(ctx, scope.Kind, scope.ID)
	if err != nil {
		return nil, err
	}
	keys = maps.Clone(keys)
	maps.Copy(keys, thread)
	return keys, nil
}
