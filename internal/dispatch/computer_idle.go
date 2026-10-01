package dispatch

import (
	"context"
	"errors"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func (d *Authority) captureIdleComputers(ctx context.Context, limit int32) (int, error) {
	return captureComputers(ctx, d.pool, limit, false, uuid.Nil(), uuid.Nil())
}

func (d *Authority) captureDrainingComputers(ctx context.Context, limit int32) (int, error) {
	return CaptureDrainingComputers(ctx, d.pool, uuid.Nil(), uuid.Nil(), limit)
}

// CaptureDrainingComputers attempts capture after host drain or Wait registration.
// Zero locators select all draining sources for the periodic repair loop. Each
// candidate is independently relocked; an ineligible source remains resident.
func CaptureDrainingComputers(ctx context.Context, database db.TxDB, hostID, instanceID uuid.UUID, limit int32) (int, error) {
	return captureComputers(ctx, database, limit, true, hostID, instanceID)
}

func captureComputers(ctx context.Context, database db.TxDB, limit int32, draining bool, hostID, instanceID uuid.UUID) (int, error) {
	var count int
	var failures []error
	var after pgtype.UUID
	// Advance past rejected candidates as well as successful captures. A stale
	// oldest candidate must not monopolize every reconciliation batch.
	for count < int(limit) {
		rows, err := database.Query(ctx, `SELECT i.id,i.environment_id,i.writer_generation,i.membership_revision,i.desired_version
 FROM computer_instances i JOIN computers c ON c.id=i.computer_id
 WHERE ($3::uuid IS NULL OR i.id>$3)
 AND ($6::uuid IS NULL OR i.worker_host_id=$6) AND ($7::uuid IS NULL OR i.id=$7) AND c.status='active' AND c.desired_state='active' AND i.desired_state='ready'
 AND i.observed_state='ready' AND i.observed_desired_version=i.desired_version
 AND (($5::boolean AND i.admission_state='draining' AND EXISTS(SELECT 1 FROM worker_hosts h WHERE h.id=i.worker_host_id AND h.current_epoch=i.worker_epoch AND h.status='draining'))
  OR (NOT $5::boolean AND i.admission_state='open')) AND i.reclaimed_at IS NULL AND i.writer_expires_at>clock_timestamp()
 AND i.capture_checkpoint_id IS NULL
 AND NOT EXISTS(SELECT 1 FROM computer_checkpoints cp WHERE cp.computer_id=i.computer_id AND cp.source_computer_instance_id=i.id
  AND cp.abort_acknowledged_at>clock_timestamp()-$4*interval '1 millisecond')
 AND ($5::boolean OR EXISTS(SELECT 1 FROM run_leases l WHERE l.computer_instance_id=i.id AND l.process_reconciled_at IS NULL)
  OR c.last_activity_at<=clock_timestamp()-$2*interval '1 millisecond')
 AND NOT EXISTS(SELECT 1 FROM run_leases l LEFT JOIN run_waits w ON w.current_run_lease_id=l.id AND w.suspension_status='hot'
  WHERE l.computer_instance_id=i.id AND l.process_reconciled_at IS NULL
  AND (l.status<>'running' OR w.id IS NULL OR w.condition_status<>'pending' OR (NOT $5::boolean AND (w.idle_timeout_ms IS NULL
   OR w.created_at+w.idle_timeout_ms*interval '1 millisecond'>clock_timestamp()))))
 AND NOT EXISTS(SELECT 1 FROM computer_commands p WHERE p.computer_instance_id=i.id AND p.process_reconciled_at IS NULL)
 AND ($5::boolean OR NOT EXISTS(SELECT 1 FROM runs r WHERE r.computer_id=i.computer_id AND r.status IN ('queued','retry_delayed')))
 AND ($5::boolean OR NOT EXISTS(SELECT 1 FROM computer_commands p WHERE p.computer_id=i.computer_id AND p.status='pending'))
 ORDER BY i.id LIMIT $1`, limit-int32(count), computer.IdleCaptureDelay.Milliseconds(), after, computer.CaptureRetryDelay.Milliseconds(), draining, pgtype.UUID{Bytes: hostID, Valid: hostID != uuid.Nil()}, pgtype.UUID{Bytes: instanceID, Valid: instanceID != uuid.Nil()})
		if err != nil {
			return count, errors.Join(append(failures, err)...)
		}
		var candidates []computer.Capture
		for rows.Next() {
			var p computer.Capture
			if err = rows.Scan(&p.InstanceID, &p.EnvironmentID, &p.WriterGeneration, &p.MembershipRevision, &p.DesiredVersion); err != nil {
				rows.Close()
				return count, errors.Join(append(failures, err)...)
			}
			p.CheckpointID = uuid.NewV7()
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
			after = pgvalue.UUID(p.InstanceID)
			err := captureComputer(ctx, database, p, draining)
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

func captureComputer(ctx context.Context, database db.TxBeginner, capture computer.Capture, draining bool) error {
	tx, err := database.Begin(ctx)
	if err != nil {
		return err
	}
	defer rollback(ctx, tx)
	if draining {
		_, err = computer.BeginDrainCapture(ctx, tx, capture)
	} else {
		_, err = computer.BeginIdleCapture(ctx, tx, capture)
	}
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}
