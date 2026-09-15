package server

import "expvar"

// Tier-version lifecycle tunables + counters for the guarded retire / delete /
// assignment-listing surface (CON-297). Counters follow the src/jobs/metrics.go
// idiom (package-level expvar.Int, exposed at /debug/vars) so operators can see
// how often versions are deleted, retired, and how many tenants get migrated.
const (
	// defaultAssignmentPageSize / maxAssignmentPageSize bound ListTierVersionAssignments.
	defaultAssignmentPageSize = 50
	maxAssignmentPageSize     = 200
	// maxHealthyActiveVersions is the concurrent-active-versions-per-tier ceiling
	// above which we log a warning — each grandfathered version is a maintenance
	// cost (CON-243 §10).
	maxHealthyActiveVersions = 3
)

var (
	metricTierVersionsDeleted      = expvar.NewInt("ogen_plans_tier_versions_deleted")
	metricTierVersionsRetired      = expvar.NewInt("ogen_plans_tier_versions_retired")
	metricTierVersionReassignments = expvar.NewInt("ogen_plans_tier_version_reassignments")
)
