package models

import "time"

// Values of the posts list's scheduled filter: whether a post has a date.
const (
	PostScheduledYes = "yes"
	PostScheduledNo  = "no"
)

// PostScheduledValues lists the scheduled values.
var PostScheduledValues = []string{PostScheduledYes, PostScheduledNo}

// PostStatuses lists every post status, in lifecycle order.
var PostStatuses = []string{
	string(PostStatusPlanned), string(PostStatusDraft), string(PostStatusReadyForPublish),
	string(PostStatusScheduled), string(PostStatusScheduledForManualPublish), string(PostStatusFailed),
	string(PostStatusPublished), string(PostStatusNotPublished),
}

// PostQuery is a read of the posts list: its filters, oldest first.
type PostQuery struct {
	Status   AssetValueFilter
	Platform AssetValueFilter
	PostType AssetValueFilter
	Campaign AssetValueFilter
	// Scheduled is PostScheduledYes, PostScheduledNo, or empty for both.
	Scheduled string
	// ScheduledAfter is inclusive, ScheduledBefore exclusive; either bound
	// leaves out the posts without a date.
	ScheduledAfter  *time.Time
	ScheduledBefore *time.Time
	// Location is the time zone a YYYY-MM-DD bound is read in.
	Location *time.Location
	// ScopeCampaign confines the read to one campaign's posts: the campaign
	// list's own, or the assistant thread's.
	ScopeCampaign string
}
