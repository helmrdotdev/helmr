package dispatch

import (
	"context"
	"fmt"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func (d *Authority) grantFreshRun(ctx context.Context, tx pgx.Tx, r db.Run, i db.ComputerInstance) (db.RunLease, error) {
	if err := d.checkRunLeaseConcurrency(ctx, tx, r); err != nil {
		return db.RunLease{}, err
	}
	var sequence int64
	if err := tx.QueryRow(ctx, `SELECT COALESCE(max(lease_sequence),0)+1 FROM run_leases WHERE run_id=$1`, r.ID).Scan(&sequence); err != nil {
		return db.RunLease{}, err
	}
	q := db.New(tx)
	now, err := q.GetRunLeaseRenewalTime(ctx)
	if err != nil {
		return db.RunLease{}, err
	}
	lease, err := q.InsertAssignedRunLease(ctx, db.InsertAssignedRunLeaseParams{
		ID: pgvalue.UUID(uuid.NewV7()), EnvironmentID: r.EnvironmentID, RunID: r.ID, ComputerID: r.ComputerID,
		LeaseSequence: sequence, AttemptNumber: r.CurrentAttemptNumber, WorkerGroupID: i.WorkerGroupID, WorkerHostID: i.WorkerHostID, WorkerEpoch: i.WorkerEpoch,
		ComputerInstanceID: i.ID, WriterGeneration: i.WriterGeneration,
		RequestedCPUMillis: i.ReservedCPUMillis, RequestedMemoryBytes: i.ReservedMemoryBytes, RequestedGuestEphemeralDiskBytes: i.ReservedGuestEphemeralDiskBytes, RequestedExecutionSlots: 1,
		TraceID: r.TraceID, SpanID: pgvalue.Text(r.RootSpanID), StartDeadlineAt: pgvalue.Timestamptz(now.Time.Add(run.StartDeadline)), ExpiresAt: pgvalue.Timestamptz(now.Time.Add(run.LeaseTTL)),
	})
	if err != nil {
		return db.RunLease{}, err
	}
	_, err = q.SetRunCurrentLease(ctx, db.SetRunCurrentLeaseParams{RunID: r.ID, EnvironmentID: r.EnvironmentID, RunLeaseID: lease.ID, ExpectedRevision: r.Revision, AttemptNumber: r.CurrentAttemptNumber})
	return lease, err
}

func (d *Authority) checkRunLeaseConcurrency(
	ctx context.Context,
	tx pgx.Tx,
	authority db.Run,
) error {
	var active int64
	var activeLimit pgtype.Int8
	err := tx.QueryRow(ctx, `
SELECT count(*),
       min(active_runs.queue_concurrency_limit)
  FROM run_leases
  JOIN runs AS active_runs
    ON active_runs.id = run_leases.run_id
   AND active_runs.environment_id = run_leases.environment_id
 WHERE active_runs.environment_id = $1
   AND active_runs.queue_name = $2
   AND active_runs.concurrency_key IS NOT DISTINCT FROM $3::text
   AND run_leases.status IN ('assigned', 'starting', 'running', 'checkpointing', 'finalizing')`,
		authority.EnvironmentID,
		authority.QueueName,
		authority.ConcurrencyKey,
	).Scan(&active, &activeLimit)
	if err != nil {
		return fmt.Errorf("read run lease concurrency: %w", err)
	}
	limit := authority.QueueConcurrencyLimit
	if activeLimit.Valid && (!limit.Valid || activeLimit.Int64 < limit.Int64) {
		limit = activeLimit
	}
	if limit.Valid && active >= limit.Int64 {
		return ErrCapacityUnavailable
	}
	return nil
}
