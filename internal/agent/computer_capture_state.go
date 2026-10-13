package agent

import (
	"context"
	"slices"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	agentv1 "github.com/helmrdotdev/helmr/internal/proto/agent/v1"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
)

type captureRecord struct {
	Computer           uuid.UUID
	SourceEpoch        int64
	TargetEpoch        *int64
	Version            int64
	Save               uuid.UUID
	State              string
	ControlsReconciled bool
	Expires            time.Time
	Members            []computerMember
}

// The caller takes supply locks before this function. Captured members remain
// lockable after loss/fencing; their immutable membership owns recovery holds.
func lockCapture(ctx context.Context, tx pgx.Tx, env, checkpoint uuid.UUID) (captureRecord, error) {
	var r captureRecord
	if err := tx.QueryRow(ctx, `SELECT computer_id FROM computer_checkpoints WHERE environment_id=$1 AND id=$2`, env, checkpoint).Scan(&r.Computer); err != nil {
		return r, err
	}
	rows, err := tx.Query(ctx, `SELECT session_id,process_epoch FROM computer_checkpoint_members WHERE environment_id=$1 AND checkpoint_id=$2 ORDER BY session_id`, env, checkpoint)
	if err != nil {
		return r, err
	}
	var ids []uuid.UUID
	for rows.Next() {
		var m computerMember
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
		return r, ErrNotReady
	}
	if _, err = lockSessions(ctx, tx, env, ids); err != nil {
		return r, err
	}
	err = tx.QueryRow(ctx, `SELECT source_lease_epoch,target_lease_epoch,control_version,disk_save_id,status,capture_expires_at,controls_reconciled_at IS NOT NULL FROM computer_checkpoints WHERE environment_id=$1 AND id=$2 FOR NO KEY UPDATE`, env, checkpoint).Scan(&r.SourceEpoch, &r.TargetEpoch, &r.Version, &r.Save, &r.State, &r.Expires, &r.ControlsReconciled)
	return r, err
}

func (r captureRecord) requireMembers(ctx context.Context, tx pgx.Tx, env uuid.UUID) error {
	rows, err := tx.Query(ctx, `SELECT session_id,epoch,computer_lease_epoch FROM session_processes WHERE environment_id=$1 AND computer_id=$2 AND fenced_at IS NULL ORDER BY session_id`, env, r.Computer)
	if err != nil {
		return err
	}
	defer rows.Close()
	var actual []computerMember
	for rows.Next() {
		var m computerMember
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
	if !slices.EqualFunc(actual, r.Members, func(a, b computerMember) bool { return a.Session == b.Session && a.Epoch == b.Epoch }) {
		return ErrNotReady
	}
	return nil
}

// RecordComputerSealed accepts only the successful exact guest freeze receipt.
// It does not make any snapshot remotely durable or authorize source teardown.
func RecordComputerSealed(ctx context.Context, pool db.TxBeginner, host workergroup.HostPrincipal, env, checkpoint uuid.UUID, receipt *agentv1.ComputerSessionReceipt) error {
	return hideMissing(db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		if err := lockComputerHost(ctx, tx, host); err != nil {
			return err
		}
		r, err := lockCapture(ctx, tx, env, checkpoint)
		if err != nil {
			return err
		}
		if _, err = currentComputerLease(ctx, tx, host, env, r.Computer, r.SourceEpoch); err != nil {
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
		_, err = tx.Exec(ctx, `UPDATE computer_checkpoints SET status='sealed' WHERE environment_id=$1 AND id=$2`, env, checkpoint)
		return err
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
		if err := lockComputerHost(ctx, tx, host); err != nil {
			return err
		}
		r, err := lockCapture(ctx, tx, env, checkpoint)
		if err != nil {
			return err
		}
		if _, err = currentComputerLease(ctx, tx, host, env, r.Computer, r.SourceEpoch); err != nil {
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
		tag, err := tx.Exec(ctx, `UPDATE computer_saves SET status='failed',failure_evidence=$3 WHERE environment_id=$1 AND id=$2 AND status='requested'`, env, r.Save, reason)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return ErrConflict
		}
		_, err = tx.Exec(ctx, `UPDATE computer_checkpoints SET status='cancelled',terminal_evidence=$3,capture_request=NULL WHERE environment_id=$1 AND id=$2`, env, checkpoint, reason)
		return err
	}))
}
