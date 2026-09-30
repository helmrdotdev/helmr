package computer

import (
	"context"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// IdleCaptureDelay is how long an Instance without resident members stays
// warm after the Computer's last activity before idle capture parks it.
const IdleCaptureDelay = 30 * time.Second

// Capture addresses the Instance incarnation a capture seals: its writer
// generation, membership revision and desired version as the caller observed
// them, and the id of the new checkpoint.
type Capture struct {
	CheckpointID       uuid.UUID
	EnvironmentID      uuid.UUID
	InstanceID         uuid.UUID
	WriterGeneration   int64
	MembershipRevision int64
	DesiredVersion     int64
}

// BeginCapture seals the complete resident set of the Instance, including an
// empty set, into a new creating checkpoint. It locks the worker Group and
// Host at the Instance's epoch without comparing claim versions, the
// Computer, the Instance, then the Sessions, Runs, Attempts, Run leases,
// Waits and Session turns of the Instance's unreconciled leases in stable
// order, and checks deadlines after the last lock. The caller must commit on
// success and roll back on any error. The changed Instance desired version is
// the durable physical capture intent. A fence that no longer holds, or a
// member that cannot be captured, returns pgx.ErrNoRows.
func BeginCapture(ctx context.Context, tx pgx.Tx, capture Capture) (db.ComputerCheckpoint, error) {
	request := db.BeginComputerCheckpointParams{CheckpointID: pgvalue.UUID(capture.CheckpointID), ComputerInstanceID: pgvalue.UUID(capture.InstanceID), EnvironmentID: pgvalue.UUID(capture.EnvironmentID), WriterGeneration: capture.WriterGeneration, MembershipRevision: capture.MembershipRevision, DesiredVersion: capture.DesiredVersion}
	var groupID, workerID uuid.UUID
	var computerID pgtype.UUID
	var region string
	var epoch int64
	err := tx.QueryRow(ctx, `SELECT worker_group_id,worker_host_id,computer_id,region_id,worker_epoch
 FROM computer_instances WHERE id=$1 AND environment_id=$2`, request.ComputerInstanceID, request.EnvironmentID).Scan(&groupID, &workerID, &computerID, &region, &epoch)
	if err != nil {
		return db.ComputerCheckpoint{}, err
	}
	q := db.New(tx)
	host, err := workergroup.LockHostIgnoringClaims(ctx, q, groupID, region, workerID, epoch)
	if err != nil {
		return db.ComputerCheckpoint{}, err
	}
	worker := host.Host
	c, err := q.LockComputer(ctx, db.LockComputerParams{EnvironmentID: request.EnvironmentID, ID: computerID})
	if err != nil {
		return db.ComputerCheckpoint{}, err
	}
	i, err := q.LockComputerInstance(ctx, db.LockComputerInstanceParams{EnvironmentID: request.EnvironmentID, ComputerID: computerID})
	if err != nil {
		return db.ComputerCheckpoint{}, err
	}
	if c.Status != "active" || c.DesiredState != "active" || c.DeletedAt.Valid || len(c.RecoveryFailure) > 0 || len(c.PreparationFailure) > 0 || c.DirtyState == "dirty_state_lost" || c.DirtyState == "capture_failed" ||
		i.ID != request.ComputerInstanceID || i.WorkerHostID != pgvalue.UUID(workerID) || i.WorkerGroupID != pgvalue.UUID(groupID) || i.WorkerEpoch != epoch || !worker.VMPlatformID.Valid || i.VMPlatformID != worker.VMPlatformID.String || i.WriterGeneration != c.WriterGeneration {
		return db.ComputerCheckpoint{}, pgx.ErrNoRows
	}
	// Admissions and detachments take the Computer lock. Lock all resident owners
	// in stable order before reading eligibility or capturing an Actor cursor.
	for _, query := range []string{
		`SELECT s.id FROM sessions s WHERE s.id IN (SELECT r.session_id FROM runs r JOIN run_leases l ON l.run_id=r.id WHERE l.computer_instance_id=$1 AND l.process_reconciled_at IS NULL) ORDER BY s.id FOR UPDATE`,
		`SELECT r.id FROM runs r WHERE r.id IN (SELECT l.run_id FROM run_leases l WHERE l.computer_instance_id=$1 AND l.process_reconciled_at IS NULL) ORDER BY r.id FOR UPDATE`,
		`SELECT a.run_id FROM run_attempts a WHERE EXISTS (SELECT 1 FROM run_leases l WHERE l.computer_instance_id=$1 AND l.process_reconciled_at IS NULL AND l.run_id=a.run_id AND l.attempt_number=a.number) ORDER BY a.run_id,a.number FOR UPDATE`,
		`SELECT l.id FROM run_leases l WHERE l.computer_instance_id=$1 AND l.process_reconciled_at IS NULL ORDER BY l.run_id,l.id FOR UPDATE`,
		`SELECT w.id FROM run_waits w WHERE EXISTS (SELECT 1 FROM run_leases l WHERE l.computer_instance_id=$1 AND l.process_reconciled_at IS NULL AND l.run_id=w.run_id AND l.attempt_number=w.attempt_number) ORDER BY w.run_id,w.id FOR UPDATE`,
		`SELECT t.id FROM session_turns t WHERE t.id IN (SELECT s.active_turn_id FROM sessions s JOIN runs r ON r.session_id=s.id JOIN run_leases l ON l.run_id=r.id WHERE l.computer_instance_id=$1 AND l.process_reconciled_at IS NULL) ORDER BY t.session_id,t.id FOR UPDATE`,
	} {
		if _, err = tx.Exec(ctx, query, i.ID); err != nil {
			return db.ComputerCheckpoint{}, err
		}
	}
	// Evaluate wall-clock deadlines only after the final potentially blocking lock.
	fresh, err := q.GetComputerCaptureWorkerFresh(ctx, db.GetComputerCaptureWorkerFreshParams{ID: worker.ID, WorkerFreshnessSeconds: workergroup.ObservationFreshnessSeconds, ExpiresAt: request.ExpiresAt})
	if err != nil {
		return db.ComputerCheckpoint{}, err
	}
	if !fresh {
		return db.ComputerCheckpoint{}, pgx.ErrNoRows
	}
	rows, err := tx.Query(ctx, `SELECT l.id,l.run_id,l.attempt_number,l.lease_sequence,w.id,
 CASE WHEN r.entrypoint_kind='actor' THEN coalesce(t.sequence,s.committed_input_sequence) END,
 coalesce(l.status='running' AND l.expires_at>clock_timestamp() AND l.writer_generation=$2
 AND r.status='waiting' AND r.current_run_lease_id=l.id AND r.current_attempt_number=l.attempt_number
 AND r.terminal_at IS NULL AND a.terminal_at IS NULL AND a.entrypoint_entered_at IS NOT NULL
 AND w.condition_status='pending' AND w.suspension_status='hot' AND w.suspend_checkpoint_id IS NULL
 AND w.expected_run_revision=r.revision
 AND (w.due_at IS NULL OR w.due_at>clock_timestamp()) AND (w.timeout_at IS NULL OR w.timeout_at>clock_timestamp())
 AND (r.entrypoint_kind='task' OR (r.entrypoint_kind='actor' AND s.current_run_id=r.id
 AND s.status IN ('open','closing') AND s.cancel_requested_at IS NULL AND s.dispatch_hold_id IS NULL
 AND ((s.active_turn_id IS NULL AND w.turn_id IS NULL)
 OR (s.active_turn_id=w.turn_id AND w.turn_session_id=s.id AND w.turn_run_generation=s.run_generation
 AND t.status='running' AND t.run_id=r.id AND t.attempt_number=l.attempt_number AND t.run_generation=s.run_generation
 AND t.sequence=s.committed_input_sequence+1
 AND t.settlement_started_at IS NULL AND t.interrupt_requested_at IS NULL)))),false)
 FROM run_leases l JOIN runs r ON r.id=l.run_id
 JOIN run_attempts a ON a.run_id=l.run_id AND a.number=l.attempt_number
 LEFT JOIN run_waits w ON w.run_id=l.run_id AND w.attempt_number=l.attempt_number AND w.current_run_lease_id=l.id AND w.suspension_status='hot'
 LEFT JOIN sessions s ON s.id=r.session_id LEFT JOIN session_turns t ON t.id=s.active_turn_id
 WHERE l.computer_instance_id=$1 AND l.process_reconciled_at IS NULL ORDER BY l.run_id,l.id`, i.ID, i.WriterGeneration)
	if err != nil {
		return db.ComputerCheckpoint{}, err
	}
	type member struct {
		leaseID, runID, waitID pgtype.UUID
		attempt                int32
		sequence               int64
		cursor                 pgtype.Int8
	}
	var members []member
	for rows.Next() {
		var m member
		var eligible bool
		if err = rows.Scan(&m.leaseID, &m.runID, &m.attempt, &m.sequence, &m.waitID, &m.cursor, &eligible); err != nil {
			rows.Close()
			return db.ComputerCheckpoint{}, err
		}
		if !eligible {
			rows.Close()
			return db.ComputerCheckpoint{}, pgx.ErrNoRows
		}
		members = append(members, m)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return db.ComputerCheckpoint{}, err
	}
	checkpoint, err := q.BeginComputerCheckpoint(ctx, request)
	if err != nil {
		return db.ComputerCheckpoint{}, err
	}
	for _, m := range members {
		if _, err = q.CreateComputerCheckpointRun(ctx, db.CreateComputerCheckpointRunParams{CheckpointID: checkpoint.ID, EnvironmentID: checkpoint.EnvironmentID, RunLeaseID: m.leaseID, RunWaitID: m.waitID, ActorSpeculativeInputSequence: m.cursor}); err != nil {
			return db.ComputerCheckpoint{}, err
		}
	}
	// Seal every membership row before transitioning any logical waiter or grant.
	for _, m := range members {
		if _, err = q.MarkCheckpointMemberWaiting(ctx, db.MarkCheckpointMemberWaitingParams{CheckpointID: checkpoint.ID, WaitID: m.waitID}); err != nil {
			return db.ComputerCheckpoint{}, err
		}
		if _, err = q.BeginRunLeaseCheckpoint(ctx, db.BeginRunLeaseCheckpointParams{ID: m.leaseID, RunID: m.runID, ComputerID: computerID, AttemptNumber: m.attempt, LeaseSequence: m.sequence}); err != nil {
			return db.ComputerCheckpoint{}, err
		}
	}
	return checkpoint, nil
}

// BeginIdleCapture is BeginCapture under the idle policy, checked under the
// capture's locks: every sealed member's Wait has passed its idle timeout,
// an Instance without members has been inactive for IdleCaptureDelay, and
// the Computer has no queued Run or pending Command. A capture that is not
// due returns pgx.ErrNoRows and the caller must roll back.
func BeginIdleCapture(ctx context.Context, tx pgx.Tx, capture Capture) (db.ComputerCheckpoint, error) {
	cp, err := BeginCapture(ctx, tx, capture)
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
 FROM computers c WHERE c.id=$2`, cp.ID, cp.ComputerID, IdleCaptureDelay.Milliseconds()).Scan(&due)
	if err != nil {
		return db.ComputerCheckpoint{}, err
	}
	if !due {
		return db.ComputerCheckpoint{}, pgx.ErrNoRows
	}
	return cp, nil
}
