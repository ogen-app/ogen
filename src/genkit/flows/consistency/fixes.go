package consistency

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"github.com/ogen-app/ogen/src/domain/models"
)

// Kinds of a finding's fix.
const (
	// FixImproveBrief rewrites the brief by Instruction.
	FixImproveBrief = "improve_brief"
	// FixRevisePost rewrites the post PostID by Instruction.
	FixRevisePost = "revise_post"
	// FixOpenPost opens the post PostID to fix it by hand.
	FixOpenPost = "open_post"
)

// Fix is one way to repair a finding, which the assistant offers.
type Fix struct {
	Kind        string `json:"kind" enums:"improve_brief,revise_post,open_post"`
	Title       string `json:"title"`
	PostID      string `json:"postId,omitempty"`
	Instruction string `json:"instruction,omitempty"`
}

// briefFindings keys the brief's findings and gives each its fix. A key
// holds while the brief's text does: an edit to the brief raises a
// dismissed finding again.
func briefFindings(c *models.Campaign, findings []Finding) []Finding {
	brief := c.Description + "\x00" + c.TargetPersona + "\x00" + c.KeyMessages + "\x00" + c.ToneGuidelines
	seen := map[string]int{}
	for i := range findings {
		f := &findings[i]
		seen[f.Aspect]++
		f.Key = key("brief_"+f.Aspect, c.ID, fmt.Sprint(seen[f.Aspect]), brief)
		f.Fixes = []Fix{{Kind: FixImproveBrief, Title: "Improve the brief's " + aspectLabel(f.Aspect),
			Instruction: fmt.Sprintf("Fix this review finding about the %s: %s (%s)", aspectLabel(f.Aspect), f.Issue, f.Suggestion)}}
	}
	return findings
}

// postFinding keys a post's finding and gives it its fixes: rewriting the
// post while its content is Ogen's alone, and opening it. A key holds while
// the post's text does.
func postFinding(campaignID string, p models.Post, f PostFinding) PostFinding {
	f.Key = key("post_drift", campaignID, p.ID, p.Title+"\x00"+p.Content)
	f.Fixes = []Fix{}
	if !p.Status.IsSubmitted() {
		f.Fixes = append(f.Fixes, Fix{Kind: FixRevisePost, Title: "Rewrite it to follow the brief", PostID: p.ID,
			Instruction: fmt.Sprintf("Bring the post in line with the campaign brief. The issue: %s Fix it this way: %s", f.Issue, f.Suggestion)})
	}
	f.Fixes = append(f.Fixes, Fix{Kind: FixOpenPost, Title: "Open the post", PostID: p.ID})
	return f
}

func key(code string, parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p + "|"))
	}
	return code + ":" + hex.EncodeToString(h.Sum(nil))[:12]
}

func aspectLabel(aspect string) string {
	switch aspect {
	case "goal_alignment":
		return "goal"
	case "messages":
		return "key messages"
	case "":
		return "brief"
	}
	return aspect
}
