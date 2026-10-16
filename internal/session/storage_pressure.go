package session

import (
	"context"
	"encoding/json"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
)

// ReportStoragePressure retains the first server observation for this physical
// writer. Worker wall time is diagnostic only; retries cannot reset the budget.
func ReportStoragePressure(ctx context.Context, database db.TxBeginner, host workergroup.HostPrincipal, identity computer.LeaseIdentity, observedSince *time.Time) error {
	if !identity.Valid() {
		return ErrInvalidInput
	}
	return hideMissing(db.RunTx(ctx, database, func(tx pgx.Tx) error {
		if err := computer.LockHost(ctx, tx, host); err != nil {
			return err
		}
		if _, err := LockComputerMembers(ctx, tx, identity.EnvironmentID, identity.ComputerID); err != nil {
			return err
		}
		var valid bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM computer_leases WHERE environment_id=$1 AND computer_id=$2 AND epoch=$3 AND allocation_id=$4 AND worker_host_id=$5 AND worker_epoch=$6 AND status IN ('active','releasing') AND fenced_at IS NULL AND expires_at>clock_timestamp())`, identity.EnvironmentID, identity.ComputerID, identity.Epoch, identity.InstanceID, host.HostID, host.Epoch).Scan(&valid); err != nil {
			return err
		}
		if !valid {
			return computer.ErrDenied
		}
		if err := computer.RecordLeaseStoragePressure(ctx, tx, identity.EnvironmentID, identity.ComputerID, identity.Epoch, observedSince); err != nil {
			return err
		}
		return reconcileStoragePressureTx(ctx, tx, identity.EnvironmentID, identity.ComputerID, identity.Epoch)
	}))
}

func reconcileSessionStoragePressure(ctx context.Context, database db.TxBeginner, env, id uuid.UUID) error {
	return db.RunTx(ctx, database, func(tx pgx.Tx) error {
		var computerID, host, group uuid.UUID
		var epoch int64
		if err := tx.QueryRow(ctx, `SELECT s.computer_id,l.epoch,l.worker_host_id,h.worker_group_id FROM sessions s JOIN computer_leases l ON (l.environment_id,l.computer_id)=(s.environment_id,s.computer_id) JOIN worker_hosts h ON h.id=l.worker_host_id WHERE s.environment_id=$1 AND s.id=$2 AND l.storage_pressure_since IS NOT NULL ORDER BY l.epoch DESC LIMIT 1`, env, id).Scan(&computerID, &epoch, &host, &group); err != nil {
			if err == pgx.ErrNoRows {
				return nil
			}
			return err
		}
		// Keep the supply-first order shared with lease expiry and host replacement.
		if _, err := tx.Exec(ctx, `SELECT id FROM worker_groups WHERE id=$1 FOR SHARE`, group); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT id FROM worker_hosts WHERE id=$1 FOR SHARE`, host); err != nil {
			return err
		}
		if _, err := LockComputerMembers(ctx, tx, env, computerID); err != nil {
			return err
		}
		return reconcileStoragePressureTx(ctx, tx, env, computerID, epoch)
	})
}

// The caller holds the supply gates, all open Session ownership roots and the Computer lock.
func reconcileStoragePressureTx(ctx context.Context, tx pgx.Tx, env, computerID uuid.UUID, epoch int64) error {
	var pressured, live, prolonged, exhausted bool
	if err := tx.QueryRow(ctx, `SELECT l.storage_pressure_since IS NOT NULL,
 l.status IN ('active','releasing') AND l.fenced_at IS NULL AND l.expires_at>clock_timestamp()
 AND COALESCE(h.current_epoch=l.worker_epoch,false) AND h.status IN ('active','draining') AND g.status IN ('active','paused','draining'),
 COALESCE(l.storage_pressure_since<=clock_timestamp()-interval '30 seconds',false),
 COALESCE(l.storage_pressure_since<=clock_timestamp()-interval '5 minutes',false)
 FROM computer_leases l JOIN worker_hosts h ON h.id=l.worker_host_id JOIN worker_groups g ON g.id=h.worker_group_id
 WHERE l.environment_id=$1 AND l.computer_id=$2 AND l.epoch=$3`, env, computerID, epoch).Scan(&pressured, &live, &prolonged, &exhausted); err != nil {
		return err
	}
	// Lease loss owns terminal outcomes once execution authority is gone.
	// An explicit pressure clear may still release its own holds.
	if pressured && !live {
		return nil
	}
	rows, err := tx.Query(ctx, `SELECT id FROM sessions WHERE environment_id=$1 AND computer_id=$2 AND status IN ('open','closing') ORDER BY id`, env, computerID)
	if err != nil {
		return err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return err
	}
	for _, id := range ids {
		var hold *uuid.UUID
		if err = tx.QueryRow(ctx, `SELECT (SELECT id FROM session_holds WHERE environment_id=$1 AND session_id=$2 AND kind='storage_pressure' AND released_at IS NULL)`, env, id).Scan(&hold); err != nil {
			return err
		}
		state := ""
		if prolonged && hold == nil {
			value := uuid.NewV7()
			hold = &value
			if _, err = tx.Exec(ctx, `INSERT INTO session_holds(environment_id,id,session_id,scope,reason,kind) VALUES($1,$2,$3,'local','Waiting for saving to recover','storage_pressure')`, env, *hold, id); err != nil {
				return err
			}
			state = "paused"
		} else if !prolonged && hold != nil {
			if _, err = tx.Exec(ctx, `UPDATE session_holds SET released_at=clock_timestamp() WHERE environment_id=$1 AND id=$2`, env, *hold); err != nil {
				return err
			}
			state = "resolved"
		}
		if state != "" {
			if err = advanceControlAuthority(ctx, tx, env, []uuid.UUID{id}); err != nil {
				return err
			}
			// Loss settlement releases its pressure hold but cannot promise
			// that storage or execution has recovered.
			if state != "resolved" || live {
				if err = storagePressureEvent(ctx, tx, env, id, computerID, epoch, *hold, state, uuid.Nil()); err != nil {
					return err
				}
			}
		}
		if exhausted {
			var turn *uuid.UUID
			var process *int64
			if err = tx.QueryRow(ctx, `SELECT (SELECT id FROM turns WHERE environment_id=$1 AND session_id=$2 AND status='running'),(SELECT process_epoch FROM turns WHERE environment_id=$1 AND session_id=$2 AND status='running')`, env, id).Scan(&turn, &process); err != nil {
				return err
			}
			if turn != nil && process != nil {
				if err = interruptControlledTurn(ctx, tx, env, id, *turn, *process); err != nil {
					return err
				}
				if err = advanceControlAuthority(ctx, tx, env, []uuid.UUID{id}); err != nil {
					return err
				}
				if _, err = tx.Exec(ctx, `UPDATE session_holds SET reason=$3 WHERE environment_id=$1 AND id=$2 AND released_at IS NULL`, env, *hold, "The request was stopped because storage stayed full. Saving is still retrying. New requests will wait until space is available."); err != nil {
					return err
				}
				if err = storagePressureEvent(ctx, tx, env, id, computerID, epoch, *hold, "stopped", *turn); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
func storagePressureEvent(ctx context.Context, tx pgx.Tx, env, id, computerID uuid.UUID, epoch int64, hold uuid.UUID, state string, turn uuid.UUID) error {
	data, err := json.Marshal(struct {
		ComputerID uuid.UUID `json:"computerId"`
		LeaseEpoch int64     `json:"leaseEpoch"`
		HoldID     uuid.UUID `json:"holdId"`
		State      string    `json:"state"`
	}{computerID, epoch, hold, state})
	if err != nil {
		return err
	}
	return eventData(ctx, tx, env, id, turn, "session.storage_pressure", data)
}
