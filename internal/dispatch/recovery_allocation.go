package dispatch

import (
	"context"
	"errors"
	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/org"
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
	"uuid"
)

// AllocateRecoveredLease reconstructs processes on the retained published disk
// head. It never replays completed or interrupted handlers, runs setup on a
// discarded image, or substitutes disk recovery for a valid coherent checkpoint.
func (a *Allocator) AllocateRecoveredLease(ctx context.Context, env, computerID uuid.UUID, epoch int64, host uuid.UUID) (computer.ComputerAllocation, error) {
	if env == uuid.Nil() || computerID == uuid.Nil() || host == uuid.Nil() || epoch <= 1 {
		return computer.ComputerAllocation{}, ErrInvalidInput
	}
	var result computer.ComputerAllocation
	err := db.RunTx(ctx, a.database, func(tx pgx.Tx) error {
		if err := allocationLockTimeout(ctx, tx); err != nil {
			return err
		}
		supply, err := lockAllocationSupply(ctx, tx, env, host, true)
		if err != nil {
			return err
		}
		_, err = org.LockAdmission(ctx, tx, env)
		if err != nil {
			return err
		}
		before, err := workergroup.ReadExecutionPlacement(ctx, tx, env)
		if err != nil {
			return err
		}
		sessions, err := session.LockInitialSessions(ctx, tx, env, computerID)
		if err != nil {
			return err
		}
		if prior, err := ReadLeaseAllocation(ctx, tx, env, computerID, epoch); err == nil {
			if !prior.DiskOnlyRecovery {
				return ErrConflict
			}
			result = prior
			return nil
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		allowed, err := initialSessionDemand(ctx, a.auditPolicy, tx, env, computerID, sessions)
		if err != nil {
			return err
		}
		if !allowed {
			return nil
		} // Commit revoked input settlement without reserving a VM.
		var save *uuid.UUID
		var base string
		var cpu, memory int64
		eligible, err := computer.DiskRecoveryEligible(ctx, tx, env, computerID)
		if err != nil {
			return err
		}
		if !eligible {
			return ErrNotReady
		}
		if err = tx.QueryRow(ctx, `SELECT s.id,COALESCE(s.id::text,'sha256:'||encode(c.initial_root_digest,'hex')),c.cpu_millis,c.memory_bytes FROM computers c LEFT JOIN computer_saves s ON (s.environment_id,s.id)=(c.environment_id,c.recovery_save_id)
 WHERE c.environment_id=$1 AND c.id=$2
 AND NOT EXISTS(SELECT 1 FROM computer_leases l WHERE l.environment_id=c.environment_id AND l.computer_id=c.id AND l.fenced_at IS NULL)
 AND (SELECT max(l.epoch) FROM computer_leases l WHERE l.environment_id=c.environment_id AND l.computer_id=c.id)=$3::bigint-1
 AND NOT EXISTS(SELECT 1 FROM computer_saves pending WHERE pending.environment_id=c.environment_id AND pending.computer_id=c.id AND pending.status IN ('requested','captured'))
 AND NOT EXISTS(SELECT 1 FROM checkpoints cp WHERE cp.environment_id=c.environment_id AND cp.computer_id=c.id AND (cp.status IN ('capturing','sealed','ready','restoring','aborting') OR (cp.status='consumed' AND cp.controls_reconciled_at IS NULL)))
 AND (EXISTS(SELECT 1 FROM sessions s WHERE s.environment_id=c.environment_id AND s.computer_id=c.id AND s.status IN ('open','closing') AND `+db.NoSessionHoldsSQL+` AND EXISTS(SELECT 1 FROM turns t WHERE t.environment_id=s.environment_id AND t.session_id=s.id AND t.status='queued')) OR EXISTS(SELECT 1 FROM commands cmd WHERE cmd.environment_id=c.environment_id AND cmd.computer_id=c.id AND cmd.status='pending'))`, env, computerID, epoch).Scan(&save, &base, &cpu, &memory); err != nil {
			return err
		}
		shape, err := qualifyAllocationShape(ctx, tx, supply, cpu, memory)
		if err != nil {
			return err
		}
		if err = requireAllocationCapacity(ctx, tx, env, supply, shape, computerID, uuid.Nil()); err != nil {
			return err
		}
		result = computer.ComputerAllocation{Allocation: computer.Allocation{EnvironmentID: env, OwnerID: computerID, InstanceID: uuid.NewV7(), Epoch: epoch, HostID: host, HostEpoch: supply.epoch, Shape: shape, Status: "acquiring"}, BaseVersion: base, RestoredFrom: save, DiskOnlyRecovery: true}
		if err = a.insertComputerAllocation(ctx, tx, &result); err != nil {
			return err
		}
		if err = session.AssignInitialProcesses(ctx, a.auditPolicy, tx, result, sessions); err != nil {
			return err
		}
		if err = requireAllocationFreshSupply(ctx, tx, supply); err != nil {
			return err
		}
		if err := computer.RequireSecretsNotRevoked(ctx, tx, env, computerID); err != nil {
			return err
		}
		return workergroup.RequireExecutionPlacement(ctx, tx, env, uuid.Nil(), before)
	})
	if err != nil {
		return computer.ComputerAllocation{}, allocationError(err)
	}
	if result.InstanceID == uuid.Nil() {
		return computer.ComputerAllocation{}, ErrNotReady
	}
	return result, nil
}
