package agent

import (
	"context"
	"errors"
	"time"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
)

type RuntimeAuthority struct {
	Generation int64
	ExpiresAt  time.Time
}

// RenewRuntimeAuthority grants a current physical owner a short-lived guest
// control envelope. It is not permission to admit business operations: those
// independently check the current Session lifecycle, holds and revocation.
func RenewRuntimeAuthority(ctx context.Context, pool db.TxBeginner, host workergroup.HostPrincipal, e Execution) (RuntimeAuthority, error) {
	var result RuntimeAuthority
	err := db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		var err error
		result, err = lockRuntimeAuthority(ctx, tx, host, e)
		return err
	})
	if err != nil {
		return RuntimeAuthority{}, hideMissing(err)
	}
	return result, nil
}

// RuntimeAttachment orders replaceable transports for one retained process. A
// lost response may leave a gap; acquiring again must advance rather than reuse
// an attachment that could already have reached the guest.
type RuntimeAttachment struct {
	Stopped   bool
	Starting  bool
	Authority RuntimeAuthority
	Sequence  int64
}

// AcquireRuntimeAttachment authorizes only a current physical owner's control
// transport. It neither starts a process, reruns setup nor releases a hold.
func AcquireRuntimeAttachment(ctx context.Context, pool db.TxBeginner, host workergroup.HostPrincipal, e Execution) (RuntimeAttachment, error) {
	var result RuntimeAttachment
	err := db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		var err error
		result.Authority, err = lockRuntimeProcessAuthority(ctx, tx, host, e, true)
		if err != nil {
			return err
		}
		if err = tx.QueryRow(ctx, `SELECT status='stopped' AND fenced_at IS NOT NULL,status='starting' FROM session_processes
 WHERE environment_id=$1 AND session_id=$2 AND epoch=$3`, e.EnvironmentID, e.SessionID, e.ProcessEpoch).Scan(&result.Stopped, &result.Starting); err != nil {
			return err
		}
		if result.Stopped {
			// This is a physical-close observation, never a grant to reconnect a
			// stopped process. The current host/lease checks still apply.
			result.Authority = RuntimeAuthority{}
			return nil
		}
		if err = tx.QueryRow(ctx, `UPDATE session_processes SET attachment_sequence=attachment_sequence+1
    WHERE environment_id=$1 AND session_id=$2 AND epoch=$3 AND attachment_sequence<9223372036854775807
    RETURNING attachment_sequence`, e.EnvironmentID, e.SessionID, e.ProcessEpoch).Scan(&result.Sequence); err != nil {
			return err
		}
		// Updating the process may wait for a row lock. Recheck physical
		// authority after that wait; rejection rolls back the sequence too.
		result.Authority, err = lockRuntimeAuthority(ctx, tx, host, e)
		return err
	})
	if err != nil {
		return RuntimeAttachment{}, hideMissing(err)
	}
	return result, nil
}

// Both paths use the supply -> ownership-root -> Computer -> Session lock order
// before observing current physical authority and its expiry. Attachment issuance
// then locks the process row and rechecks authority after that final lock.
func lockRuntimeAuthority(ctx context.Context, tx pgx.Tx, host workergroup.HostPrincipal, e Execution) (RuntimeAuthority, error) {
	return lockRuntimeProcessAuthority(ctx, tx, host, e, false)
}

func lockRuntimeProcessAuthority(ctx context.Context, tx pgx.Tx, host workergroup.HostPrincipal, e Execution, allowStopped bool) (RuntimeAuthority, error) {
	caller := Caller{Kind: "session", ID: e.SessionID, Execution: e, Host: &host}
	if err := lockRuntimeHost(ctx, tx, caller); err != nil {
		return RuntimeAuthority{}, err
	}
	owner, err := lockSession(ctx, tx, e.EnvironmentID, e.SessionID)
	if err != nil {
		return RuntimeAuthority{}, err
	}
	// A host can discover the fresh generation after control/revocation; an old
	// model request cannot use this path because worker authentication is required.
	result := RuntimeAuthority{Generation: owner.generation}
	// Acquiring leases receive control only for an exact claimed checkpoint
	// member. Revocation does not prevent delivering its stop control. Business
	// operations independently require an active lease and a live Deployment.
	// A confirmed stopped member is observable only through attachment acquisition;
	// ordinary renewal must not grant its already-closed process new authority.
	err = tx.QueryRow(ctx, `SELECT LEAST(l.expires_at,clock_timestamp()+interval '30 seconds')
 FROM session_processes p JOIN computer_leases l ON (l.environment_id,l.computer_id,l.epoch)=(p.environment_id,p.computer_id,p.computer_lease_epoch)
 JOIN computers c ON (c.environment_id,c.id)=(l.environment_id,l.computer_id)
 WHERE p.environment_id=$1 AND p.session_id=$2 AND p.epoch=$3 AND l.epoch=$4 AND l.worker_host_id=$5 AND l.worker_epoch=$6
 AND l.fenced_at IS NULL AND l.expires_at>clock_timestamp() AND c.integrity_fault_at IS NULL AND c.deleted_at IS NULL
 AND ((l.status='active' AND ((p.status IN ('ready','starting','stopping') AND p.fenced_at IS NULL) OR ($7 AND p.status='stopped' AND p.fenced_at IS NOT NULL)))
 OR (l.status='acquiring' AND (p.status<>'stopped' OR $7) AND EXISTS(SELECT 1 FROM computer_checkpoints cp JOIN computer_checkpoint_members m ON (m.environment_id,m.checkpoint_id)=(cp.environment_id,cp.id)
 WHERE cp.environment_id=l.environment_id AND cp.computer_id=l.computer_id AND cp.target_lease_epoch=l.epoch AND cp.status IN ('restoring','consumed') AND cp.controls_reconciled_at IS NULL
 AND m.session_id=p.session_id AND m.process_epoch=p.epoch)))`, e.EnvironmentID, e.SessionID, e.ProcessEpoch, e.LeaseEpoch, e.WorkerHostID, e.WorkerEpoch, allowStopped).Scan(&result.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return RuntimeAuthority{}, ErrNotReady
	}
	if err != nil {
		return RuntimeAuthority{}, err
	}
	return result, nil
}
