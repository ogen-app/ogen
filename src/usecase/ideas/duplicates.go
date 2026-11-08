package ideas

import (
	"cmp"
	"context"
	"errors"
	"math"
	"slices"
	"strings"
	"unicode"

	"github.com/ogen-app/ogen/src/domain/models"
)

// DuplicateThreshold is the similarity at which two ideas count as near
// duplicates. A pair matches when the Jaccard index of their title tokens, or
// of their title-and-note tokens, reaches it.
const DuplicateThreshold = 0.6

// MaxDuplicateScan caps the open ideas one search compares, newest first.
const MaxDuplicateScan = 500

// DuplicateIdea is one member of a duplicate group.
type DuplicateIdea struct {
	Idea models.Idea
	// AssetCount is how many assets the idea drew on.
	AssetCount int
}

// DuplicateGroup is a set of open ideas that read as the same idea. Ideas
// lists KeepID's idea first, then the rest oldest first.
type DuplicateGroup struct {
	// KeepID is the suggested idea to merge the others into: a yes first,
	// then the one with most assets, then the oldest.
	KeepID string
	// Similarity is the weakest pair score that joined the group into one,
	// 0–1.
	Similarity float64
	Ideas      []DuplicateIdea
}

// DuplicateResult is a duplicate search: the groups, largest first, how many
// open ideas were compared, and whether the scope held more than that.
type DuplicateResult struct {
	Groups    []DuplicateGroup
	Scanned   int
	Truncated bool
}

// Duplicates finds groups of near-duplicate open ideas — not turned down, no
// post made from them — in one scope: a campaign's ideas, or with campaignID
// nil the workspace-wide ones. Merging needs ideas of one scope, so a search
// never mixes them. It returns ErrScopeNotFound for a campaign that is gone.
func (s *Service) Duplicates(ctx context.Context, campaignID *string) (*DuplicateResult, error) {
	scope := ""
	if campaignID != nil {
		if err := s.checkCampaign(ctx, campaignID); errors.Is(err, ErrCampaignNotFound) {
			return nil, ErrScopeNotFound
		} else if err != nil {
			return nil, err
		}
		scope = *campaignID
	}
	open, more, err := s.repo.OpenIdeas(ctx, scope, MaxDuplicateScan)
	if err != nil {
		return nil, err
	}
	return &DuplicateResult{Groups: groupDuplicates(open), Scanned: len(open), Truncated: more}, nil
}

// ideaTokens is an idea's normalised words: its title alone, and its title
// with its note.
type ideaTokens struct {
	title, all []string
}

// groupDuplicates joins every matching pair into groups (union-find) and
// orders them largest first, then most similar, then oldest.
func groupDuplicates(list []models.Idea) []DuplicateGroup {
	tokens := make([]ideaTokens, len(list))
	for i, idea := range list {
		title := tokenize(idea.Title)
		tokens[i] = ideaTokens{title: title, all: union(title, tokenize(idea.Note))}
	}
	parent := make([]int, len(list))
	for i := range parent {
		parent[i] = i
	}
	var find func(int) int
	find = func(i int) int {
		if parent[i] != i {
			parent[i] = find(parent[i])
		}
		return parent[i]
	}
	weakest := map[int]float64{}
	for i := range list {
		for j := i + 1; j < len(list); j++ {
			score := similarity(tokens[i], tokens[j])
			if score < DuplicateThreshold {
				continue
			}
			ri, rj := find(i), find(j)
			if ri == rj {
				continue
			}
			weakest[ri] = math.Min(score, minScore(weakest, ri, rj))
			parent[rj] = ri
			delete(weakest, rj)
		}
	}
	members := map[int][]models.Idea{}
	for i, idea := range list {
		r := find(i)
		members[r] = append(members[r], idea)
	}
	var groups []DuplicateGroup
	for r, ideas := range members {
		if len(ideas) > 1 {
			groups = append(groups, newGroup(ideas, weakest[r]))
		}
	}
	slices.SortFunc(groups, func(a, b DuplicateGroup) int {
		return cmp.Or(
			cmp.Compare(len(b.Ideas), len(a.Ideas)),
			cmp.Compare(b.Similarity, a.Similarity),
			oldestFirst(a.Ideas[0].Idea, b.Ideas[0].Idea),
		)
	})
	return groups
}

// minScore is the weakest link so far of the groups rooted at a and b; 1
// for a group that has none yet.
func minScore(weakest map[int]float64, a, b int) float64 {
	w := 1.0
	for _, r := range []int{a, b} {
		if v, ok := weakest[r]; ok {
			w = math.Min(w, v)
		}
	}
	return w
}

func newGroup(ideas []models.Idea, score float64) DuplicateGroup {
	slices.SortFunc(ideas, keepFirst)
	g := DuplicateGroup{KeepID: ideas[0].ID, Similarity: math.Round(score*100) / 100, Ideas: make([]DuplicateIdea, len(ideas))}
	g.Ideas[0] = DuplicateIdea{Idea: ideas[0], AssetCount: len(ideas[0].AssetIDs)}
	rest := ideas[1:]
	slices.SortFunc(rest, oldestFirst)
	for i, idea := range rest {
		g.Ideas[i+1] = DuplicateIdea{Idea: idea, AssetCount: len(idea.AssetIDs)}
	}
	return g
}

// keepFirst orders the idea a group should keep first: a yes, then most
// assets, then the oldest.
func keepFirst(a, b models.Idea) int {
	yes := func(i models.Idea) int {
		if i.Verdict != nil && *i.Verdict == models.IdeaVerdictYes {
			return 0
		}
		return 1
	}
	return cmp.Or(
		cmp.Compare(yes(a), yes(b)),
		cmp.Compare(len(b.AssetIDs), len(a.AssetIDs)),
		oldestFirst(a, b),
	)
}

func oldestFirst(a, b models.Idea) int {
	return cmp.Or(a.CreatedAt.Compare(b.CreatedAt), cmp.Compare(a.ID, b.ID))
}

// similarity is the better of the two ideas' title and title-and-note
// Jaccard indexes: a duplicate often shares its headline but words its note
// afresh. A title of stopwords alone leaves the note to decide.
func similarity(a, b ideaTokens) float64 {
	return math.Max(jaccard(a.title, b.title), jaccard(a.all, b.all))
}

// jaccard is |a∩b| / |a∪b| of two sorted, unique token lists.
func jaccard(a, b []string) float64 {
	if len(a) == 0 && len(b) == 0 {
		return 0
	}
	shared, i, j := 0, 0, 0
	for i < len(a) && j < len(b) {
		switch strings.Compare(a[i], b[j]) {
		case 0:
			shared++
			i++
			j++
		case -1:
			i++
		default:
			j++
		}
	}
	return float64(shared) / float64(len(a)+len(b)-shared)
}

// stopwords are English words too common to tell ideas apart.
var stopwords = map[string]bool{
	"a": true, "an": true, "and": true, "are": true, "as": true, "at": true, "be": true, "but": true,
	"by": true, "can": true, "do": true, "for": true, "from": true, "has": true, "have": true, "how": true,
	"i": true, "in": true, "into": true, "is": true, "it": true, "its": true, "of": true, "on": true,
	"or": true, "our": true, "so": true, "that": true, "the": true, "their": true, "this": true, "to": true,
	"us": true, "was": true, "we": true, "what": true, "when": true, "why": true, "will": true, "with": true,
	"you": true, "your": true,
	// What an apostrophe leaves behind: founder's, don't, we'll.
	"s": true, "t": true, "d": true, "ll": true, "re": true, "ve": true, "m": true,
}

// tokenize lowercases text, splits it on anything but letters and digits,
// drops stopwords and trims plurals, and returns the sorted unique words.
func tokenize(text string) []string {
	var out []string
	words := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	for _, w := range words {
		if stopwords[w] {
			continue
		}
		out = append(out, singular(w))
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// singular trims a simple English plural: stories → story, myths → myth.
// Short words and -ss/-us/-is endings are left alone.
func singular(w string) string {
	if len(w) <= 3 {
		return w
	}
	if stem, ok := strings.CutSuffix(w, "ies"); ok && len(stem) > 1 {
		return stem + "y"
	}
	for _, keep := range []string{"ss", "us", "is"} {
		if strings.HasSuffix(w, keep) {
			return w
		}
	}
	return strings.TrimSuffix(w, "s")
}

// union merges two sorted, unique token lists.
func union(a, b []string) []string {
	out := slices.Concat(a, b)
	slices.Sort(out)
	return slices.Compact(out)
}
