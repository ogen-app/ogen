package billing

import (
	"cmp"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
)

// Live reports whether a subscription still entitles the workspace: every
// status except expired, and cancelled only until its end date.
func Live(status string, endsAt *time.Time, now time.Time) bool {
	switch status {
	case models.SubscriptionStatusOnTrial, models.SubscriptionStatusActive,
		models.SubscriptionStatusPastDue, models.SubscriptionStatusUnpaid,
		models.SubscriptionStatusPaused:
		return true
	case models.SubscriptionStatusCancelled:
		return endsAt != nil && endsAt.After(now)
	default:
		return false
	}
}

// Changeable reports whether the owner may change plan in-app. Any other
// status is resolved in the provider's portal first.
func Changeable(status string) bool {
	return status == models.SubscriptionStatusOnTrial || status == models.SubscriptionStatusActive
}

// Price is a tier version price row with its tier, as ranking needs it.
type Price struct {
	ID            string
	TierID        string
	TierName      string
	TierVersionID string
	Currency      string
	Interval      string // month | year
	NetMinor      int64
}

// Direction of a plan change.
const (
	DirectionUpgrade   = "upgrade"
	DirectionDowngrade = "downgrade"
	DirectionSame      = "same"
)

// Direction ranks a move between two prices by their yearly net amount (a
// monthly price counts twelve times), so a monthly-to-yearly switch on one
// tier is an upgrade. Equal amounts fall back to the tier name order, then to
// the interval (yearly above monthly), so two different prices never rank the
// same.
func Direction(from, to Price) string {
	if from.ID == to.ID {
		return DirectionSame
	}
	c := cmp.Or(
		cmp.Compare(yearly(to), yearly(from)),
		cmp.Compare(to.TierName, from.TierName),
		cmp.Compare(intervalRank(to), intervalRank(from)),
	)
	switch {
	case c > 0:
		return DirectionUpgrade
	case c < 0:
		return DirectionDowngrade
	default:
		return DirectionSame
	}
}

func intervalRank(p Price) int {
	if p.Interval == "year" {
		return 1
	}
	return 0
}

func yearly(p Price) int64 {
	if p.Interval == "year" {
		return p.NetMinor
	}
	return p.NetMinor * 12
}
