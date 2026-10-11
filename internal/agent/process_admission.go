package agent

import (
	"context"
	"errors"
	"math"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
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

func readProcessAllocation(ctx context.Context, q db.DBTX, env, session uuid.UUID, epoch int64) (ProcessAllocation, error) {
	r := ProcessAllocation{EnvironmentID: env, SessionID: session, Epoch: epoch}
	err := q.QueryRow(ctx, `SELECT p.computer_id,p.computer_lease_epoch,p.status,l.computer_instance_id,l.worker_host_id,l.worker_epoch
 FROM session_processes p JOIN computer_leases l ON (l.environment_id,l.computer_id,l.epoch)=(p.environment_id,p.computer_id,p.computer_lease_epoch)
 WHERE p.environment_id=$1 AND p.session_id=$2 AND p.epoch=$3`, env, session, epoch).Scan(&r.ComputerID, &r.LeaseEpoch, &r.Status, &r.InstanceID, &r.HostID, &r.HostEpoch)
	return r, err
}

// AllocateSessionProcess is called by trusted CP discovery after VM readiness.
// The retained epoch is the retry identity; a live or unfenced old process never
// becomes permission to rerun setup. Explicit resume must clear failure holds,
// and physical stop must be confirmed before a replacement can be admitted.
func (a *Allocator) AllocateSessionProcess(ctx context.Context, env, session uuid.UUID, epoch int64) (ProcessAllocation, error) {
	if env == uuid.Nil() || session == uuid.Nil() || epoch <= 0 {
		return ProcessAllocation{}, ErrInvalidInput
	}
	if r, err := readProcessAllocation(ctx, a.database, env, session, epoch); err == nil {
		return r, nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return ProcessAllocation{}, err
	}
	var result ProcessAllocation
	err := db.RunTx(ctx, a.database, func(tx pgx.Tx) error {
		if err := allocationLockTimeout(ctx, tx); err != nil {
			return err
		}
		var group, host uuid.UUID
		var hostEpoch int64
		var region string
		if err := tx.QueryRow(ctx, `SELECT h.worker_group_id,h.id,l.worker_epoch,g.region_id FROM sessions s JOIN computer_leases l
 ON (l.environment_id,l.computer_id)=(s.environment_id,s.computer_id) JOIN worker_hosts h ON h.id=l.worker_host_id JOIN worker_groups g ON g.id=h.worker_group_id
 WHERE s.environment_id=$1 AND s.id=$2 AND l.fenced_at IS NULL`, env, session).Scan(&group, &host, &hostEpoch, &region); err != nil {
			return err
		}
		admitting, err := workergroup.LockDispatchSupply(ctx, tx, workergroup.DispatchSupply{GroupID: pgvalue.UUID(group), HostID: pgvalue.UUID(host), Epoch: hostEpoch, RegionID: region, RunArchitecture: string(definition.ArchitectureX8664), Continuation: true})
		if err != nil {
			return err
		}
		o, err := lockSession(ctx, tx, env, session)
		if err != nil {
			return err
		}
		if r, err := readProcessAllocation(ctx, tx, env, session, epoch); err == nil {
			result = r
			return nil
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if !admitting || (o.lifecycle != "open" && o.lifecycle != "closing") {
			return ErrNotReady
		}
		assigned, err := assignSessionProcess(ctx, tx, env, session, epoch, host, hostEpoch, 0)
		if err != nil {
			return err
		}
		if !assigned {
			return ErrNotReady
		}
		result, err = readProcessAllocation(ctx, tx, env, session, epoch)
		if err != nil {
			return err
		}
		// Foreign-key waits cannot turn an expired VM lease into fresh setup authority.
		return executionPhysical(ctx, tx, Execution{EnvironmentID: env, SessionID: session, ProcessEpoch: epoch, LeaseEpoch: result.LeaseEpoch, WorkerHostID: host, WorkerEpoch: hostEpoch}, true, true)
	})
	if err != nil {
		return ProcessAllocation{}, allocationError(err)
	}
	return result, nil
}

// assignSessionProcess shares initial-cohort and later admission eligibility.
// freshLease is nonzero only inside the transaction creating that acquiring lease.
// Assignment does not authorize attachment or setup before VM readiness.
func assignSessionProcess(ctx context.Context, tx pgx.Tx, env, session uuid.UUID, epoch int64, host uuid.UUID, hostEpoch, freshLease int64) (bool, error) {
	changed, err := tx.Exec(ctx, `INSERT INTO session_processes(environment_id,session_id,epoch,computer_id,computer_lease_epoch,status)
 SELECT s.environment_id,s.id,$3,c.id,l.epoch,'starting' FROM sessions s
 JOIN deployments d ON (d.environment_id,d.id)=(s.environment_id,s.deployment_id)
 JOIN computers c ON (c.environment_id,c.id)=(s.environment_id,s.computer_id)
 JOIN computer_leases l ON (l.environment_id,l.computer_id)=(c.environment_id,c.id)
 WHERE s.environment_id=$1 AND s.id=$2 AND s.status IN ('open','closing')
 AND l.worker_host_id=$4 AND l.worker_epoch=$5 AND l.fenced_at IS NULL
 AND (($6::bigint=0 AND l.status='active' AND l.expires_at>clock_timestamp())
 OR ($6::bigint>0 AND l.epoch=$6 AND l.status='acquiring' AND l.delivered_at IS NULL AND l.initialized_at IS NULL AND l.restored_from_save_id IS NULL))
 AND c.initial_root_id IS NOT NULL AND c.preparation_failed_at IS NULL AND c.integrity_fault_at IS NULL AND c.deleted_at IS NULL AND d.execution_revoked_at IS NULL
 AND `+noSessionHoldsSQL+`
 AND NOT EXISTS(SELECT 1 FROM session_processes p WHERE p.environment_id=s.environment_id AND p.session_id=s.id AND p.fenced_at IS NULL)
 AND (SELECT COALESCE(max(p.epoch),0) FROM session_processes p WHERE p.environment_id=s.environment_id AND p.session_id=s.id)=$3::bigint-1
 AND EXISTS(SELECT 1 FROM turns t WHERE t.environment_id=s.environment_id AND t.session_id=s.id AND t.status='queued')
 AND NOT EXISTS(SELECT 1 FROM turns t WHERE t.environment_id=s.environment_id AND t.session_id=s.id AND t.status IN ('running','finalizing'))
 AND NOT EXISTS(SELECT 1 FROM computer_secret_revocations revoked WHERE revoked.environment_id=c.environment_id AND revoked.computer_id=c.id)
 AND NOT EXISTS(SELECT 1 FROM computer_checkpoints cp WHERE cp.environment_id=c.environment_id AND cp.computer_id=c.id AND (cp.status IN ('capturing','sealed','ready','restoring','aborting') OR (cp.status='consumed' AND cp.controls_reconciled_at IS NULL)))
 AND NOT EXISTS(SELECT 1 FROM computer_saves save WHERE save.environment_id=c.environment_id AND save.computer_id=c.id AND save.computer_lease_epoch<>l.epoch AND save.status IN ('requested','captured'))`, env, session, epoch, host, hostEpoch, freshLease)
	if err != nil {
		return false, err
	}
	return changed.RowsAffected() == 1, nil
}

// lockInitialComputerSessions discovers roots before the Computer lock. The
// Environment admission lock prevents new Sessions or inputs crossing this cut;
// controls serialize on the roots and final eligibility is checked under locks.
func lockInitialComputerSessions(ctx context.Context, tx pgx.Tx, env, computer uuid.UUID) ([]uuid.UUID, error) {
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
	if _, err := lockSessionsAndComputers(ctx, tx, env, sessions, []uuid.UUID{computer}); err != nil {
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

func assignInitialComputerSessions(ctx context.Context, tx pgx.Tx, r ComputerAllocation, sessions []uuid.UUID) error {
	for _, session := range sessions {
		var epoch int64
		if err := tx.QueryRow(ctx, `SELECT COALESCE(max(epoch),0) FROM session_processes WHERE environment_id=$1 AND session_id=$2`, r.EnvironmentID, session).Scan(&epoch); err != nil {
			return err
		}
		if epoch == math.MaxInt64 {
			continue
		}
		if _, err := assignSessionProcess(ctx, tx, r.EnvironmentID, session, epoch+1, r.HostID, r.HostEpoch, r.Epoch); err != nil {
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
	return allocationError(db.RunTx(ctx, database, func(tx pgx.Tx) error {
		if err := allocationLockTimeout(ctx, tx); err != nil {
			return err
		}
		if _, err := lockRuntimeAuthority(ctx, tx, host, e); err != nil {
			return err
		}
		var currentAttachment int64
		if err := tx.QueryRow(ctx, `SELECT attachment_sequence FROM session_processes WHERE environment_id=$1 AND session_id=$2 AND epoch=$3 FOR NO KEY UPDATE`, e.EnvironmentID, e.SessionID, e.ProcessEpoch).Scan(&currentAttachment); err != nil {
			return err
		}
		if currentAttachment != attachment {
			return ErrNotReady
		}
		control, err := desiredSessionControl(ctx, tx, e)
		if err != nil {
			return err
		}
		if control == "shutdown" {
			if _, err := lockRuntimeAuthority(ctx, tx, host, e); err != nil {
				return err
			}
			return ErrConflict
		}
		changed, err := tx.Exec(ctx, `UPDATE session_processes SET status='ready' WHERE environment_id=$1 AND session_id=$2 AND epoch=$3 AND computer_lease_epoch=$4
 AND status IN ('starting','ready') AND fenced_at IS NULL AND attachment_sequence=$5 AND failure_recorded_at IS NULL`, e.EnvironmentID, e.SessionID, e.ProcessEpoch, e.LeaseEpoch, attachment)
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
		return err
	}))
}
