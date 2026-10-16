package queues

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/ogen-app/ogen/src/infra/database"
	"github.com/ogen-app/ogen/src/jobs/outbox"
	"github.com/ogen-app/ogen/src/kernel/logging"
)

// riverLiveStates are the river_job states of a job that has yet to finish.
var riverLiveStates = []string{"available", "pending", "retryable", "running", "scheduled"}

// riverKinds decodes a River job's args, by kind, for every kind ogen
// enqueued. Periodic sweeps are absent: river-service schedules its own.
var riverKinds = map[string]func([]byte) (JobArgs, error){
	SubmitPostToZernioQueue:     decodeRiverArgs[SubmitPostTask],
	PollZernioStatusQueue:       decodeRiverArgs[PollZernioStatusTask],
	CancelZernioJobQueue:        decodeRiverArgs[CancelZernioJobTask],
	WithdrawZernioPostQueue:     decodeRiverArgs[WithdrawZernioPostTask],
	PostFirstCommentQueue:       decodeRiverArgs[PostFirstCommentTask],
	BootstrapZernioProfileQueue: decodeRiverArgs[BootstrapZernioProfileTask],
	TeardownZernioProfileQueue:  decodeRiverArgs[TeardownZernioProfileTask],
	SendEmailQueue:              decodeRiverArgs[SendEmailTask],
	NotifyHarborQueue:           decodeRiverArgs[NotifyHarborTenantRegisteredTask],
	ProcessPDFQueue:             decodeRiverArgs[ProcessPDFTask],
	ProcessDocumentQueue:        decodeRiverArgs[ProcessDocumentTask],
	ProcessURLQueue:             decodeRiverArgs[ProcessURLTask],
	ProcessImageQueue:           decodeRiverArgs[ProcessImageTask],
	ProcessAudioKind:            decodeRiverArgs[ProcessAudioTask],
	ReembedImageQueue:           decodeRiverArgs[ReembedImageTask],
	AnalyzeAssetKind:            decodeRiverArgs[AnalyzeAssetTask],
	ReanalyzeCampaignKind:       decodeRiverArgs[ReanalyzeCampaignTask],
}

func decodeRiverArgs[T JobArgs](b []byte) (JobArgs, error) {
	var args T
	err := json.Unmarshal(b, &args)
	return args, err
}

// RetireRiver hands the jobs ogen's in-process River left unfinished to
// river-service and drops River's schema. In one transaction, holding the
// migration lock and an exclusive lock on river_job, it writes each live job
// to the job outbox, scheduled as it was, and drops River's tables, functions
// and type. A job that was running when the lock was taken runs again in
// river-service; job steps converge on repeats. It returns how many jobs it
// moved, and does nothing on a database without River.
func RetireRiver(ctx context.Context, db *sql.DB) (int, error) {
	moved := 0
	err := database.WithMigrationLock(ctx, db, func(ctx context.Context) error {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		var hasRiver bool
		if err := tx.QueryRowContext(ctx, `SELECT to_regclass('river_job') IS NOT NULL`).Scan(&hasRiver); err != nil {
			return fmt.Errorf("look up river_job: %w", err)
		}
		if !hasRiver {
			return nil
		}
		// A River client still running elsewhere holds row locks only briefly;
		// a lock that can't be had fails the boot, which retries.
		if _, err := tx.ExecContext(ctx, `SET LOCAL lock_timeout = '30s'`); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `LOCK TABLE river_job IN ACCESS EXCLUSIVE MODE`); err != nil {
			return fmt.Errorf("lock river_job: %w", err)
		}
		if moved, err = moveRiverJobs(ctx, tx); err != nil {
			return err
		}
		for _, q := range []string{
			`DROP TABLE IF EXISTS river_client_queue`,
			`DROP TABLE IF EXISTS river_client`,
			`DROP TABLE IF EXISTS river_queue`,
			`DROP TABLE IF EXISTS river_leader`,
			`DROP TABLE IF EXISTS river_job`,
			`DROP TABLE IF EXISTS river_migration`,
			`DROP FUNCTION IF EXISTS river_job_notify() CASCADE`,
			`DROP FUNCTION IF EXISTS river_job_state_in_bitmask CASCADE`,
			`DROP TYPE IF EXISTS river_job_state CASCADE`,
		} {
			if _, err := tx.ExecContext(ctx, q); err != nil {
				return fmt.Errorf("drop River: %w", err)
			}
		}
		return tx.Commit()
	})
	if err != nil {
		return 0, fmt.Errorf("retire River: %w", err)
	}
	return moved, nil
}

type riverJob struct {
	id          int64
	kind        string
	args        []byte
	scheduledAt time.Time
}

func moveRiverJobs(ctx context.Context, tx *sql.Tx) (int, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT id, kind, args, scheduled_at FROM river_job
		WHERE state::text = ANY($1) ORDER BY id`, riverLiveStates)
	if err != nil {
		return 0, fmt.Errorf("read River jobs: %w", err)
	}
	var jobs []riverJob
	for rows.Next() {
		var j riverJob
		if err := rows.Scan(&j.id, &j.kind, &j.args, &j.scheduledAt); err != nil {
			_ = rows.Close()
			return 0, err
		}
		jobs = append(jobs, j)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return 0, fmt.Errorf("read River jobs: %w", err)
	}
	w := outbox.NewWriter(nil)
	moved := 0
	for _, j := range jobs {
		decode, ok := riverKinds[j.kind]
		if !ok {
			slog.InfoContext(ctx, "leaving a River job river-service schedules itself",
				logging.AttrComponent, "jobs", "kind", j.kind, "river_job_id", j.id)
			continue
		}
		args, err := decode(j.args)
		if err != nil {
			return 0, fmt.Errorf("decode River job %d (%s): %w", j.id, j.kind, err)
		}
		req, err := enqueueRequest(args, &EnqueueOpts{ScheduledAt: j.scheduledAt})
		if err != nil {
			return 0, err
		}
		if err := w.WriteTx(ctx, tx, j.kind, req); err != nil {
			return 0, fmt.Errorf("move River job %d: %w", j.id, err)
		}
		moved++
	}
	return moved, nil
}
