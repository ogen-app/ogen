// Package activity is the centralised user-activity collection layer for
// An async Recorder that captures meaningful user/tenant actions and
// writes them to tenant_activity_events in the isolated analytics DB, without adding
// latency to — or ever failing — the calling flow. It mirrors the CON-86 usage
// Recorder but is behaviour-oriented (category/type/entity), not cost-oriented.
// It depends only on a narrow Writer interface that repository.ActivityRepository
// satisfies, plus tenant/user context resolution.
package activity

import "expvar"

// Metrics holds the CON-125 activity counters, registered under
// ogen_activity_* on the global expvar registry for /debug/vars. NewMetrics
// registers them; call it once at boot (expvar.NewInt panics on a duplicate
// name). Tests construct a Metrics with unregistered counters instead.
type Metrics struct {
	EventsRecorded *expvar.Int // rows successfully written
	EventsDropped  *expvar.Int // dropped on a full buffer (or id-gen failure)
	WriteErrors    *expvar.Int // batch Insert failures
}

// NewMetrics registers and returns the activity counters. Call once at boot.
func NewMetrics() *Metrics {
	return &Metrics{
		EventsRecorded: expvar.NewInt("ogen_activity_events_recorded"),
		EventsDropped:  expvar.NewInt("ogen_activity_events_dropped"),
		WriteErrors:    expvar.NewInt("ogen_activity_write_errors"),
	}
}
