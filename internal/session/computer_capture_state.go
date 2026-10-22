package session

import (
	"context"
	"slices"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/computer"

	"github.com/helmrdotdev/helmr/internal/db"
	agentv1 "github.com/helmrdotdev/helmr/internal/proto/agent/v1"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
)

type CaptureRecord struct {
	Computer           uuid.UUID
	SourceEpoch        int64
	TargetEpoch        *int64
	Version            int64
	Save               uuid.UUID
	State              string
	ControlsReconciled bool
	Expires            time.Time
	Members            []ComputerMember
}

// The caller takes supply locks before this function. Captured members remain
// lockable after loss/fencing; their immutable membership owns recovery holds.
func LockCapture(ctx context.Context, tx pgx.Tx, env, checkpoint uuid.UUID) (CaptureRecord, error) {
	var r CaptureRecord
	if err := tx.QueryRow(ctx, `SELECT computer_id FROM checkpoints WHERE environment_id=$1 AND id=$2`, env, checkpoint).Scan(&r.Computer); err != nil {
		return r, err
	}
	rows, err := tx.Query(ctx, `SELECT session_id,process_epoch FROM checkpoint_processes WHERE environment_id=$1 AND checkpoint_id=$2 ORDER BY session_id`, env, checkpoint)
	if err != nil {
		return r, err
	}
	var ids []uuid.UUID
	for rows.Next() {
		var m ComputerMember
		if err := rows.Scan(&m.Session, &m.Epoch); err != nil {
			rows.Close()
			return r, err
		}
		r.Members = append(r.Members, m)
		ids = append(ids, m.Session)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return r, err
	}
	if len(ids) == 0 {
		// Standalone Computers have no Session ownership roots, but still need
		// the Computer fence before locking their checkpoint.
		err = tx.QueryRow(ctx, `SELECT id FROM computers WHERE environment_id=$1 AND id=$2 FOR NO KEY UPDATE`, env, r.Computer).Scan(&r.Computer)
	} else {
		_, err = lockSessions(ctx, tx, env, ids)
	}
	if err != nil {
		return r, err
	}
	err = tx.QueryRow(ctx, `SELECT source_lease_epoch,target_lease_epoch,control_version,disk_save_id,status,capture_expires_at,controls_reconciled_at IS NOT NULL FROM checkpoints WHERE environment_id=$1 AND id=$2 FOR NO KEY UPDATE`, env, checkpoint).Scan(&r.SourceEpoch, &r.TargetEpoch, &r.Version, &r.Save, &r.State, &r.Expires, &r.ControlsReconciled)
	return r, err
}

func (r CaptureRecord) requireMembers(ctx context.Context, tx pgx.Tx, env uuid.UUID) error {
	rows, err := tx.Query(ctx, `SELECT session_id,epoch,computer_lease_epoch FROM session_processes WHERE environment_id=$1 AND computer_id=$2 AND fenced_at IS NULL ORDER BY session_id`, env, r.Computer)
	if err != nil {
		return err
	}
	defer rows.Close()
	var actual []ComputerMember
	for rows.Next() {
		var m ComputerMember
		var lease int64
		if err := rows.Scan(&m.Session, &m.Epoch, &lease); err != nil {
			return err
		}
		if lease != r.SourceEpoch {
			return ErrNotReady
		}
		actual = append(actual, m)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if !slices.EqualFunc(actual, r.Members, func(a, b ComputerMember) bool { return a.Session == b.Session && a.Epoch == b.Epoch }) {
		return ErrNotReady
	}
	return nil
}

// RecordComputerSealed accepts only the successful exact guest freeze receipt.
// It does not make any snapshot remotely durable or authorize source teardown.
func RecordComputerSealed(ctx context.Context, pool db.TxBeginner, host workergroup.HostPrincipal, env, checkpoint uuid.UUID, receipt *agentv1.ComputerSessionReceipt) error {
	return hideMissing(db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		if err := computer.LockHost(ctx, tx, host); err != nil {
			return err
		}
		r, err := LockCapture(ctx, tx, env, checkpoint)
		if err != nil {
			return err
		}
		if _, err = computer.CurrentLease(ctx, tx, host, env, r.Computer, r.SourceEpoch); err != nil {
			return err
		}
		if receipt.GetCheckpointId() != checkpoint.String() || receipt.GetDesiredVersion() != r.Version || !receipt.GetFrozen() || receipt.GetInstalled() || receipt.GetActivationStarted() || receipt.GetActivated() || receipt.GetError() != "" {
			return ErrConflict
		}
		if r.State == "sealed" || r.State == "ready" {
			return nil
		}
		if r.State != "capturing" {
			return ErrNotReady
		}
		if err = r.requireMembers(ctx, tx, env); err != nil {
			return err
		}
		pending, err := computerExecutionDemand(ctx, tx, env, r.Computer)
		if err != nil {
			return err
		}
		if pending {
			return ErrComputerCaptureDemand
		}
		return computer.SealCheckpoint(ctx, tx, env, checkpoint)
	}))
}

// UnsealedCaptureEvidence comes from the owned worker exchange, never a public
// request. Rejected means a completed synchronous response that retained no
// capture, with all attempts joined and no delayed retries. Otherwise the worker
// must inspect absence after the supplied expiry;
// delayed arrival of an earlier observation is not such evidence.
type UnsealedCaptureEvidence struct {
	Rejected         bool
	AbsentObservedAt time.Time
}

func CancelUnsealedComputerCapture(ctx context.Context, pool db.TxBeginner, host workergroup.HostPrincipal, env, checkpoint uuid.UUID, evidence UnsealedCaptureEvidence) error {
	return hideMissing(db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		if err := computer.LockHost(ctx, tx, host); err != nil {
			return err
		}
		r, err := LockCapture(ctx, tx, env, checkpoint)
		if err != nil {
			return err
		}
		if _, err = computer.CurrentLease(ctx, tx, host, env, r.Computer, r.SourceEpoch); err != nil {
			return err
		}
		if r.State == "cancelled" {
			return nil
		}
		if r.State != "capturing" {
			return ErrNotReady
		}
		reason := "guest rejected capture without retaining it"
		if !evidence.Rejected {
			var now time.Time
			if err = tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
				return err
			}
			if evidence.AbsentObservedAt.Before(r.Expires) || evidence.AbsentObservedAt.After(now) {
				return ErrNotReady
			}
			reason = "guest capture absent after envelope expiry"
		}
		// A disk cut must not already have been recorded for an allegedly absent
		// capture. Roll back everything if that evidence conflicts.
		return computer.CancelUnsealedCheckpoint(ctx, tx, env, checkpoint, r.Save, reason)
	}))
}
