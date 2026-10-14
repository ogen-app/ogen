package queues

import (
	"slices"
	"testing"
)

// TestMaintenanceTasksMatchTheContract keeps ogen's MaintenanceService task
// names in step with the sweeps jobrpc.v1 lists.
func TestMaintenanceTasksMatchTheContract(t *testing.T) {
	var got []string
	for k := range MaintenanceTasks(Deps{}) {
		got = append(got, k)
	}
	slices.Sort(got)
	want := []string{"cleanup_login_security", "cleanup_notifications", "cleanup_plugin_pairings", "cleanup_post_logs", "sweep_pending_uploads"}
	if !slices.Equal(got, want) {
		t.Errorf("maintenance tasks = %v, want %v", got, want)
	}
	// With no repositories wired, every sweep is a no-op rather than a crash.
	for name, run := range MaintenanceTasks(Deps{}) {
		if err := run(t.Context()); err != nil {
			t.Errorf("%s with no deps: %v", name, err)
		}
	}
}
