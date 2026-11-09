package packs

import (
	"cmp"
	"context"
	"slices"
	"strings"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/genkit/flows/assistant"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/assistantkit"
	"github.com/ogen-app/ogen/src/infra/repository"
)

// What readAnalysis returns at most: the summary's length and the ideas it
// lists.
const (
	analysisSummaryRunes = 2000
	analysisTopIdeas     = 8
)

// lensNotRead is the status of a lens that has never read the asset.
const lensNotRead = "not_read"

// ReadAnalysisInput names the asset and the lens to read.
type ReadAnalysisInput struct {
	AssetID    string `json:"assetId,omitempty"    jsonschema:"description=The asset; defaults to the one open on the user's screen"`
	CampaignID string `json:"campaignId,omitempty" jsonschema:"description=Read this campaign's lens; defaults to the thread's campaign, else the workspace lens"`
	Workspace  bool   `json:"workspace,omitempty"  jsonschema:"description=Read the workspace lens even in a campaign thread"`
}

// LensRef names a lens: a campaign's, or the workspace's when CampaignID is
// empty.
type LensRef struct {
	CampaignID   string     `json:"campaignId,omitempty"`
	CampaignName string     `json:"campaignName,omitempty"`
	Status       string     `json:"status"`
	ReadAt       *time.Time `json:"readAt,omitempty"`
}

// IdeaCounts counts a lens's ideas by verdict; a later idea whose reminder
// has passed counts as undecided.
type IdeaCounts struct {
	Undecided int `json:"undecided"`
	Yes       int `json:"yes"`
	Later     int `json:"later"`
	No        int `json:"no"`
}

// AnalysisIdea is one idea a lens distilled; Verdict is empty while
// undecided.
type AnalysisIdea struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Verdict  string `json:"verdict,omitempty"`
	FitScore *int   `json:"fitScore,omitempty"`
}

// ReadAnalysisOutput is what one lens found in an asset, and the asset's
// other lenses, which are offered and never mixed in.
type ReadAnalysisOutput struct {
	AssetID       string         `json:"assetId"`
	Title         string         `json:"title"`
	Lens          LensRef        `json:"lens"`
	Gist          string         `json:"gist,omitempty"`
	Summary       string         `json:"summary,omitempty"`
	KeyPoints     []string       `json:"keyPoints,omitempty"`
	Ideas         IdeaCounts     `json:"ideas"`
	TopIdeas      []AnalysisIdea `json:"topIdeas"`
	FailureReason string         `json:"failureReason,omitempty"`
	OtherLenses   []LensRef      `json:"otherLenses"`
	Note          string         `json:"note,omitempty"`
}

func readAnalysis(ctx context.Context, d Deps, in ReadAnalysisInput) (*ReadAnalysisOutput, error) {
	assetID := cmp.Or(strings.TrimSpace(in.AssetID), assistant.FocusedAsset(ctx))
	if assetID == "" {
		return nil, assistantkit.Refused("which asset? pass assetId; get it from getSelection or findAssets")
	}
	metas, err := d.Assets.ListMeta(ctx, []string{assetID})
	if err != nil {
		return nil, err
	}
	if len(metas) == 0 {
		return nil, assistantkit.Refused("that asset doesn't exist")
	}
	var want *string
	if !in.Workspace {
		want = lensCampaign(ctx, in.CampaignID)
	}
	lenses, err := d.Analyses.ListByAsset(ctx, assetID)
	if err != nil {
		return nil, err
	}
	out := &ReadAnalysisOutput{AssetID: assetID, Title: metas[0].Title, TopIdeas: []AnalysisIdea{}, OtherLenses: []LensRef{}}
	var lens *models.AssetAnalysis
	for i := range lenses {
		if sameLens(lenses[i].CampaignID, want) {
			lens = &lenses[i]
			continue
		}
		out.OtherLenses = append(out.OtherLenses, lensRef(&lenses[i]))
	}
	if lens == nil {
		out.Lens = LensRef{Status: lensNotRead}
		if want != nil {
			out.Lens.CampaignID = *want
		}
		out.Note = "this lens hasn't read the asset. Say so; offer a lens in otherLenses or analyseAsset, and never answer from another lens unasked"
		return out, nil
	}
	out.Lens = lensRef(lens)
	switch lens.Status {
	case models.AssetAnalysisStatusReady:
	case models.AssetAnalysisStatusFailed, models.AssetAnalysisStatusSkipped:
		out.FailureReason = lens.FailureReason
		out.Note = "this lens has no reading: it " + string(lens.Status) + "; analyseAsset retries it"
		return out, nil
	default:
		out.Note = "this lens is still reading the asset; its summary and ideas appear on the asset when it's done"
		return out, nil
	}
	out.Gist, out.KeyPoints = lens.Gist, lens.Result.KeyPoints
	out.Summary = clip(lens.Summary, analysisSummaryRunes)
	ideas, err := d.IdeaRepo.List(ctx, repository.IdeaListFilter{AssetID: assetID})
	if err != nil {
		return nil, err
	}
	out.Ideas, out.TopIdeas = tallyIdeas(ideas, lens.ID, time.Now())
	return out, nil
}

// tallyIdeas counts the ideas lensID distilled by verdict and lists the
// first of them: undecided first, then the ones taken up, the best fit first
// in each.
func tallyIdeas(ideas []models.Idea, lensID string, now time.Time) (IdeaCounts, []AnalysisIdea) {
	var counts IdeaCounts
	top := []AnalysisIdea{}
	for _, idea := range ideas {
		if idea.AssetAnalysisID == nil || *idea.AssetAnalysisID != lensID {
			continue
		}
		verdict := ""
		switch {
		case idea.Verdict == nil, *idea.Verdict == models.IdeaVerdictLater && idea.RemindAt != nil && !idea.RemindAt.After(now):
			counts.Undecided++
		case *idea.Verdict == models.IdeaVerdictYes:
			counts.Yes++
			verdict = string(*idea.Verdict)
		case *idea.Verdict == models.IdeaVerdictLater:
			counts.Later++
			verdict = string(*idea.Verdict)
		default:
			counts.No++
			verdict = string(*idea.Verdict)
		}
		top = append(top, AnalysisIdea{ID: idea.ID, Title: idea.Title, Verdict: verdict, FitScore: idea.FitScore})
	}
	slices.SortStableFunc(top, func(a, b AnalysisIdea) int {
		return cmp.Or(cmp.Compare(ideaRank(a), ideaRank(b)), cmp.Compare(fit(b), fit(a)))
	})
	return counts, top[:min(len(top), analysisTopIdeas)]
}

func sameLens(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func lensRef(a *models.AssetAnalysis) LensRef {
	r := LensRef{Status: string(a.Status), ReadAt: a.AnalyzedAt}
	if a.CampaignID != nil {
		r.CampaignID = *a.CampaignID
	}
	if a.CampaignName != nil {
		r.CampaignName = *a.CampaignName
	}
	return r
}

func ideaRank(i AnalysisIdea) int {
	switch i.Verdict {
	case "":
		return 0
	case string(models.IdeaVerdictYes):
		return 1
	case string(models.IdeaVerdictLater):
		return 2
	}
	return 3
}

func fit(i AnalysisIdea) int {
	if i.FitScore == nil {
		return 0
	}
	return *i.FitScore
}

// clip cuts s to n runes, marking the cut.
func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
