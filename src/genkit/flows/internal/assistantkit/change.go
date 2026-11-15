package assistantkit

import "github.com/ogen-app/ogen/src/domain/models"

// Change is one thing that changed since an offer was made. Reason is it in
// words, as the model and the thread read it; the fields after it are the
// facts it is worded from, so the client can word it its own way.
type Change struct {
	Reason string `json:"reason"`
	// Code is what happened, one of the Change slugs.
	Code string `json:"code"`
	// Count of the offer's Of Items it happened to, e.g. post.
	Item  string `json:"item,omitempty"`
	Count int    `json:"count,omitzero"`
	Of    int    `json:"of,omitzero"`
	// Day is the day, YYYY-MM-DD, the posts were on when offered, for a
	// change to posts the offer moved; Of then counts that day's.
	Day string `json:"day,omitempty"`
	// Title is the one item's, when Count is one.
	Title string `json:"title,omitempty"`
	// By is the members who made the change, most first; empty when none
	// is known to have, e.g. a post the publisher published.
	By []models.ChangedBy `json:"by,omitempty"`
}

// The things that can change under an offer.
const (
	// ChangeDeleted: the items no longer exist.
	ChangeDeleted = "deleted"
	// ChangeLeftCampaign: the items are no longer in the campaign.
	ChangeLeftCampaign = "left_campaign"
	// ChangeScheduled: the posts were scheduled, and scheduled posts stay
	// put.
	ChangeScheduled = "scheduled"
	// ChangePublished: the posts were published.
	ChangePublished = "published"
	// ChangeMoved: the posts were moved to another date.
	ChangeMoved = "moved"
	// ChangeDayClosed: the campaign no longer publishes on the day the
	// posts were to move to.
	ChangeDayClosed = "day_closed"
	// ChangeSlotTaken: another post on the platform took the free day the
	// post was to move to.
	ChangeSlotTaken = "slot_taken"
	// ChangeOther: Reason alone says what changed.
	ChangeOther = "other"
)

// Reasons are changes in words.
func Reasons(changes []Change) []string {
	if len(changes) == 0 {
		return nil
	}
	out := make([]string, len(changes))
	for i, c := range changes {
		out[i] = c.Reason
	}
	return out
}
