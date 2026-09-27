package dispatch

import (
	"context"
	"errors"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

const idleComputerCaptureDelay = 30 * time.Second

// BeginIdleComputerCapture uses the same whole-Instance barrier as explicit
// capture. Idle policy is checked under those locks; the caller must roll back
// when a member still needs its warm interval or new work is queued.
func BeginIdleComputerCapture(ctx context.Context, tx pgx.Tx, request db.BeginComputerCheckpointParams) (db.ComputerCheckpoint, error) {
	cp, err := BeginComputerCapture(ctx, tx, request)
	if err != nil {
		return db.ComputerCheckpoint{}, err
	}
	var due bool
	err = tx.QueryRow(ctx, `SELECT
 NOT EXISTS(SELECT 1 FROM computer_checkpoint_runs m JOIN run_waits w ON w.id=m.run_wait_id
  WHERE m.checkpoint_id=$1 AND (w.idle_timeout_ms IS NULL OR w.created_at+w.idle_timeout_ms*interval '1 millisecond'>clock_timestamp()))
 AND (EXISTS(SELECT 1 FROM computer_checkpoint_runs WHERE checkpoint_id=$1)
  OR c.last_activity_at<=clock_timestamp()-$3*interval '1 millisecond')
 AND NOT EXISTS(SELECT 1 FROM runs r WHERE r.computer_id=c.id AND r.status IN ('queued','retry_delayed'))
 AND NOT EXISTS(SELECT 1 FROM computer_commands p WHERE p.computer_id=c.id AND p.status='pending')
 FROM computers c WHERE c.id=$2`, cp.ID, cp.ComputerID, idleComputerCaptureDelay.Milliseconds()).Scan(&due)
	if err != nil {
		return db.ComputerCheckpoint{}, err
	}
	if !due {
		return db.ComputerCheckpoint{}, pgx.ErrNoRows
	}
	return cp, nil
}

func (d *Authority) captureIdleComputers(ctx context.Context, limit int32) (int, error) {
	var count int
	var failures []error
	var after pgtype.UUID
	// Advance past rejected candidates as well as successful captures. A stale
	// oldest candidate must not monopolize every reconciliation batch.
	for count < int(limit) {
		rows, err := d.pool.Query(ctx, `SELECT i.id,i.environment_id,i.writer_generation,i.membership_revision,i.desired_version
 FROM computer_instances i JOIN computers c ON c.id=i.computer_id
 WHERE ($3::uuid IS NULL OR i.id>$3) AND c.status='active' AND c.desired_state='active' AND i.desired_state='ready'
 AND i.observed_state='ready' AND i.observed_desired_version=i.desired_version
 AND i.admission_state='open' AND i.reclaimed_at IS NULL AND i.writer_expires_at>clock_timestamp()
 AND i.capture_checkpoint_id IS NULL
 AND (EXISTS(SELECT 1 FROM run_leases l WHERE l.computer_instance_id=i.id AND l.process_reconciled_at IS NULL)
  OR c.last_activity_at<=clock_timestamp()-$2*interval '1 millisecond')
 AND NOT EXISTS(SELECT 1 FROM run_leases l LEFT JOIN run_waits w ON w.current_run_lease_id=l.id AND w.suspension_status='hot'
  WHERE l.computer_instance_id=i.id AND l.process_reconciled_at IS NULL
  AND (l.status<>'running' OR w.id IS NULL OR w.condition_status<>'pending' OR w.idle_timeout_ms IS NULL
   OR w.created_at+w.idle_timeout_ms*interval '1 millisecond'>clock_timestamp()))
 AND NOT EXISTS(SELECT 1 FROM computer_commands p WHERE p.computer_instance_id=i.id AND p.process_reconciled_at IS NULL)
 AND NOT EXISTS(SELECT 1 FROM runs r WHERE r.computer_id=i.computer_id AND r.status IN ('queued','retry_delayed'))
 AND NOT EXISTS(SELECT 1 FROM computer_commands p WHERE p.computer_id=i.computer_id AND p.status='pending')
 ORDER BY i.id LIMIT $1`, limit-int32(count), idleComputerCaptureDelay.Milliseconds(), after)
		if err != nil {
			return count, errors.Join(append(failures, err)...)
		}
		var candidates []db.BeginComputerCheckpointParams
		for rows.Next() {
			var p db.BeginComputerCheckpointParams
			if err = rows.Scan(&p.ComputerInstanceID, &p.EnvironmentID, &p.WriterGeneration, &p.MembershipRevision, &p.DesiredVersion); err != nil {
				rows.Close()
				return count, errors.Join(append(failures, err)...)
			}
			p.CheckpointID = pgvalue.UUID(uuid.NewV7())
			candidates = append(candidates, p)
		}
		rows.Close()
		if err = rows.Err(); err != nil {
			return count, errors.Join(append(failures, err)...)
		}
		if len(candidates) == 0 {
			break
		}
		for _, p := range candidates {
			after = p.ComputerInstanceID
			err := d.captureIdleComputer(ctx, p)
			if errors.Is(err, pgx.ErrNoRows) {
				continue
			}
			if err != nil {
				failures = append(failures, err)
			} else {
				count++
			}
		}
	}
	return count, errors.Join(failures...)
}

func (d *Authority) captureIdleComputer(ctx context.Context, request db.BeginComputerCheckpointParams) error {
	tx, err := d.begin(ctx)
	if err != nil {
		return err
	}
	defer rollback(ctx, tx)
	if _, err = BeginIdleComputerCapture(ctx, tx, request); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
