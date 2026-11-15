package zernio

import "github.com/ogen-app/ogen/src/infra/repository"

// WaitingByAccount attributes the scheduled posts to the connected accounts,
// as publishing picks them: a post naming an account waits on it, and one
// naming none waits on its platform's account when that is the platform's
// only one; with two or more, it waits on none of them. platformOf maps every
// connected account's id to its platform's Zernio slug. The result has an
// entry, maybe zero, for each account.
func WaitingByAccount(counts []repository.ScheduledCount, platformOf map[string]string) map[string]int {
	perPlatform := map[string]int{}
	for _, p := range platformOf {
		perPlatform[p]++
	}
	out := make(map[string]int, len(platformOf))
	for id, p := range platformOf {
		out[id] = 0
		for _, c := range counts {
			if c.AccountID == id || (c.AccountID == "" && perPlatform[p] == 1 && slugOf(c.PlatformID) == p) {
				out[id] += c.Count
			}
		}
	}
	return out
}

// slugOf is the Zernio slug of an Ogen platform sqid; "" when it isn't one
// Zernio publishes to.
func slugOf(sqid string) string {
	if p := LookupSupportedBySqid(sqid); p != nil {
		return p.ZernioID
	}
	return ""
}
