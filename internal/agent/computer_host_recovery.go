package agent

import (
	"context"
	"errors"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ObserveRecoveredHostComputers consumes the current worker epoch's completed
// local VM inventory and physical cleanup. Quarantined instances remain unknown.
// Epoch advancement alone does not authorize this operation. Each Computer is
// settled in its own ownership transaction, so a retry can finish partial progress.
func ObserveRecoveredHostComputers(ctx context.Context, database db.TxDB, host workergroup.HostPrincipal, quarantined []uuid.UUID) error {
	if quarantined == nil {
		return ErrInvalidInput
	}
	type stoppedAllocation struct {
		identity  ComputerLeaseIdentity
		hostEpoch int64
	}
	var allocations []stoppedAllocation
	err := db.RunTx(ctx, database, func(tx pgx.Tx) error {
		if err := lockRecoveringComputerHost(ctx, tx, host); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT environment_id,computer_id,computer_instance_id,epoch,worker_epoch FROM computer_leases
 WHERE worker_host_id=$1 AND worker_epoch<$2 AND fenced_at IS NULL AND NOT (computer_instance_id=ANY($3::uuid[]))
 ORDER BY environment_id,computer_id,epoch`, host.HostID, host.Epoch, quarantined)
		if err != nil {
			return err
		}
		allocations, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (stoppedAllocation, error) {
			var a stoppedAllocation
			err := row.Scan(&a.identity.EnvironmentID, &a.identity.ComputerID, &a.identity.InstanceID, &a.identity.Epoch, &a.hostEpoch)
			return a, err
		})
		return err
	})
	if err != nil {
		return hideMissing(err)
	}
	var pending bool
	for _, a := range allocations {
		hostLocked := false
		if err := db.RunTx(ctx, database, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout='1s'`); err != nil {
				return err
			}
			if err := lockRecoveringComputerHost(ctx, tx, host); err != nil {
				return err
			}
			hostLocked = true
			return recordComputerStopped(ctx, tx, host.HostID, a.hostEpoch, a.identity, "successor worker completed local VM inventory and physical cleanup", uuid.Nil())
		}); err != nil {
			var lock *pgconn.PgError
			if errors.Is(err, ErrNotReady) || (errors.As(err, &lock) && lock.Code == "55P03") {
				if !hostLocked {
					return ErrNotReady
				}
				pending = true
				continue
			}
			return hideMissing(err)
		}
	}
	if err := observeRecoveredHostPreparations(ctx, database, host, quarantined); err != nil {
		if errors.Is(err, ErrNotReady) {
			pending = true
		} else {
			return hideMissing(err)
		}
	}
	if pending {
		return ErrNotReady
	}
	return nil
}

func lockRecoveringComputerHost(ctx context.Context, tx pgx.Tx, host workergroup.HostPrincipal) error {
	var groupClaim, hostClaim int64
	var epoch *int64
	var groupStatus string
	var recovering bool
	if err := tx.QueryRow(ctx, `SELECT claim_version,status FROM worker_groups WHERE id=$1 FOR SHARE`, host.GroupID).Scan(&groupClaim, &groupStatus); err != nil {
		return err
	}
	if err := tx.QueryRow(ctx, `SELECT claim_version,current_epoch,status='registering' OR (status='draining' AND vm_platform_id IS NULL)
 FROM worker_hosts WHERE id=$1 AND worker_group_id=$2 FOR SHARE`, host.HostID, host.GroupID).Scan(&hostClaim, &epoch, &recovering); err != nil {
		return err
	}
	if groupClaim != host.GroupClaimVersion || hostClaim != host.HostClaimVersion {
		return workergroup.ErrStaleClaims
	}
	if epoch == nil || *epoch != host.Epoch || !recovering || (groupStatus != "active" && groupStatus != "paused" && groupStatus != "draining") {
		return ErrDenied
	}
	return nil
}
