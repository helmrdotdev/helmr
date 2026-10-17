package dispatch

import (
	"context"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/session"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
)

// ObserveLeaseStopped is called only after the owning worker has joined the
// exact physical VM's closure. Expiry and an epoch change are not this evidence.
// A retained coherent checkpoint keeps its logical processes available to restore.
func ObserveLeaseStopped(ctx context.Context, pool db.TxBeginner, host workergroup.HostPrincipal, identity computer.LeaseIdentity, invalidCheckpoint uuid.UUID) error {
	if !identity.Valid() {
		return ErrInvalidInput
	}
	return hideMissing(db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		if err := computer.LockHost(ctx, tx, host); err != nil {
			return err
		}
		return recordComputerStopped(ctx, tx, host.HostID, host.Epoch, identity, "owning worker joined physical VM closure", invalidCheckpoint)
	}))
}

// Expiry rejects new execution and retains the unique writer blocker. Only a
// later physical observation may fence the lease and release its allocation.
func ExpireLease(ctx context.Context, pool db.TxBeginner, env, computerID uuid.UUID, epoch int64) error {
	return hideMissing(db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout='1s'`); err != nil {
			return err
		}
		var host, group uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT l.worker_host_id,h.worker_group_id FROM computer_leases l JOIN worker_hosts h ON h.id=l.worker_host_id WHERE l.environment_id=$1 AND l.computer_id=$2 AND l.epoch=$3`, env, computerID, epoch).Scan(&host, &group); err != nil {
			return err
		}
		// Supply changes and worker epoch replacement use the same supply-first order.
		if _, err := tx.Exec(ctx, `SELECT id FROM worker_groups WHERE id=$1 FOR SHARE`, group); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT id FROM worker_hosts WHERE id=$1 FOR SHARE`, host); err != nil {
			return err
		}
		members, err := session.LockComputerMembers(ctx, tx, env, computerID)
		if err != nil {
			return err
		}
		var due bool
		if err = tx.QueryRow(ctx, `SELECT l.fenced_at IS NULL AND l.status IN ('active','acquiring','releasing')
 AND (COALESCE(l.expires_at<=clock_timestamp(),false) OR h.current_epoch IS DISTINCT FROM l.worker_epoch OR h.status NOT IN ('active','draining'))
 FROM computer_leases l JOIN worker_hosts h ON h.id=l.worker_host_id WHERE l.environment_id=$1 AND l.computer_id=$2 AND l.epoch=$3 FOR NO KEY UPDATE OF l`, env, computerID, epoch).Scan(&due); err != nil {
			return err
		}
		if !due {
			return nil
		}
		if err = computer.MarkLeaseLost(ctx, tx, env, computerID, epoch); err != nil {
			return err
		}
		if err = computer.SettleLostSaves(ctx, tx, env, computerID, epoch); err != nil {
			return err
		}
		if err = session.RecordComputerRecoveryGap(ctx, tx, env, computerID, epoch); err != nil {
			return err
		}
		eligible, err := computer.DiskRecoveryEligible(ctx, tx, env, computerID)
		if err != nil {
			return err
		}
		disposition := session.ComputerLossRequiresAttention
		if eligible {
			disposition = session.ComputerLossRecoverable
		}
		if err := session.LoseLeaseProcesses(ctx, tx, env, computerID, epoch, members, false, disposition); err != nil {
			return err
		}
		return nil
	}))
}

// The caller has locked and validated the observer's supply authority. hostEpoch
// identifies the stopped allocation, which may precede the observer after recovery.
func recordComputerStopped(ctx context.Context, tx pgx.Tx, hostID uuid.UUID, hostEpoch int64, identity computer.LeaseIdentity, evidence string, invalidCheckpoint uuid.UUID) error {
	members, err := session.LockCheckpointMembers(ctx, tx, identity.EnvironmentID, identity.ComputerID, invalidCheckpoint)
	if err != nil {
		return err
	}
	var fenced bool
	if err = tx.QueryRow(ctx, `SELECT fenced_at IS NOT NULL FROM computer_leases WHERE environment_id=$1 AND computer_id=$2 AND epoch=$3 AND allocation_id=$4 AND worker_host_id=$5 AND worker_epoch=$6 FOR NO KEY UPDATE`, identity.EnvironmentID, identity.ComputerID, identity.Epoch, identity.InstanceID, hostID, hostEpoch).Scan(&fenced); err != nil {
		return err
	}
	if fenced {
		return computer.ReleaseDeletedKey(ctx, tx, identity.EnvironmentID, identity.ComputerID)
	}

	if invalidCheckpoint != uuid.Nil() {
		var bound bool
		if err := tx.QueryRow(ctx, `SELECT COALESCE(cp.status IN ('ready','restoring') AND cp.disk_save_id=l.restored_from_save_id
   AND cp.source_lease_epoch<l.epoch AND (cp.target_lease_epoch IS NULL OR cp.target_lease_epoch<=l.epoch),false)
   FROM checkpoints cp JOIN computer_leases l ON (l.environment_id,l.computer_id)=(cp.environment_id,cp.computer_id)
   WHERE cp.environment_id=$1 AND cp.computer_id=$2 AND cp.id=$3 AND l.epoch=$4 FOR NO KEY UPDATE OF cp`, identity.EnvironmentID, identity.ComputerID, invalidCheckpoint, identity.Epoch).Scan(&bound); err != nil {
			return err
		}
		if !bound {
			return ErrConflict
		}
		if err := computer.LoseInvalidCheckpoint(ctx, tx, identity.EnvironmentID, invalidCheckpoint); err != nil {
			return err
		}
	}
	if err = computer.FenceLease(ctx, tx, identity.EnvironmentID, identity.ComputerID, identity.Epoch, evidence); err != nil {
		return err
	}
	if err = computer.SettleLostSaves(ctx, tx, identity.EnvironmentID, identity.ComputerID, identity.Epoch); err != nil {
		return err
	}
	if err = session.RecordComputerRecoveryGap(ctx, tx, identity.EnvironmentID, identity.ComputerID, identity.Epoch); err != nil {
		return err
	}
	eligible, err := computer.DiskRecoveryEligible(ctx, tx, identity.EnvironmentID, identity.ComputerID)
	if err != nil {
		return err
	}
	disposition := session.ComputerLossRequiresAttention
	if eligible {
		disposition = session.ComputerLossRecoverable
	}
	if invalidCheckpoint != uuid.Nil() {
		for _, member := range members {
			if member.LeaseEpoch == identity.Epoch {
				continue // The target epoch is settled below with its ordinary members.
			}
			var captured bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM checkpoint_processes WHERE environment_id=$1 AND checkpoint_id=$2 AND session_id=$3 AND process_epoch=$4)`, identity.EnvironmentID, invalidCheckpoint, member.Session, member.Epoch).Scan(&captured); err != nil {
				return err
			}
			if !captured {
				continue
			}
			// A checkpoint member may still name a previous source/target epoch.
			// Its physical closure must be independently established.
			var stopped bool
			if err := tx.QueryRow(ctx, `SELECT fenced_at IS NOT NULL FROM computer_leases WHERE environment_id=$1 AND computer_id=$2 AND epoch=$3`, identity.EnvironmentID, identity.ComputerID, member.LeaseEpoch).Scan(&stopped); err != nil {
				return err
			}
			if !stopped {
				return ErrNotReady
			}
			if err := session.LoseLeaseProcesses(ctx, tx, identity.EnvironmentID, identity.ComputerID, member.LeaseEpoch, []session.ComputerMember{member}, true, disposition); err != nil {
				return err
			}
		}
	}
	if err = session.LoseLeaseProcesses(ctx, tx, identity.EnvironmentID, identity.ComputerID, identity.Epoch, members, true, disposition); err != nil {
		return err
	}
	if err = computer.LoseLeaseCheckpoints(ctx, tx, identity.EnvironmentID, identity.ComputerID, identity.Epoch); err != nil {
		return err
	}
	return computer.ReleaseDeletedKey(ctx, tx, identity.EnvironmentID, identity.ComputerID)
}
