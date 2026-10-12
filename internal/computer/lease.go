package computer

import (
	"context"
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

func (identity ComputerLeaseIdentity) Valid() bool {
	return identity.EnvironmentID != uuid.Nil() && identity.ComputerID != uuid.Nil() && identity.InstanceID != uuid.Nil() && identity.Epoch > 0
}

// RenewComputerLease extends a still-live allocation. Expired authority is never
// resurrected, even when no replacement has acquired the Computer yet.
func RenewComputerLease(ctx context.Context, pool db.TxBeginner, host workergroup.HostPrincipal, identity ComputerLeaseIdentity) (time.Time, error) {
	if !identity.Valid() {
		return time.Time{}, ErrInvalidInput
	}
	var expires time.Time
	err := db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		if err := LockHost(ctx, tx, host); err != nil {
			return err
		}
		var id uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT id FROM computers WHERE environment_id=$1 AND id=$2 FOR NO KEY UPDATE`, identity.EnvironmentID, identity.ComputerID).Scan(&id); err != nil {
			return err
		}
		// The time predicate runs after the ownership lock, including any lock wait.
		return tx.QueryRow(ctx, `UPDATE computer_leases SET expires_at=GREATEST(expires_at,clock_timestamp()+interval '1 minute')
 WHERE environment_id=$1 AND computer_id=$2 AND epoch=$3 AND allocation_id=$4 AND worker_host_id=$5 AND worker_epoch=$6
 AND status IN ('active','acquiring','releasing') AND fenced_at IS NULL AND expires_at>clock_timestamp()
 AND NOT EXISTS(SELECT 1 FROM (`+db.RevokedComputerSecretsSQL+`) revoked_secrets WHERE environment_id=$1 AND computer_id=$2) RETURNING expires_at`, identity.EnvironmentID, identity.ComputerID, identity.Epoch, identity.InstanceID, host.HostID, host.Epoch).Scan(&expires)
	})
	if err != nil {
		return time.Time{}, hideMissing(err)
	}
	return expires, nil
}

type LeaseAuthority struct {
	Instance         uuid.UUID
	CredentialDigest []byte
	BaseVersion      string
	Expires          time.Time
}

func CurrentLease(ctx context.Context, tx pgx.Tx, host workergroup.HostPrincipal, env, computer uuid.UUID, epoch int64) (LeaseAuthority, error) {
	var lease LeaseAuthority
	err := tx.QueryRow(ctx, `SELECT l.allocation_id,l.channel_credential_digest,COALESCE(l.restored_from_save_id::text,'sha256:'||encode(c.initial_root_digest,'hex')),LEAST(l.expires_at,clock_timestamp()+interval '30 seconds')
  FROM computer_leases l JOIN computers c ON (c.environment_id,c.id)=(l.environment_id,l.computer_id)
  WHERE l.environment_id=$1 AND l.computer_id=$2 AND l.epoch=$3 AND l.worker_host_id=$4 AND l.worker_epoch=$5
   AND l.status='active' AND l.fenced_at IS NULL AND l.expires_at>clock_timestamp()`, env, computer, epoch, host.HostID, host.Epoch).Scan(&lease.Instance, &lease.CredentialDigest, &lease.BaseVersion, &lease.Expires)
	return lease, err
}

func DispatchAvailable(ctx context.Context, tx pgx.Tx, env, computer uuid.UUID) error {
	var sealed bool
	err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM (`+db.RevokedComputerSecretsSQL+`) revoked_secrets WHERE environment_id=$1 AND computer_id=$2) OR EXISTS(SELECT 1 FROM checkpoints WHERE environment_id=$1 AND computer_id=$2 AND (status IN ('capturing','sealed','ready','restoring','aborting') OR (status='consumed' AND controls_reconciled_at IS NULL)))`, env, computer).Scan(&sealed)
	if err != nil {
		return err
	}
	if sealed {
		return ErrNotReady
	}
	return nil
}

// The Computer lock held by the physical-stop owner serializes this release
// with placement. Expired authority alone never releases a deleted key.
func ReleaseDeletedKey(ctx context.Context, tx pgx.Tx, env, computerID uuid.UUID) error {
	_, err := tx.Exec(ctx, `UPDATE computers c SET key=NULL WHERE environment_id=$1 AND id=$2 AND deleted_at IS NOT NULL AND key IS NOT NULL AND NOT EXISTS(SELECT 1 FROM computer_leases l WHERE l.environment_id=c.environment_id AND l.computer_id=c.id AND l.fenced_at IS NULL)`, env, computerID)
	return err
}
