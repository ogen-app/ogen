package queues

import (
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/pgtest"
)

func TestCleanupPluginPairings(t *testing.T) {
	db := pgtest.MustDB()
	t.Cleanup(func() { _ = db.Close() })
	ctx := t.Context()
	now := time.Now().UTC()

	for _, p := range []models.PluginPairing{
		{ID: "p-live", ExpiresAt: now.Add(time.Minute), ReadKeyHash: "r1", WriteKeyHash: "w1"},
		{ID: "p-gone", ExpiresAt: now.Add(-time.Minute), ReadKeyHash: "r2", WriteKeyHash: "w2"},
	} {
		p.Client, p.ClientLabel, p.Status = models.PluginClientFigma, "Figma", models.PluginPairingPending
		if _, err := db.NewInsert().Model(&p).Exec(ctx); err != nil {
			t.Fatal(err)
		}
	}

	p := &CleanupPluginPairingsProcessor{Repo: repository.NewPluginPairingRepository(db)}
	if err := p.Process(ctx, CleanupPluginPairingsTask{}); err != nil {
		t.Fatalf("process: %v", err)
	}
	var ids []string
	if err := db.NewSelect().Model((*models.PluginPairing)(nil)).Column("id").Scan(ctx, &ids); err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != "p-live" {
		t.Fatalf("after sweep: %v, want only p-live", ids)
	}

	if err := (&CleanupPluginPairingsProcessor{}).Process(ctx, CleanupPluginPairingsTask{}); err != nil {
		t.Fatalf("unwired sweep must be a no-op: %v", err)
	}
}

func TestPeriodicJobsPluginPairingCleanupNeedsInterval(t *testing.T) {
	base := PeriodicConfig{CleanupEvery: time.Hour, ReconcileEvery: 5 * time.Minute}
	if got := len(base.PeriodicJobs()); got != 2 {
		t.Fatalf("zero interval must not register the sweep: got %d jobs", got)
	}
	base.PluginPairingCleanupEvery = 15 * time.Minute
	if got := len(base.PeriodicJobs()); got != 3 {
		t.Fatalf("positive interval must register the sweep: got %d jobs", got)
	}
}
