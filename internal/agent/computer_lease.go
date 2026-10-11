package agent

import (
	"context"
	"errors"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
)

// ComputerLeaseIdentity names the exact physical allocation, not logical Session
// continuity. Restoring a retained Session always uses a different allocation.
type ComputerLeaseIdentity struct {
	EnvironmentID uuid.UUID
	ComputerID    uuid.UUID
	InstanceID    uuid.UUID
	Epoch         int64
}

func (identity ComputerLeaseIdentity) valid() bool {
	return identity.EnvironmentID != uuid.Nil() && identity.ComputerID != uuid.Nil() && identity.InstanceID != uuid.Nil() && identity.Epoch > 0
}

// RenewComputerLease extends a still-live allocation. Expired authority is never
// resurrected, even when no replacement has acquired the Computer yet.
func RenewComputerLease(ctx context.Context, pool db.TxBeginner, host workergroup.HostPrincipal, identity ComputerLeaseIdentity) (time.Time, error) {
	if !identity.valid() {
		return time.Time{}, ErrInvalidInput
	}
	var expires time.Time
	err := db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		if err := lockComputerHost(ctx, tx, host); err != nil {
			return err
		}
		var id uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT id FROM computers WHERE environment_id=$1 AND id=$2 FOR NO KEY UPDATE`, identity.EnvironmentID, identity.ComputerID).Scan(&id); err != nil {
			return err
		}
		// The time predicate runs after the ownership lock, including any lock wait.
		return tx.QueryRow(ctx, `UPDATE computer_leases SET expires_at=GREATEST(expires_at,clock_timestamp()+interval '1 minute')
 WHERE environment_id=$1 AND computer_id=$2 AND epoch=$3 AND computer_instance_id=$4 AND worker_host_id=$5 AND worker_epoch=$6
 AND status IN ('active','acquiring','releasing') AND fenced_at IS NULL AND expires_at>clock_timestamp()
 AND NOT EXISTS(SELECT 1 FROM computer_secret_revocations WHERE environment_id=$1 AND computer_id=$2) RETURNING expires_at`, identity.EnvironmentID, identity.ComputerID, identity.Epoch, identity.InstanceID, host.HostID, host.Epoch).Scan(&expires)
	})
	if err != nil {
		return time.Time{}, hideMissing(err)
	}
	return expires, nil
}

// ObserveComputerStopped is called only after the owning worker has joined the
// exact physical VM's closure. Expiry and an epoch change are not this evidence.
// A retained coherent checkpoint keeps its logical processes available to restore.
func ObserveComputerStopped(ctx context.Context, pool db.TxBeginner, host workergroup.HostPrincipal, identity ComputerLeaseIdentity, invalidCheckpoint uuid.UUID) error {
	if !identity.valid() {
		return ErrInvalidInput
	}
	return hideMissing(db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		if err := lockComputerHost(ctx, tx, host); err != nil {
			return err
		}
		return recordComputerStopped(ctx, tx, host.HostID, host.Epoch, identity, "owning worker joined physical VM closure", invalidCheckpoint)
	}))
}

// Expiry rejects new execution and retains the unique writer blocker. Only a
// later physical observation may fence the lease and release its allocation.
func expireComputerLease(ctx context.Context, pool db.TxBeginner, env, computer uuid.UUID, epoch int64) error {
	return hideMissing(db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout='1s'`); err != nil {
			return err
		}
		var host, group uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT l.worker_host_id,h.worker_group_id FROM computer_leases l JOIN worker_hosts h ON h.id=l.worker_host_id WHERE l.environment_id=$1 AND l.computer_id=$2 AND l.epoch=$3`, env, computer, epoch).Scan(&host, &group); err != nil {
			return err
		}
		// Supply changes and worker epoch replacement use the same supply-first order.
		if _, err := tx.Exec(ctx, `SELECT id FROM worker_groups WHERE id=$1 FOR SHARE`, group); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT id FROM worker_hosts WHERE id=$1 FOR SHARE`, host); err != nil {
			return err
		}
		members, err := lockComputerMembers(ctx, tx, env, computer)
		if err != nil {
			return err
		}
		var due bool
		if err = tx.QueryRow(ctx, `SELECT l.fenced_at IS NULL AND l.status IN ('active','acquiring','releasing')
 AND (COALESCE(l.expires_at<=clock_timestamp(),false) OR h.current_epoch IS DISTINCT FROM l.worker_epoch OR h.status NOT IN ('active','draining'))
 FROM computer_leases l JOIN worker_hosts h ON h.id=l.worker_host_id WHERE l.environment_id=$1 AND l.computer_id=$2 AND l.epoch=$3 FOR NO KEY UPDATE OF l`, env, computer, epoch).Scan(&due); err != nil {
			return err
		}
		if !due {
			return nil
		}
		if _, err = tx.Exec(ctx, `UPDATE computer_leases SET status='lost' WHERE environment_id=$1 AND computer_id=$2 AND epoch=$3`, env, computer, epoch); err != nil {
			return err
		}
		if err := loseComputerProcesses(ctx, tx, env, computer, epoch, members, false); err != nil {
			return err
		}
		return settleLostComputerSaves(ctx, tx, env, computer, epoch)
	}))
}

func loseComputerProcesses(ctx context.Context, tx pgx.Tx, env, computer uuid.UUID, epoch int64, members []computerMember, physicalStopped bool) error {
	for _, member := range members {
		if member.LeaseEpoch != epoch {
			continue
		}
		var save uuid.UUID
		err := tx.QueryRow(ctx, `SELECT cp.disk_save_id FROM computer_checkpoints cp
 JOIN computer_checkpoint_members m ON (m.environment_id,m.checkpoint_id)=(cp.environment_id,cp.id)
 WHERE cp.environment_id=$1 AND cp.computer_id=$2 AND cp.status IN ('ready','restoring')
 AND (cp.source_lease_epoch=$3 OR cp.target_lease_epoch=$3) AND m.session_id=$4 AND m.process_epoch=$5`, env, computer, epoch, member.Session, member.Epoch).Scan(&save)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if err == nil {
			retained, err := checkpointDiskEligible(ctx, tx, env, computer, save)
			if err != nil {
				return err
			}
			if retained {
				continue
			}
		}
		e := Execution{EnvironmentID: env, SessionID: member.Session, ProcessEpoch: member.Epoch, LeaseEpoch: epoch}
		if err := loseSessionProcess(ctx, tx, e, physicalStopped); err != nil {
			return err
		}
	}
	return nil
}

// The caller holds all owner/Computer locks. The failure marker makes a later
// physical fence or checkpoint-loss reconciliation safe after explicit hold release.
func loseSessionProcess(ctx context.Context, tx pgx.Tx, e Execution, physicalStopped bool) error {
	var recorded, terminal, fenced, neverStarted bool
	if err := tx.QueryRow(ctx, `SELECT p.failure_recorded_at IS NOT NULL,s.status IN ('closed','cancelled') OR d.execution_revoked_at IS NOT NULL OR EXISTS(SELECT 1 FROM computer_secret_revocations revoked WHERE revoked.environment_id=s.environment_id AND revoked.computer_id=s.computer_id),p.fenced_at IS NOT NULL, p.status IN ('starting','stopping') AND p.attachment_sequence=0 AND p.failure_recorded_at IS NULL AND l.initialized_at IS NULL AND l.restored_from_save_id IS NULL
 FROM session_processes p JOIN computer_leases l ON (l.environment_id,l.computer_id,l.epoch)=(p.environment_id,p.computer_id,p.computer_lease_epoch) JOIN sessions s ON (s.environment_id,s.id)=(p.environment_id,p.session_id)
 JOIN deployments d ON (d.environment_id,d.id)=(s.environment_id,s.deployment_id)
 WHERE p.environment_id=$1 AND p.session_id=$2 AND p.epoch=$3 AND p.computer_lease_epoch=$4 FOR NO KEY UPDATE OF p`, e.EnvironmentID, e.SessionID, e.ProcessEpoch, e.LeaseEpoch).Scan(&recorded, &terminal, &fenced, &neverStarted); err != nil {
		return err
	}
	// Immutable checkpoint members may already have a confirmed physical stop.
	// Loss of the remaining Computer must not rewrite that outcome or its hold.
	if fenced {
		return nil
	}
	revocationRecorded, err := recordImageRevocationFailure(ctx, tx, e)
	if err != nil {
		return err
	}
	// An initial assignment on a never-ready fresh VM cannot have run setup.
	// Expiry retains custody; confirmed absence retires it without a false
	// execution-loss hold. Independent controls and revocations still win.
	if neverStarted && !terminal && !revocationRecorded {
		if physicalStopped {
			_, err = tx.Exec(ctx, `UPDATE session_processes SET status='stopped',fenced_at=clock_timestamp() WHERE environment_id=$1 AND session_id=$2 AND epoch=$3`, e.EnvironmentID, e.SessionID, e.ProcessEpoch)
		}
		return err
	}
	if !recorded && !terminal && !revocationRecorded {
		if err := recordSessionFailure(ctx, tx, e, "Computer execution authority lost without an eligible continuation"); err != nil {
			return err
		}
	}
	if err := interruptRunningSessionTurn(ctx, tx, e); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE session_processes SET status='lost',failure_recorded_at=COALESCE(failure_recorded_at,clock_timestamp()),
 fenced_at=CASE WHEN $4 THEN COALESCE(fenced_at,clock_timestamp()) ELSE fenced_at END
 WHERE environment_id=$1 AND session_id=$2 AND epoch=$3`, e.EnvironmentID, e.SessionID, e.ProcessEpoch, physicalStopped)
	return err
}

// The caller has locked and validated the observer's supply authority. hostEpoch
// identifies the stopped allocation, which may precede the observer after recovery.
func recordComputerStopped(ctx context.Context, tx pgx.Tx, hostID uuid.UUID, hostEpoch int64, identity ComputerLeaseIdentity, evidence string, invalidCheckpoint uuid.UUID) error {
	members, err := lockComputerCheckpointMembers(ctx, tx, identity.EnvironmentID, identity.ComputerID, invalidCheckpoint)
	if err != nil {
		return err
	}
	var fenced bool
	if err = tx.QueryRow(ctx, `SELECT fenced_at IS NOT NULL FROM computer_leases WHERE environment_id=$1 AND computer_id=$2 AND epoch=$3 AND computer_instance_id=$4 AND worker_host_id=$5 AND worker_epoch=$6 FOR NO KEY UPDATE`, identity.EnvironmentID, identity.ComputerID, identity.Epoch, identity.InstanceID, hostID, hostEpoch).Scan(&fenced); err != nil {
		return err
	}
	if fenced {
		return releaseDeletedComputerKey(ctx, tx, identity.EnvironmentID, identity.ComputerID)
	}

	if invalidCheckpoint != uuid.Nil() {
		var bound bool
		if err := tx.QueryRow(ctx, `SELECT COALESCE(cp.status IN ('ready','restoring') AND cp.disk_save_id=l.restored_from_save_id
   AND cp.source_lease_epoch<l.epoch AND (cp.target_lease_epoch IS NULL OR cp.target_lease_epoch<=l.epoch),false)
   FROM computer_checkpoints cp JOIN computer_leases l ON (l.environment_id,l.computer_id)=(cp.environment_id,cp.computer_id)
   WHERE cp.environment_id=$1 AND cp.computer_id=$2 AND cp.id=$3 AND l.epoch=$4 FOR NO KEY UPDATE OF cp`, identity.EnvironmentID, identity.ComputerID, invalidCheckpoint, identity.Epoch).Scan(&bound); err != nil {
			return err
		}
		if !bound {
			return ErrConflict
		}
		if _, err := tx.Exec(ctx, `UPDATE computer_checkpoints SET status='lost',terminal_evidence='owning worker proved checkpoint contents invalid after physical cleanup',capture_request=NULL WHERE environment_id=$1 AND id=$2`, identity.EnvironmentID, invalidCheckpoint); err != nil {
			return err
		}
		for _, member := range members {
			var captured bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM computer_checkpoint_members WHERE environment_id=$1 AND checkpoint_id=$2 AND session_id=$3 AND process_epoch=$4)`, identity.EnvironmentID, invalidCheckpoint, member.Session, member.Epoch).Scan(&captured); err != nil {
				return err
			}
			if !captured {
				continue
			}
			// Before preparation a member still names the physically fenced source;
			// after preparation it names this target. Never substitute the target epoch.
			var stopped bool
			if err := tx.QueryRow(ctx, `SELECT fenced_at IS NOT NULL OR epoch=$3 FROM computer_leases WHERE environment_id=$1 AND computer_id=$2 AND epoch=$4`, identity.EnvironmentID, identity.ComputerID, identity.Epoch, member.LeaseEpoch).Scan(&stopped); err != nil {
				return err
			}
			if !stopped {
				return ErrNotReady
			}
			if err := loseSessionProcess(ctx, tx, Execution{EnvironmentID: identity.EnvironmentID, SessionID: member.Session, ProcessEpoch: member.Epoch, LeaseEpoch: member.LeaseEpoch}, true); err != nil {
				return err
			}
		}
	}
	if err = loseComputerProcesses(ctx, tx, identity.EnvironmentID, identity.ComputerID, identity.Epoch, members, true); err != nil {
		return err
	}
	// Physical closure also settles an unfinished capture owned by this allocation.
	// Disk publication is reconciled under the same Computer lock below.
	if _, err = tx.Exec(ctx, `UPDATE computer_checkpoints SET status='lost',terminal_evidence='physical owner unavailable without an eligible restore',capture_request=NULL
 WHERE environment_id=$1 AND computer_id=$2 AND COALESCE(target_lease_epoch,source_lease_epoch)=$3
 AND status IN ('capturing','sealed','aborting','consumed') AND controls_reconciled_at IS NULL`, identity.EnvironmentID, identity.ComputerID, identity.Epoch); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE computer_leases SET status='lost',fenced_at=clock_timestamp(),fence_evidence=$4 WHERE environment_id=$1 AND computer_id=$2 AND epoch=$3`, identity.EnvironmentID, identity.ComputerID, identity.Epoch, evidence)
	if err != nil {
		return err
	}
	if err := settleLostComputerSaves(ctx, tx, identity.EnvironmentID, identity.ComputerID, identity.Epoch); err != nil {
		return err
	}
	return releaseDeletedComputerKey(ctx, tx, identity.EnvironmentID, identity.ComputerID)
}

// The Computer lock held by the physical-stop owner serializes this release
// with placement. Expired authority alone never releases a deleted key.
func releaseDeletedComputerKey(ctx context.Context, tx pgx.Tx, env, computer uuid.UUID) error {
	_, err := tx.Exec(ctx, `UPDATE computers c SET key=NULL WHERE environment_id=$1 AND id=$2 AND deleted_at IS NOT NULL AND key IS NOT NULL AND NOT EXISTS(SELECT 1 FROM computer_leases l WHERE l.environment_id=c.environment_id AND l.computer_id=c.id AND l.fenced_at IS NULL)`, env, computer)
	return err
}
