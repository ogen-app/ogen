package queues_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/jobs"
	"github.com/ogen-app/ogen/src/jobs/queues"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
	"github.com/ogen-app/ogen/src/pgtest"
)

// sweepStorage records deletes and fails the keys in failing.
type sweepStorage struct {
	fakeStorage
	deleted []string
	failing map[string]bool
}

func (s *sweepStorage) Delete(_ context.Context, key string) error {
	if s.failing[key] {
		return errors.New("storage unavailable")
	}
	s.deleted = append(s.deleted, key)
	return nil
}

func TestSweepPendingUploads(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	expired := now.Add(-3 * time.Hour) // past the 2h grace
	recent := now.Add(-time.Hour)      // expired, still inside the grace

	setup := func(t *testing.T) (*bun.DB, *sweepStorage, *queues.SweepPendingUploadsProcessor) {
		t.Helper()
		db := pgtest.MustDB()
		db.SetMaxOpenConns(1)
		db.SetMaxIdleConns(1)
		if _, err := db.Exec("SET session_replication_role = replica"); err != nil {
			t.Fatalf("disable fks: %v", err)
		}
		t.Cleanup(func() { _ = db.Close() })

		ctx := tenantctx.With(t.Context(), models.DefaultTenantID)
		for _, u := range []*models.PendingUpload{
			{ID: "abandoned", S3Key: "k-abandoned", ExpiresAt: expired},
			{ID: "finalized-late", S3Key: "k-finalized-late", ExpiresAt: expired},
			{ID: "never-uploaded", S3Key: "k-never-uploaded", ExpiresAt: expired},
			{ID: "in-grace", S3Key: "k-in-grace", ExpiresAt: recent},
		} {
			if _, err := db.NewInsert().Model(u).Exec(ctx); err != nil {
				t.Fatalf("seed %s: %v", u.ID, err)
			}
		}
		// The late finalize's attachment holds its key while the record is
		// still there.
		att := &models.PostAttachment{ID: "att-1", PostID: "post-1", MimeType: "video/mp4", S3Key: "k-finalized-late", CreatedBy: "user-1"}
		if _, err := db.NewInsert().Model(att).Exec(ctx); err != nil {
			t.Fatalf("seed attachment: %v", err)
		}

		store := &sweepStorage{}
		p := &queues.SweepPendingUploadsProcessor{
			Repo:    repository.NewPendingUploadRepository(db),
			Storage: store,
			Config:  queues.PendingUploadSweepConfig{Live: true, Grace: 2 * time.Hour},
			Now:     func() time.Time { return now },
		}
		return db, store, p
	}
	remaining := func(t *testing.T, db *bun.DB) []string {
		t.Helper()
		var ids []string
		if err := db.NewRaw(`SELECT id FROM pending_uploads ORDER BY id`).Scan(t.Context(), &ids); err != nil {
			t.Fatalf("list records: %v", err)
		}
		return ids
	}
	run := func(t *testing.T, p *queues.SweepPendingUploadsProcessor) {
		t.Helper()
		if err := p.Process(tenantctx.WithSystem(t.Context()), queues.SweepPendingUploadsTask{}); err != nil {
			t.Fatalf("process: %v", err)
		}
	}

	t.Run("live sweep deletes abandoned objects and keeps attached ones", func(t *testing.T) {
		db, store, p := setup(t)
		swept := jobs.PendingUploadsSwept.Value()
		run(t, p)

		// A missing object deletes cleanly (storage deletes are idempotent),
		// so a never-uploaded record goes the same way as an abandoned one.
		slices.Sort(store.deleted)
		if want := []string{"k-abandoned", "k-never-uploaded"}; !slices.Equal(store.deleted, want) {
			t.Fatalf("deleted %v, want %v", store.deleted, want)
		}
		if got := remaining(t, db); !slices.Equal(got, []string{"in-grace"}) {
			t.Fatalf("records left %v, want only the one inside the grace period", got)
		}
		if got := jobs.PendingUploadsSwept.Value() - swept; got != 2 {
			t.Fatalf("swept metric moved by %d, want 2", got)
		}
	})

	t.Run("failed delete keeps the record for the next tick", func(t *testing.T) {
		db, store, p := setup(t)
		store.failing = map[string]bool{"k-abandoned": true}
		run(t, p)

		if got := remaining(t, db); !slices.Equal(got, []string{"abandoned", "in-grace"}) {
			t.Fatalf("records left %v, want the failed one kept", got)
		}
		store.failing = nil
		run(t, p)
		if got := remaining(t, db); !slices.Equal(got, []string{"in-grace"}) {
			t.Fatalf("records left after retry %v", got)
		}
	})

	t.Run("dry run counts but touches nothing", func(t *testing.T) {
		db, store, p := setup(t)
		p.Config.Live = false
		found := jobs.PendingUploadsFound.Value()
		run(t, p)

		if len(store.deleted) != 0 {
			t.Fatalf("dry run deleted %v", store.deleted)
		}
		if got := remaining(t, db); len(got) != 4 {
			t.Fatalf("dry run removed records: %v left", got)
		}
		if got := jobs.PendingUploadsFound.Value() - found; got != 3 {
			t.Fatalf("found metric moved by %d, want 3", got)
		}
	})

	t.Run("unwired sweep is a no-op", func(t *testing.T) {
		if err := (&queues.SweepPendingUploadsProcessor{}).Process(tenantctx.WithSystem(t.Context()), queues.SweepPendingUploadsTask{}); err != nil {
			t.Fatalf("process: %v", err)
		}
	})
}

func TestPeriodicJobsPendingUploadSweepNeedsInterval(t *testing.T) {
	base := queues.PeriodicConfig{CleanupEvery: time.Hour, ReconcileEvery: 5 * time.Minute}
	if got := len(base.PeriodicJobs()); got != 2 {
		t.Fatalf("without an interval: got %d jobs, want 2", got)
	}
	base.PendingUploadSweepEvery = time.Hour
	if got := len(base.PeriodicJobs()); got != 3 {
		t.Fatalf("with an interval: got %d jobs, want 3", got)
	}
}
