package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
)

// Test fixtures drive Turn lifecycle transitions through the production runtime
// operations with the fixture Worker's host principal, so the same authority,
// attachment and lease checks apply as in the Control Plane.

func testHostFor(ctx context.Context, pool db.TxBeginner, e Execution) (workergroup.HostPrincipal, error) {
	var group uuid.UUID
	if err := db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT worker_group_id FROM worker_hosts WHERE id=$1`, e.WorkerHostID).Scan(&group)
	}); err != nil {
		return workergroup.HostPrincipal{}, err
	}
	return workergroup.HostPrincipal{HostID: e.WorkerHostID, GroupID: group, Epoch: e.WorkerEpoch, HostClaimVersion: 1, GroupClaimVersion: 1}, nil
}

func currentAttachment(ctx context.Context, pool db.TxBeginner, host workergroup.HostPrincipal, e Execution) (int64, error) {
	var sequence int64
	if err := db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT attachment_sequence FROM session_processes WHERE environment_id=$1 AND session_id=$2 AND epoch=$3`, e.EnvironmentID, e.SessionID, e.ProcessEpoch).Scan(&sequence)
	}); err != nil {
		return 0, err
	}
	if sequence > 0 {
		return sequence, nil
	}
	a, err := AcquireRuntimeAttachment(ctx, pool, host, e)
	if err != nil {
		return 0, err
	}
	return a.Sequence, nil
}

func dispatchTurnForTest(ctx context.Context, pool db.TxBeginner, e Execution) (TurnDispatch, error) {
	host, err := testHostFor(ctx, pool, e)
	if err != nil {
		return TurnDispatch{}, err
	}
	attachment, err := currentAttachment(ctx, pool, host, e)
	if err != nil {
		return TurnDispatch{}, fmt.Errorf("acquire attachment: %w", err)
	}
	dispatch, err := DispatchRuntimeTurn(ctx, pool, host, e, attachment)
	if err != nil {
		return TurnDispatch{}, fmt.Errorf("dispatch: %w", err)
	}
	if dispatch == nil {
		return TurnDispatch{}, ErrNotReady
	}
	return *dispatch, nil
}

func closeProcessingForTest(ctx context.Context, pool db.TxBeginner, e Execution, turn uuid.UUID) error {
	host, err := testHostFor(ctx, pool, e)
	if err != nil {
		return err
	}
	return RuntimeCloseProcessing(ctx, pool, host, e, turn)
}

func recordResultForTest(ctx context.Context, pool db.TxBeginner, e Execution, turn uuid.UUID, result json.RawMessage, evidence string) (SaveRequest, error) {
	host, err := testHostFor(ctx, pool, e)
	if err != nil {
		return SaveRequest{}, err
	}
	if _, err := RuntimeFinalize(ctx, pool, host, e, turn, result, evidence); err != nil {
		return SaveRequest{}, err
	}
	var save SaveRequest
	err = db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT id,computer_id,computer_lease_epoch,seq,requested_at FROM computer_saves WHERE environment_id=$1 AND turn_id=$2`, e.EnvironmentID, turn).Scan(&save.ID, &save.ComputerID, &save.LeaseEpoch, &save.Sequence, &save.RequestedAt)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return SaveRequest{}, ErrNotReady
	}
	return save, err
}

func recordCaptureForTest(ctx context.Context, pool db.TxBeginner, c CaptureEvidence) error {
	if !sha256sum.ValidDigest(c.DiskRoot) {
		return ErrInvalidInput
	}
	if c.Evidence == "" {
		return ErrNotReady
	}
	return db.RunTx(ctx, pool, func(tx pgx.Tx) error { return recordCapture(ctx, tx, c) })
}

func attachComputerPreparationForTest(ctx context.Context, pool db.TxBeginner, env, computer uuid.UUID) (Preparation, error) {
	var result Preparation
	if env == uuid.Nil() || computer == uuid.Nil() {
		return result, ErrInvalidInput
	}
	err := db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		var err error
		result, err = attachComputerPreparation(ctx, tx, env, computer)
		return err
	})
	if err != nil {
		return Preparation{}, hideMissing(err)
	}
	return result, nil
}

func releaseComputerLeaseDiskForTest(ctx context.Context, pool db.TxBeginner, env, computer uuid.UUID, epoch int64) error {
	if env == uuid.Nil() || computer == uuid.Nil() || epoch <= 0 {
		return ErrInvalidInput
	}
	return hideMissing(db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		return releaseComputerLeaseDisk(ctx, tx, env, computer, epoch)
	}))
}

// savePublicationForTest observes a save's committed publication state the way
// a retrying publisher would: terminal failure, a conflicting root, pending, or
// published.
func savePublicationForTest(ctx context.Context, pool db.TxBeginner, env, saveID uuid.UUID, diskRoot string) error {
	if !sha256sum.ValidDigest(diskRoot) {
		return ErrInvalidInput
	}
	return hideMissing(db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		var state string
		var root *string
		if err := tx.QueryRow(ctx, `SELECT status,'sha256:'||encode(captured_root_digest,'hex') FROM computer_saves WHERE environment_id=$1 AND id=$2`, env, saveID).Scan(&state, &root); err != nil {
			return err
		}
		if state == "failed" {
			return ErrTerminal
		}
		if root != nil && *root != diskRoot {
			return ErrConflict
		}
		if state != "published" {
			return ErrNotReady
		}
		return nil
	}))
}

// bindComputerLeaseDiskForTest records the admitted lease's mounted source and
// write key as the Worker's key broker does after acquisition.
func bindComputerLeaseDiskForTest(ctx context.Context, pool db.TxBeginner, host workergroup.HostPrincipal, env, computer uuid.UUID, epoch int64, writeKey uuid.UUID) (disk.VersionRoot, error) {
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
 AND NOT EXISTS(SELECT 1 FROM (`+db.RevokedComputerSecretsSQL+`) revoked_secrets WHERE environment_id=$1 AND computer_id=$2) FOR NO KEY UPDATE`, env, computer).Scan(&initial); err != nil {
			return err
		}
		var restored *uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT restored_from_save_id FROM computer_leases WHERE environment_id=$1 AND computer_id=$2 AND epoch=$3 AND worker_host_id=$4 AND worker_epoch=$5 AND status IN ('acquiring','active','releasing') AND fenced_at IS NULL AND expires_at>clock_timestamp()`, env, computer, epoch, host.HostID, host.Epoch).Scan(&restored); err != nil {
			return err
		}
		var rootID uuid.UUID
		if restored == nil {
			if err := tx.QueryRow(ctx, `SELECT r.id,`+diskRootColumns+` FROM computers c JOIN computer_disk_roots r ON (r.environment_id,r.id)=(c.environment_id,c.initial_root_id) WHERE c.environment_id=$1 AND c.id=$2`, env, computer).Scan(append([]any{&rootID}, diskRootFields(&result)...)...); err != nil {
				return err
			}
		} else {
			if err := tx.QueryRow(ctx, `SELECT r.id,`+diskRootColumns+` FROM computer_saves v JOIN computer_disk_roots r ON (r.environment_id,r.id)=(v.environment_id,v.root_id) WHERE v.environment_id=$1 AND v.computer_id=$2 AND v.id=$3`, env, computer, *restored).Scan(append([]any{&rootID}, diskRootFields(&result)...)...); err != nil {
				return err
			}
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
		var valid bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM computer_leases WHERE environment_id=$1 AND computer_id=$2 AND epoch=$3 AND fenced_at IS NULL AND status IN ('acquiring','active','releasing') AND expires_at>clock_timestamp()) AND NOT EXISTS(SELECT 1 FROM (`+db.RevokedComputerSecretsSQL+`) revoked_secrets WHERE environment_id=$1 AND computer_id=$2)`, env, computer, epoch).Scan(&valid); err != nil {
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

// loseUnreadyComputerCaptureForTest applies the reconciler's loss transition for
// a checkpoint whose owning lease is fenced or whose Worker epoch was replaced.
func loseUnreadyComputerCaptureForTest(ctx context.Context, pool db.TxBeginner, env, checkpoint uuid.UUID) error {
	return hideMissing(db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		var host, group uuid.UUID
		var ownerEpoch int64
		if err := tx.QueryRow(ctx, `SELECT l.worker_host_id,h.worker_group_id,l.epoch FROM computer_checkpoints c JOIN computer_leases l ON (l.environment_id,l.computer_id,l.epoch)=(c.environment_id,c.computer_id,COALESCE(c.target_lease_epoch,c.source_lease_epoch)) JOIN worker_hosts h ON h.id=l.worker_host_id WHERE c.environment_id=$1 AND c.id=$2`, env, checkpoint).Scan(&host, &group, &ownerEpoch); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT id FROM worker_groups WHERE id=$1 FOR SHARE`, group); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT id FROM worker_hosts WHERE id=$1 FOR SHARE`, host); err != nil {
			return err
		}
		r, err := lockCapture(ctx, tx, env, checkpoint)
		if err != nil {
			return err
		}
		if r.State == "lost" {
			return nil
		}
		if r.ControlsReconciled || (r.State != "capturing" && r.State != "sealed" && r.State != "aborting" && r.State != "consumed") {
			return ErrNotReady
		}
		currentEpoch := r.SourceEpoch
		if r.TargetEpoch != nil {
			currentEpoch = *r.TargetEpoch
		}
		if currentEpoch != ownerEpoch {
			return ErrNotReady
		}
		var fenced, replaced bool
		if err = tx.QueryRow(ctx, `SELECT l.fenced_at IS NOT NULL,COALESCE(h.current_epoch>l.worker_epoch,false) FROM computer_leases l JOIN worker_hosts h ON h.id=l.worker_host_id WHERE l.environment_id=$1 AND l.computer_id=$2 AND l.epoch=$3`, env, r.Computer, ownerEpoch).Scan(&fenced, &replaced); err != nil {
			return err
		}
		if !fenced && !replaced {
			return ErrNotReady
		}
		for _, m := range r.Members {
			if err = loseSessionProcess(ctx, tx, Execution{EnvironmentID: env, SessionID: m.Session, ProcessEpoch: m.Epoch, LeaseEpoch: ownerEpoch}, fenced); err != nil {
				return err
			}
		}
		_, err = tx.Exec(ctx, `UPDATE computer_checkpoints SET status='lost',terminal_evidence='physical owner unavailable without an eligible restore',capture_request=NULL WHERE environment_id=$1 AND id=$2`, env, checkpoint)
		return err
	}))
}

func preparationIdentity(e PreparationExecutor) PreparationIdentity {
	return PreparationIdentity{EnvironmentID: e.EnvironmentID, PreparationID: e.PreparationID, InstanceID: e.InstanceID, Epoch: e.Epoch}
}

func sameLifetime(a, b *int64) bool { return a == nil && b == nil || a != nil && b != nil && *a == *b }
