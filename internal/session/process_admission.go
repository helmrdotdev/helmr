package session

import (
	"context"
	"errors"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"math"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
)

// ProcessAllocation is a durable assignment, not a guest attachment grant.
// Replacement transports attach to this process and never create a generation.
type ProcessAllocation struct {
	EnvironmentID, SessionID, ComputerID, InstanceID, HostID uuid.UUID
	Epoch, LeaseEpoch, HostEpoch                             int64
	Status                                                   string
}

func ReadProcessAllocation(ctx context.Context, q db.DBTX, env, session uuid.UUID, epoch int64) (ProcessAllocation, error) {
	r := ProcessAllocation{EnvironmentID: env, SessionID: session, Epoch: epoch}
	err := q.QueryRow(ctx, `SELECT p.computer_id,p.computer_lease_epoch,p.status,l.allocation_id,l.worker_host_id,l.worker_epoch
 FROM session_processes p JOIN computer_leases l ON (l.environment_id,l.computer_id,l.epoch)=(p.environment_id,p.computer_id,p.computer_lease_epoch)
 WHERE p.environment_id=$1 AND p.session_id=$2 AND p.epoch=$3`, env, session, epoch).Scan(&r.ComputerID, &r.LeaseEpoch, &r.Status, &r.InstanceID, &r.HostID, &r.HostEpoch)
	return r, err
}

// AdmitProcess assigns the next process of a Session on its Computer's live
// lease, or returns the retained assignment for the same epoch. The caller
// holds the worker supply gate; admitting reports whether supply still admits
// continuation work. An empty successful result means no process was assigned;
// the caller must commit any queued-work settlement before reporting not ready.
func AdmitProcess(ctx context.Context, auditPolicy auditpolicy.Policy, tx pgx.Tx, env, session uuid.UUID, epoch int64, host uuid.UUID, hostEpoch int64, admitting bool) (ProcessAllocation, error) {
	o, err := lockSession(ctx, tx, env, session)
	if err != nil {
		return ProcessAllocation{}, err
	}
	if r, err := ReadProcessAllocation(ctx, tx, env, session, epoch); err == nil {
		return r, nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return ProcessAllocation{}, err
	}
	if !admitting || (o.lifecycle != "open" && o.lifecycle != "closing") {
		return ProcessAllocation{}, ErrNotReady
	}
	assigned, err := AssignProcess(ctx, auditPolicy, tx, env, session, epoch, host, hostEpoch, 0)
	if err != nil {
		return ProcessAllocation{}, err
	}
	if !assigned {
		// An empty result commits any denial settlement written by AssignProcess.
		// Errors after assignment must still roll the entire admission back.
		return ProcessAllocation{}, nil
	}
	result, err := ReadProcessAllocation(ctx, tx, env, session, epoch)
	if err != nil {
		return ProcessAllocation{}, err
	}
	// Foreign-key waits cannot turn an expired VM lease into fresh setup authority.
	return result, executionPhysical(ctx, tx, Execution{EnvironmentID: env, SessionID: session, ProcessEpoch: epoch, LeaseEpoch: result.LeaseEpoch, WorkerHostID: host, WorkerEpoch: hostEpoch}, true)
}

// AssignProcess shares initial-cohort and later admission eligibility.
// freshLease is nonzero only inside the transaction creating that acquiring lease.
// Assignment does not authorize attachment or setup before VM readiness.
func AssignProcess(ctx context.Context, auditPolicy auditpolicy.Policy, tx pgx.Tx, env, session uuid.UUID, epoch int64, host uuid.UUID, hostEpoch, freshLease int64) (bool, error) {
	if err := authorizeRuntimeOrigin(ctx, tx, env, session); errors.Is(err, ErrDenied) {
		return false, failQueuedPermission(ctx, auditPolicy, tx, env, session)
	} else if err != nil {
		return false, err
	}
	if revoked, err := sessionRawSecretRevoked(ctx, tx, env, session); err != nil || revoked {
		if err != nil {
			return false, err
		}
		return false, failQueuedSecretUnavailable(ctx, auditPolicy, tx, env, session)
	}
	changed, err := tx.Exec(ctx, `INSERT INTO session_processes(environment_id,session_id,epoch,computer_id,computer_lease_epoch,status)
 SELECT s.environment_id,s.id,$3,c.id,l.epoch,'starting' FROM sessions s
 JOIN computers c ON (c.environment_id,c.id)=(s.environment_id,s.computer_id)
 JOIN computer_leases l ON (l.environment_id,l.computer_id)=(c.environment_id,c.id)
 WHERE s.environment_id=$1 AND s.id=$2 AND s.status IN ('open','closing')
 AND l.worker_host_id=$4 AND l.worker_epoch=$5 AND l.fenced_at IS NULL
 AND (($6::bigint=0 AND l.status='active' AND l.expires_at>clock_timestamp())
 OR ($6::bigint>0 AND l.epoch=$6 AND l.status='acquiring' AND l.delivered_at IS NULL AND l.initialized_at IS NULL AND (l.restored_from_save_id IS NULL OR l.disk_only_recovery)))
 AND c.initial_root_id IS NOT NULL AND c.preparation_failed_at IS NULL AND c.integrity_fault_at IS NULL AND c.deleted_at IS NULL
 AND `+db.NoSessionHoldsSQL+`
 AND NOT EXISTS(SELECT 1 FROM session_processes p WHERE p.environment_id=s.environment_id AND p.session_id=s.id AND p.fenced_at IS NULL)
 AND (SELECT COALESCE(max(p.epoch),0) FROM session_processes p WHERE p.environment_id=s.environment_id AND p.session_id=s.id)=$3::bigint-1
 AND EXISTS(SELECT 1 FROM turns t WHERE t.environment_id=s.environment_id AND t.session_id=s.id AND t.status='queued')
 AND NOT EXISTS(SELECT 1 FROM turns t WHERE t.environment_id=s.environment_id AND t.session_id=s.id AND t.status IN ('running'))
 AND NOT EXISTS(SELECT 1 FROM checkpoints cp WHERE cp.environment_id=c.environment_id AND cp.computer_id=c.id AND (cp.status IN ('capturing','sealed','ready','restoring','aborting') OR (cp.status='consumed' AND cp.controls_reconciled_at IS NULL)))
 AND NOT EXISTS(SELECT 1 FROM computer_saves save WHERE save.environment_id=c.environment_id AND save.computer_id=c.id AND save.computer_lease_epoch<>l.epoch AND save.status IN ('requested','captured'))`, env, session, epoch, host, hostEpoch, freshLease)
	if err != nil {
		return false, err
	}
	if changed.RowsAffected() == 0 {
		return false, nil
	}
	return true, notifySessionsChanged(ctx, tx, env, session)
}

// LockInitialSessions discovers roots before the Computer lock. The
// Environment admission lock prevents new Sessions or inputs crossing this cut;
// controls serialize on the roots and final eligibility is checked under locks.
func LockInitialSessions(ctx context.Context, tx pgx.Tx, env, computer uuid.UUID) ([]uuid.UUID, error) {
	rows, err := tx.Query(ctx, `SELECT id FROM sessions WHERE environment_id=$1 AND computer_id=$2 AND status IN ('open','closing') ORDER BY id`, env, computer)
	if err != nil {
		return nil, err
	}
	sessions := make([]uuid.UUID, 0)
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		sessions = append(sessions, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if _, err := LockSessionsAndComputers(ctx, tx, env, sessions, []uuid.UUID{computer}); err != nil {
		return nil, err
	}
	var changed bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM sessions WHERE environment_id=$1 AND computer_id=$2 AND status IN ('open','closing') AND NOT(id=ANY($3::uuid[])))`, env, computer, sessions).Scan(&changed); err != nil {
		return nil, err
	}
	if changed {
		return nil, ErrNotReady
	}
	return sessions, nil
}

// AuthorizeInitialSessions settles queued work that can never start before
// reserving a VM: revoked authority, or a revoked raw Secret on its Computer.
// A false result means no Session in this cohort can start.
func AuthorizeInitialSessions(ctx context.Context, auditPolicy auditpolicy.Policy, tx pgx.Tx, env uuid.UUID, sessions []uuid.UUID) (bool, error) {
	allowed := len(sessions) == 0
	for _, id := range sessions {
		err := authorizeRuntimeOrigin(ctx, tx, env, id)
		if errors.Is(err, ErrDenied) {
			if err := failQueuedPermission(ctx, auditPolicy, tx, env, id); err != nil {
				return false, err
			}
			continue
		} else if err != nil {
			return false, err
		}
		revoked, err := sessionRawSecretRevoked(ctx, tx, env, id)
		if err != nil {
			return false, err
		}
		if revoked {
			if err := failQueuedSecretUnavailable(ctx, auditPolicy, tx, env, id); err != nil {
				return false, err
			}
			continue
		}
		allowed = true
	}
	return allowed, nil
}

func sessionRawSecretRevoked(ctx context.Context, tx pgx.Tx, env, session uuid.UUID) (bool, error) {
	var computerID uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT computer_id FROM sessions WHERE environment_id=$1 AND id=$2 AND computer_id IS NOT NULL`, env, session).Scan(&computerID); errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	return computer.RawSecretRevoked(ctx, tx, env, computerID)
}

// RefuseRevokedSessionStart settles a starting process whose start was refused
// because its Computer gained a revoked raw Secret binding after assignment.
// Its queued input fails with secret_unavailable and the process receives
// ordinary stop intent, so the host stops retrying the start.
func RefuseRevokedSessionStart(ctx context.Context, auditPolicy auditpolicy.Policy, database db.TxBeginner, e Execution) error {
	return hideMissing(db.RunTx(ctx, database, func(tx pgx.Tx) error {
		if _, err := lockSession(ctx, tx, e.EnvironmentID, e.SessionID); err != nil {
			return err
		}
		var starting bool
		if err := tx.QueryRow(ctx, `SELECT status='starting' AND fenced_at IS NULL AND failure_recorded_at IS NULL FROM session_processes WHERE environment_id=$1 AND session_id=$2 AND epoch=$3 FOR NO KEY UPDATE`, e.EnvironmentID, e.SessionID, e.ProcessEpoch).Scan(&starting); err != nil {
			return err
		}
		if !starting {
			return nil
		}
		revoked, err := sessionRawSecretRevoked(ctx, tx, e.EnvironmentID, e.SessionID)
		if err != nil || !revoked {
			return err
		}
		if err = failQueuedSecretUnavailable(ctx, auditPolicy, tx, e.EnvironmentID, e.SessionID); err != nil {
			return err
		}
		// This is stop intent, never evidence that the physical process is absent.
		if _, err = tx.Exec(ctx, `UPDATE session_processes SET failure_recorded_at=clock_timestamp(),status='stopping' WHERE environment_id=$1 AND session_id=$2 AND epoch=$3`, e.EnvironmentID, e.SessionID, e.ProcessEpoch); err != nil {
			return err
		}
		return notifySessionsChanged(ctx, tx, e.EnvironmentID, e.SessionID)
	}))
}

func AssignInitialProcesses(ctx context.Context, auditPolicy auditpolicy.Policy, tx pgx.Tx, r computer.ComputerAllocation, sessions []uuid.UUID) error {
	for _, session := range sessions {
		var epoch int64
		if err := tx.QueryRow(ctx, `SELECT COALESCE(max(epoch),0) FROM session_processes WHERE environment_id=$1 AND session_id=$2`, r.EnvironmentID, session).Scan(&epoch); err != nil {
			return err
		}
		if epoch == math.MaxInt64 {
			continue
		}
		if _, err := AssignProcess(ctx, auditPolicy, tx, r.EnvironmentID, session, epoch+1, r.HostID, r.HostEpoch, r.Epoch); err != nil {
			return err
		}
	}
	return nil
}

// ObserveSessionReady records setup success from the exact current attachment.
// Holds and lifecycle controls retain precedence; readiness never clears them.
// ErrConflict means shutdown superseded this otherwise valid observation. The
// transport may retire the Ready event while continuing shutdown reconciliation.
func ObserveSessionReady(ctx context.Context, database db.TxBeginner, host workergroup.HostPrincipal, e Execution, attachment int64) error {
	if attachment <= 0 {
		return ErrInvalidInput
	}
	return notReadyOnLock(db.RunTx(ctx, database, func(tx pgx.Tx) error {
		if err := shortLockTimeout(ctx, tx); err != nil {
			return err
		}
		if _, err := lockRuntimeAuthority(ctx, tx, host, e); err != nil {
			return err
		}
		var currentAttachment int64
		var stopPending, alreadyReady bool
		if err := tx.QueryRow(ctx, `SELECT attachment_sequence,stop_deadline_at IS NOT NULL,status='ready' FROM session_processes WHERE environment_id=$1 AND session_id=$2 AND epoch=$3 FOR NO KEY UPDATE`, e.EnvironmentID, e.SessionID, e.ProcessEpoch).Scan(&currentAttachment, &stopPending, &alreadyReady); err != nil {
			return err
		}
		if currentAttachment != attachment {
			return ErrNotReady
		}
		control, err := desiredSessionControl(ctx, tx, e)
		if err != nil {
			return err
		}
		if control == "shutdown" || stopPending {
			if _, err := lockRuntimeAuthority(ctx, tx, host, e); err != nil {
				return err
			}
			return ErrConflict
		}
		changed, err := tx.Exec(ctx, `UPDATE session_processes SET status='ready' WHERE environment_id=$1 AND session_id=$2 AND epoch=$3 AND computer_lease_epoch=$4
 AND status IN ('starting','ready') AND fenced_at IS NULL AND attachment_sequence=$5 AND failure_recorded_at IS NULL AND stop_deadline_at IS NULL`, e.EnvironmentID, e.SessionID, e.ProcessEpoch, e.LeaseEpoch, attachment)
		if err != nil {
			return err
		}
		if changed.RowsAffected() != 1 {
			return ErrNotReady
		}
		// Readiness is a retained setup fact, not business-operation admission.
		// A restored outbox can replay it while the exact checkpoint member's
		// lease is still acquiring; rejecting it would block control receipts
		// needed to finish restoration. Recheck that control authority after the
		// process write; Dispatch still requires active physical authority.
		_, err = lockRuntimeAuthority(ctx, tx, host, e)
		if err != nil {
			return err
		}
		if alreadyReady {
			return nil
		}
		return notifySessionsChanged(ctx, tx, e.EnvironmentID, e.SessionID)
	}))
}
