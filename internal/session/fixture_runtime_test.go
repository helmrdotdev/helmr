package session_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/session"

	"github.com/helmrdotdev/helmr/internal/computer"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
)

// Test fixtures drive Turn lifecycle transitions through the production runtime
// operations with the fixture Worker's host principal, so the same authority,
// attachment and lease checks apply as in the Control Plane.

func testHostFor(ctx context.Context, pool db.TxBeginner, e session.Execution) (workergroup.HostPrincipal, error) {
	var group uuid.UUID
	if err := db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT worker_group_id FROM worker_hosts WHERE id=$1`, e.WorkerHostID).Scan(&group)
	}); err != nil {
		return workergroup.HostPrincipal{}, err
	}
	return workergroup.HostPrincipal{HostID: e.WorkerHostID, GroupID: group, Epoch: e.WorkerEpoch, HostClaimVersion: 1, GroupClaimVersion: 1}, nil
}

func currentAttachment(ctx context.Context, pool db.TxBeginner, host workergroup.HostPrincipal, e session.Execution) (int64, error) {
	var sequence int64
	if err := db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT attachment_sequence FROM session_processes WHERE environment_id=$1 AND session_id=$2 AND epoch=$3`, e.EnvironmentID, e.SessionID, e.ProcessEpoch).Scan(&sequence)
	}); err != nil {
		return 0, err
	}
	if sequence > 0 {
		return sequence, nil
	}
	a, err := session.AcquireRuntimeAttachment(ctx, pool, host, e)
	if err != nil {
		return 0, err
	}
	return a.Sequence, nil
}

func dispatchTurnForTest(ctx context.Context, pool db.TxBeginner, e session.Execution) (session.TurnDispatch, error) {
	host, err := testHostFor(ctx, pool, e)
	if err != nil {
		return session.TurnDispatch{}, err
	}
	attachment, err := currentAttachment(ctx, pool, host, e)
	if err != nil {
		return session.TurnDispatch{}, fmt.Errorf("acquire attachment: %w", err)
	}
	dispatch, err := session.DispatchRuntimeTurn(ctx, auditpolicy.Policy{
		RetentionSeconds: 3600,
	}, pool, host, e, attachment, nil)
	if err != nil {
		return session.TurnDispatch{}, fmt.Errorf("dispatch: %w", err)
	}
	if dispatch == nil {
		return session.TurnDispatch{}, session.ErrNotReady
	}
	return *dispatch, nil
}

func closeProcessingForTest(ctx context.Context, pool db.TxBeginner, e session.Execution, turn uuid.UUID) error {
	host, err := testHostFor(ctx, pool, e)
	if err != nil {
		return err
	}
	return session.RuntimeCloseProcessing(ctx, auditpolicy.Policy{
		RetentionSeconds: 3600,
	}, pool, host, e, turn)
}

func recordResultForTest(ctx context.Context, pool db.TxBeginner, e session.Execution, turn uuid.UUID, result json.RawMessage, evidence string) (computer.SaveRequest, error) {
	host, err := testHostFor(ctx, pool, e)
	if err != nil {
		return computer.SaveRequest{}, err
	}
	if _, err := session.RuntimeFinalize(ctx, auditpolicy.Policy{RetentionSeconds: 3600}, pool, host, e, turn, result, evidence); err != nil {
		return computer.SaveRequest{}, err
	}
	var save computer.SaveRequest
	err = db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT id,computer_id,computer_lease_epoch,seq,requested_at FROM computer_saves WHERE environment_id=$1 AND turn_id=$2`, e.EnvironmentID, turn).Scan(&save.ID, &save.ComputerID, &save.LeaseEpoch, &save.Sequence, &save.RequestedAt)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return computer.SaveRequest{}, session.ErrNotReady
	}
	return save, err
}

func recordCaptureForTest(ctx context.Context, pool db.TxBeginner, c computer.CaptureEvidence) error {
	if !sha256sum.ValidDigest(c.DiskRoot) {
		return session.ErrInvalidInput
	}
	if c.Evidence == "" {
		return session.ErrNotReady
	}
	return db.RunTx(ctx, pool, func(tx pgx.Tx) error { return computer.RecordCapture(ctx, tx, c) })
}

func attachComputerPreparationForTest(ctx context.Context, pool db.TxBeginner, env, computerID uuid.UUID) (computer.ImageBuild, error) {
	var result computer.ImageBuild
	if env == uuid.Nil() || computerID == uuid.Nil() {
		return result, session.ErrInvalidInput
	}
	err := db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		var err error
		result, err = computer.AttachImageBuild(ctx, tx, env, computerID)
		return err
	})
	if err != nil {
		return computer.ImageBuild{}, session.HideMissing(err)
	}
	return result, nil
}

func releaseComputerLeaseDiskForTest(ctx context.Context, pool db.TxBeginner, env, computerID uuid.UUID, epoch int64) error {
	if env == uuid.Nil() || computerID == uuid.Nil() || epoch <= 0 {
		return session.ErrInvalidInput
	}
	return session.HideMissing(db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		return computer.ReleaseLeaseDisk(ctx, tx, env, computerID, epoch)
	}))
}

// savePublicationForTest observes a save's committed publication state the way
// a retrying publisher would: terminal failure, a conflicting root, pending, or
// published.
func savePublicationForTest(ctx context.Context, pool db.TxBeginner, env, saveID uuid.UUID, diskRoot string) error {
	if !sha256sum.ValidDigest(diskRoot) {
		return session.ErrInvalidInput
	}
	return session.HideMissing(db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		var state string
		var root *string
		if err := tx.QueryRow(ctx, `SELECT status,'sha256:'||encode(captured_root_digest,'hex') FROM computer_saves WHERE environment_id=$1 AND id=$2`, env, saveID).Scan(&state, &root); err != nil {
			return err
		}
		if state == "failed" {
			return session.ErrTerminal
		}
		if root != nil && *root != diskRoot {
			return session.ErrConflict
		}
		if state != "published" {
			return session.ErrNotReady
		}
		return nil
	}))
}

// bindComputerLeaseDiskForTest records the admitted lease's mounted source and
// write key as the Worker's key broker does after acquisition.
func bindComputerLeaseDiskForTest(ctx context.Context, pool db.TxBeginner, host workergroup.HostPrincipal, env, computerID uuid.UUID, epoch int64, writeKey uuid.UUID) (disk.VersionRoot, error) {
	var result disk.VersionRoot
	if env == uuid.Nil() || computerID == uuid.Nil() || epoch <= 0 || writeKey == uuid.Nil() {
		return result, session.ErrInvalidInput
	}
	err := db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		if err := computer.LockHost(ctx, tx, host); err != nil {
			return err
		}
		var initial string
		if err := tx.QueryRow(ctx, `SELECT 'sha256:'||encode(initial_root_digest,'hex') FROM computers WHERE environment_id=$1 AND id=$2
 AND NOT EXISTS(SELECT 1 FROM (`+db.RevokedComputerSecretsSQL+`) revoked_secrets WHERE environment_id=$1 AND computer_id=$2) FOR NO KEY UPDATE`, env, computerID).Scan(&initial); err != nil {
			return err
		}
		var restored *uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT restored_from_save_id FROM computer_leases WHERE environment_id=$1 AND computer_id=$2 AND epoch=$3 AND worker_host_id=$4 AND worker_epoch=$5 AND status IN ('acquiring','active','releasing') AND fenced_at IS NULL AND expires_at>clock_timestamp()`, env, computerID, epoch, host.HostID, host.Epoch).Scan(&restored); err != nil {
			return err
		}
		var rootID uuid.UUID
		if restored == nil {
			if err := tx.QueryRow(ctx, `SELECT r.id,`+computer.DiskRootColumns+` FROM computers c JOIN disk_roots r ON (r.environment_id,r.id)=(c.environment_id,c.initial_root_id) WHERE c.environment_id=$1 AND c.id=$2`, env, computerID).Scan(append([]any{&rootID}, computer.DiskRootFields(&result)...)...); err != nil {
				return err
			}
		} else {
			if err := tx.QueryRow(ctx, `SELECT r.id,`+computer.DiskRootColumns+` FROM computer_saves v JOIN disk_roots r ON (r.environment_id,r.id)=(v.environment_id,v.root_id) WHERE v.environment_id=$1 AND v.computer_id=$2 AND v.id=$3`, env, computerID, *restored).Scan(append([]any{&rootID}, computer.DiskRootFields(&result)...)...); err != nil {
				return err
			}
		}
		identity, err := result.Digest()
		if err != nil {
			return err
		}
		if restored == nil && identity != initial {
			return session.ErrConflict
		}
		var authorizedKey uuid.UUID
		if err = tx.QueryRow(ctx, `SELECT id FROM disk_encryption_keys WHERE environment_id=$1 AND writer_computer_id=$2 AND id=$3 AND available FOR KEY SHARE`, env, computerID, writeKey).Scan(&authorizedKey); err != nil {
			return session.HideMissing(err)
		}
		if _, err = tx.Exec(ctx, `UPDATE computer_leases SET base_root_id=$4,write_key_id=$5 WHERE environment_id=$1 AND computer_id=$2 AND epoch=$3 AND base_root_id IS NULL AND disk_released_at IS NULL`, env, computerID, epoch, rootID, writeKey); err != nil {
			return err
		}
		var priorRoot, priorKey uuid.UUID
		if err = tx.QueryRow(ctx, `SELECT base_root_id,write_key_id FROM computer_leases WHERE environment_id=$1 AND computer_id=$2 AND epoch=$3 AND base_root_id IS NOT NULL AND disk_released_at IS NULL`, env, computerID, epoch).Scan(&priorRoot, &priorKey); err != nil {
			return err
		}
		if priorRoot != rootID || priorKey != writeKey {
			return session.ErrConflict
		}
		var valid bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM computer_leases WHERE environment_id=$1 AND computer_id=$2 AND epoch=$3 AND fenced_at IS NULL AND status IN ('acquiring','active','releasing') AND expires_at>clock_timestamp()) AND NOT EXISTS(SELECT 1 FROM (`+db.RevokedComputerSecretsSQL+`) revoked_secrets WHERE environment_id=$1 AND computer_id=$2)`, env, computerID, epoch).Scan(&valid); err != nil {
			return err
		}
		if !valid {
			return session.ErrDenied
		}
		return nil
	})
	if err != nil {
		return disk.VersionRoot{}, session.HideMissing(err)
	}
	return result, nil
}

func preparationIdentity(e computer.ImageBuildExecutor) computer.ImageBuildIdentity {
	return computer.ImageBuildIdentity{EnvironmentID: e.EnvironmentID, ImageBuildID: e.ImageBuildID, InstanceID: e.InstanceID, Epoch: e.Epoch}
}

func sameLifetime(a, b *int64) bool { return a == nil && b == nil || a != nil && b != nil && *a == *b }

// Completion is already durable before background save work starts.
func completedTurnForTest(ctx context.Context, pool db.TxBeginner, env, sessionID, turn uuid.UUID) error {
	return db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		var status string
		if err := tx.QueryRow(ctx, `SELECT status FROM turns WHERE environment_id=$1 AND session_id=$2 AND id=$3`, env, sessionID, turn).Scan(&status); err != nil {
			return err
		}
		if status == "completed" {
			return nil
		}
		if status == "running" || status == "queued" {
			return session.ErrNotReady
		}
		return session.ErrTerminal
	})
}
