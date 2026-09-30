package dispatch

import (
	"context"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// ComputerRestoreGrant is the exact grant installed by the Worker for a member.
// Run identity alone is insufficient: a receipt must not activate a replacement lease.
type ComputerRestoreGrant struct {
	RunID, LeaseID pgtype.UUID
	LeaseSequence  int64
}

// AcknowledgeComputerRestore records installation of the entire destination's
// grants on the computer.Restore fence before opening admission. Logical wait
// outcomes are acknowledged by each member separately. The caller owns
// rollback on error and commit on success.
func AcknowledgeComputerRestore(ctx context.Context, tx pgx.Tx, destination computer.InstanceRef, checkpointID pgtype.UUID, writerGeneration int64, grants []ComputerRestoreGrant) (db.ComputerInstance, error) {
	restore, err := computer.LockRestore(ctx, tx, destination)
	if err != nil {
		return db.ComputerInstance{}, err
	}
	i := restore.Instance()
	if i.SourceCheckpointID != checkpointID || i.WriterGeneration != writerGeneration || i.ObservedState != "ready" || i.MountState != "mounted" || i.ObservedDesiredVersion != i.DesiredVersion {
		return db.ComputerInstance{}, pgx.ErrNoRows
	}
	q := db.New(tx)
	members, err := q.ListComputerCheckpointRuns(ctx, db.ListComputerCheckpointRunsParams{EnvironmentID: i.EnvironmentID, CheckpointID: checkpointID})
	if err != nil {
		return db.ComputerInstance{}, err
	}
	if len(grants) != len(members) {
		return db.ComputerInstance{}, pgx.ErrNoRows
	}
	installed := make(map[pgtype.UUID]ComputerRestoreGrant, len(grants))
	for _, g := range grants {
		if !g.RunID.Valid || !g.LeaseID.Valid || g.LeaseSequence <= 0 {
			return db.ComputerInstance{}, pgx.ErrNoRows
		}
		if _, exists := installed[g.RunID]; exists {
			return db.ComputerInstance{}, pgx.ErrNoRows
		}
		installed[g.RunID] = g
	}
	if err = restore.LockAcknowledgementMembers(ctx); err != nil {
		return db.ComputerInstance{}, err
	}
	cp, err := q.LockComputerCheckpoint(ctx, db.LockComputerCheckpointParams{EnvironmentID: i.EnvironmentID, ComputerID: i.ComputerID, CheckpointID: checkpointID})
	if err != nil {
		return db.ComputerInstance{}, err
	}
	if cp.Status != "ready" || cp.ResumeComputerInstanceID != i.ID || !cp.ResumeCommittedAt.Valid {
		return db.ComputerInstance{}, pgx.ErrNoRows
	}
	// The open barrier is the durable receipt. Replays must still name exactly the
	// installed grants, even after a member has finished and cleared its current lease.
	for _, m := range members {
		g, ok := installed[m.RunID]
		if !ok {
			return db.ComputerInstance{}, pgx.ErrNoRows
		}
		var matches bool
		err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM run_leases l WHERE id=$1 AND run_id=$2 AND lease_sequence=$3 AND computer_instance_id=$4 AND attempt_number=$5 AND writer_generation=$6 AND NOT EXISTS(SELECT 1 FROM run_leases earlier WHERE earlier.run_id=l.run_id AND earlier.computer_instance_id=l.computer_instance_id AND earlier.lease_sequence<l.lease_sequence))`, g.LeaseID, g.RunID, g.LeaseSequence, i.ID, m.AttemptNumber, writerGeneration).Scan(&matches)
		if err != nil {
			return db.ComputerInstance{}, err
		}
		if !matches {
			return db.ComputerInstance{}, pgx.ErrNoRows
		}
	}
	if i.AdmissionState == "open" || i.AdmissionState == "draining" {
		return i, nil
	}
	// Opening a restored Instance starts its Runs, so it needs admitting supply.
	if !restore.Admitting() || i.AdmissionState != "restoring" {
		return db.ComputerInstance{}, pgx.ErrNoRows
	}
	for _, m := range members {
		g := installed[m.RunID]
		current, err := restoreWaitIsCurrent(ctx, tx, m.RunWaitID)
		if err != nil {
			return db.ComputerInstance{}, err
		}
		if !current {
			return db.ComputerInstance{}, pgx.ErrNoRows
		}
		result, err := tx.Exec(ctx, `UPDATE run_leases l SET status='running',claimed_at=coalesce(claimed_at,clock_timestamp()),started_at=clock_timestamp(),updated_at=clock_timestamp()
 FROM runs r,run_waits w,run_attempts a WHERE l.id=$1 AND l.run_id=r.id AND l.id=r.current_run_lease_id
 AND l.status='assigned' AND l.process_reconciled_at IS NULL AND l.start_deadline_at>clock_timestamp() AND l.expires_at>clock_timestamp()
 AND r.status='waiting' AND r.current_attempt_number=l.attempt_number AND r.active_started_at IS NULL AND r.active_elapsed_ms<r.max_active_duration_ms
 AND a.run_id=r.id AND a.number=l.attempt_number AND a.entrypoint_entered_at IS NOT NULL AND a.terminal_at IS NULL
 AND w.id=$2 AND w.run_id=r.id AND w.current_run_lease_id=l.id AND w.expected_run_revision=r.revision
 AND w.suspension_status='resuming' AND w.suspend_checkpoint_id=$3 AND w.prior_run_lease_id=$4`, g.LeaseID, m.RunWaitID, checkpointID, m.SourceRunLeaseID)
		if err != nil {
			return db.ComputerInstance{}, err
		}
		if result.RowsAffected() != 1 {
			return db.ComputerInstance{}, pgx.ErrNoRows
		}
		if _, err = tx.Exec(ctx, `UPDATE runs SET active_started_at=clock_timestamp(),updated_at=clock_timestamp() WHERE id=$1`, m.RunID); err != nil {
			return db.ComputerInstance{}, err
		}
		wait, err := q.GetRunWait(ctx, db.GetRunWaitParams{ID: m.RunWaitID, RunID: m.RunID, AttemptNumber: m.AttemptNumber})
		if err != nil {
			return db.ComputerInstance{}, err
		}
		result, err = tx.Exec(ctx, `UPDATE session_turns t SET ready_run_lease_id=$2 FROM run_waits w WHERE w.id=$1 AND t.id=w.turn_id AND t.status='running' AND t.interrupt_requested_at IS NULL AND t.settlement_started_at IS NULL`, m.RunWaitID, g.LeaseID)
		if err != nil {
			return db.ComputerInstance{}, err
		}
		if wait.TurnID.Valid && result.RowsAffected() != 1 {
			return db.ComputerInstance{}, pgx.ErrNoRows
		}
	}
	if i, err = restore.Open(ctx); err != nil {
		return db.ComputerInstance{}, err
	}
	// Blocking writes above may consume the remaining preparation or lease budget.
	if err = restore.RecheckReady(ctx); err != nil {
		return db.ComputerInstance{}, err
	}
	var live bool
	err = tx.QueryRow(ctx, `SELECT i.preparation_expires_at>clock_timestamp() AND NOT EXISTS(SELECT 1 FROM run_leases l WHERE l.computer_instance_id=i.id AND (l.status<>'running' OR l.expires_at<=clock_timestamp() OR l.start_deadline_at<=clock_timestamp())) AND NOT EXISTS(SELECT 1 FROM runs r JOIN run_leases l ON l.id=r.current_run_lease_id WHERE l.computer_instance_id=i.id AND (r.active_started_at IS NULL OR r.active_started_at + (r.max_active_duration_ms-r.active_elapsed_ms)*interval '1 millisecond'<=clock_timestamp())) FROM computer_instances i WHERE i.id=$1`, i.ID).Scan(&live)
	if err != nil {
		return db.ComputerInstance{}, err
	}
	if !live {
		return db.ComputerInstance{}, pgx.ErrNoRows
	}
	return i, nil
}
