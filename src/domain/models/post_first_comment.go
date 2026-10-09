package models

import (
	"slices"
	"strings"
)

// FirstCommentStatus is the outcome of a post's first comment.
type FirstCommentStatus string

const (
	// FirstCommentPending: the post is live and Ogen posts the comment once
	// the delay elapses.
	FirstCommentPending FirstCommentStatus = "pending"
	// FirstCommentDelegated: the comment rode the publish request and Zernio
	// posted it with the post. Zernio reports no outcome for it.
	FirstCommentDelegated FirstCommentStatus = "delegated"
	FirstCommentPosted    FirstCommentStatus = "posted"
	FirstCommentFailed    FirstCommentStatus = "failed"
	// FirstCommentSkipped: the post never went live, so neither did the comment.
	FirstCommentSkipped FirstCommentStatus = "skipped"
)

// FirstCommentDelays are the minutes after publish a first comment may wait.
// 0 sends it with the post.
var FirstCommentDelays = []int{0, 1, 3, 5, 10}

// ValidFirstCommentDelay reports whether minutes is one of FirstCommentDelays.
func ValidFirstCommentDelay(minutes int) bool {
	return slices.Contains(FirstCommentDelays, minutes)
}

// HasFirstComment reports whether the post carries a first comment.
func (p *Post) HasFirstComment() bool {
	return strings.TrimSpace(p.FirstComment) != ""
}

// FirstCommentLimit is the comment ceiling for the post's platform and type;
// 0 means the post can't take a first comment.
func (p *Post) FirstCommentLimit() int {
	if p.Platform == nil {
		return 0
	}
	return p.Platform.TextConstraints.FirstCommentLimitFor(p.PlatformPostType)
}

// SendsFirstComment reports whether publishing this post sends a first
// comment: it has one and its platform and type take one.
func (p *Post) SendsFirstComment() bool {
	return p.HasFirstComment() && p.FirstCommentLimit() > 0
}
