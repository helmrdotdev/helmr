package agent

import (
	"context"
	"encoding/json"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
)

// BindComputerLeaseDisk retains the admitted lease's exact mounted source and
// Computer-owned write key. Key rotation belongs to lease acquisition; this
// binding does not require a distinct key for every epoch. It does not acquire a lease, select a recovery point or
// permit execution. Acquisition must already have resolved all pending saves
// before recording restored_from_save_id.
func BindComputerLeaseDisk(ctx context.Context, pool db.TxBeginner, host workergroup.HostPrincipal, env, computer uuid.UUID, epoch int64, writeKey uuid.UUID) (disk.VersionRoot, error) {
	var result disk.VersionRoot
	if env == uuid.Nil() || computer == uuid.Nil() || epoch <= 0 || writeKey == uuid.Nil() {
		return result, ErrInvalidInput
	}
	err := db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		if err := lockComputerHost(ctx, tx, host); err != nil {
			return err
		}
		var initial string
		if err := tx.QueryRow(ctx, `SELECT 'sha256:'||encode(initial_root_digest,'hex') FROM computers WHERE environment_id=$1 AND id=$2
 AND NOT EXISTS(SELECT 1 FROM computer_secret_revocations WHERE environment_id=$1 AND computer_id=$2) FOR NO KEY UPDATE`, env, computer).Scan(&initial); err != nil {
			return err
		}
		var restored *uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT restored_from_save_id FROM computer_leases WHERE environment_id=$1 AND computer_id=$2 AND epoch=$3 AND worker_host_id=$4 AND worker_epoch=$5 AND status IN ('acquiring','active','releasing') AND fenced_at IS NULL AND expires_at>clock_timestamp()`, env, computer, epoch, host.HostID, host.Epoch).Scan(&restored); err != nil {
			return err
		}
		var rootID uuid.UUID
		var raw []byte
		if restored == nil {
			if err := tx.QueryRow(ctx, `SELECT r.id,r.locator FROM computers c JOIN computer_disk_roots r ON (r.environment_id,r.id)=(c.environment_id,c.initial_root_id) WHERE c.environment_id=$1 AND c.id=$2`, env, computer).Scan(&rootID, &raw); err != nil {
				return err
			}
		} else {
			if err := tx.QueryRow(ctx, `SELECT r.id,r.locator FROM computer_saves v JOIN computer_disk_roots r ON (r.environment_id,r.id)=(v.environment_id,v.root_id) WHERE v.environment_id=$1 AND v.computer_id=$2 AND v.id=$3`, env, computer, *restored).Scan(&rootID, &raw); err != nil {
				return err
			}
		}
		if err := json.Unmarshal(raw, &result); err != nil {
			return err
		}
		identity, err := result.Digest()
		if err != nil {
			return err
		}
		if restored == nil && identity != initial {
			return ErrConflict
		}
		var authorizedKey uuid.UUID
		if err = tx.QueryRow(ctx, `SELECT id FROM computer_data_keys WHERE environment_id=$1 AND writer_computer_id=$2 AND id=$3 AND available FOR KEY SHARE`, env, computer, writeKey).Scan(&authorizedKey); err != nil {
			return hideMissing(err)
		}
		if _, err = tx.Exec(ctx, `UPDATE computer_leases SET base_root_id=$4,write_key_id=$5 WHERE environment_id=$1 AND computer_id=$2 AND epoch=$3 AND base_root_id IS NULL AND disk_released_at IS NULL`, env, computer, epoch, rootID, writeKey); err != nil {
			return err
		}
		var priorRoot, priorKey uuid.UUID
		if err = tx.QueryRow(ctx, `SELECT base_root_id,write_key_id FROM computer_leases WHERE environment_id=$1 AND computer_id=$2 AND epoch=$3 AND base_root_id IS NOT NULL AND disk_released_at IS NULL`, env, computer, epoch).Scan(&priorRoot, &priorKey); err != nil {
			return err
		}
		if priorRoot != rootID || priorKey != writeKey {
			return ErrConflict
		}
		// Key/root FK acquisition may wait. A binding cannot outlive the authority
		// that admitted it merely because the first lookup preceded a lock wait.
		var valid bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM computer_leases WHERE environment_id=$1 AND computer_id=$2 AND epoch=$3 AND fenced_at IS NULL AND status IN ('acquiring','active','releasing') AND expires_at>clock_timestamp()) AND NOT EXISTS(SELECT 1 FROM computer_secret_revocations WHERE environment_id=$1 AND computer_id=$2)`, env, computer, epoch).Scan(&valid); err != nil {
			return err
		}
		if !valid {
			return ErrDenied
		}
		return nil
	})
	if err != nil {
		return disk.VersionRoot{}, hideMissing(err)
	}
	return result, nil
}

// ReleaseComputerLeaseDisk drops availability pins only after physical closure
// and all source publication/continuation obligations have settled. Expiry or a
// logical lost status alone cannot authorize collecting a writer's dependencies.
func ReleaseComputerLeaseDisk(ctx context.Context, pool db.TxBeginner, env, computer uuid.UUID, epoch int64) error {
	if env == uuid.Nil() || computer == uuid.Nil() || epoch <= 0 {
		return ErrInvalidInput
	}
	return hideMissing(db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		return releaseComputerLeaseDisk(ctx, tx, env, computer, epoch)
	}))
}

func releaseComputerLeaseDisk(ctx context.Context, tx pgx.Tx, env, computer uuid.UUID, epoch int64) error {
	var id uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT id FROM computers WHERE environment_id=$1 AND id=$2 FOR NO KEY UPDATE`, env, computer).Scan(&id); err != nil {
		return err
	}
	var fenced, bound, released bool
	if err := tx.QueryRow(ctx, `SELECT fenced_at IS NOT NULL,base_root_id IS NOT NULL,disk_released_at IS NOT NULL FROM computer_leases WHERE environment_id=$1 AND computer_id=$2 AND epoch=$3 FOR NO KEY UPDATE`, env, computer, epoch).Scan(&fenced, &bound, &released); err != nil {
		return err
	}
	if released || !bound {
		return nil
	}
	if !fenced {
		return ErrNotReady
	}
	var pending bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM computer_saves WHERE environment_id=$1 AND computer_id=$2 AND computer_lease_epoch=$3 AND status IN ('requested','captured'))
 OR EXISTS(SELECT 1 FROM computer_checkpoints WHERE environment_id=$1 AND computer_id=$2 AND (source_lease_epoch=$3 OR target_lease_epoch=$3) AND status NOT IN ('cancelled','lost') AND controls_reconciled_at IS NULL)`, env, computer, epoch).Scan(&pending); err != nil {
		return err
	}
	if pending {
		return ErrNotReady
	}
	_, err := tx.Exec(ctx, `UPDATE computer_leases SET disk_released_at=clock_timestamp() WHERE environment_id=$1 AND computer_id=$2 AND epoch=$3`, env, computer, epoch)
	return err
}
