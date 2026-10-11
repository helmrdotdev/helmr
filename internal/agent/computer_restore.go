package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"github.com/helmrdotdev/helmr/internal/computercheckpoint"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	agentv1 "github.com/helmrdotdev/helmr/internal/proto/agent/v1"
	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/proto"
)

// PrepareComputerRestore binds an already-admitted target lease to an eligible
// checkpoint. The acquisition owner must first fence the source, reserve target
// capacity, create its unique physical instance and bind its coherent disk.
// This operation neither allocates capacity nor permits customer execution.
// The host retains an installed request exactly; only an uninstalled attempt may
// request freshly bounded grants. A new physical attempt needs a new lease.
func PrepareComputerRestore(ctx context.Context, pool db.TxBeginner, host workergroup.HostPrincipal, env, checkpoint uuid.UUID, epoch int64, credential string) (*agentv1.ComputerSessionInstallation, error) {
	if env == uuid.Nil() || checkpoint == uuid.Nil() || epoch <= 0 || len(credential) == 0 || len(credential) > 4096 {
		return nil, ErrInvalidInput
	}
	var result *agentv1.ComputerSessionInstallation
	err := db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		if err := lockComputerHost(ctx, tx, host); err != nil {
			return err
		}
		r, err := lockCapture(ctx, tx, env, checkpoint)
		if err != nil {
			return err
		}
		var prior *int64
		var version *int64
		var raw, capture []byte
		if err = tx.QueryRow(ctx, `SELECT target_lease_epoch,restore_control_version,manifest,capture_request FROM computer_checkpoints WHERE environment_id=$1 AND id=$2`, env, checkpoint).Scan(&prior, &version, &raw, &capture); err != nil {
			return err
		}
		target, err := restoreTargetLease(ctx, tx, host, env, r, epoch)
		if err != nil {
			return err
		}
		if r.State != "ready" && r.State != "restoring" {
			return ErrNotReady
		}
		supplied := sha256.Sum256([]byte(credential))
		if !bytes.Equal(supplied[:], target.CredentialDigest) {
			return ErrDenied
		}
		var manifest computercheckpoint.Manifest
		if json.Unmarshal(raw, &manifest) != nil {
			return ErrNotReady
		}
		if err = validateCheckpointRuntime(ctx, tx, host, manifest.Runtime); err != nil {
			return err
		}
		eligible, err := checkpointDiskEligible(ctx, tx, env, r.Computer, r.Save)
		if err != nil {
			return err
		}
		if !eligible {
			return ErrNotReady
		}
		var sourceFenced bool
		var sourceCredential []byte
		if err = tx.QueryRow(ctx, `SELECT fenced_at IS NOT NULL,channel_credential_digest FROM computer_leases WHERE environment_id=$1 AND computer_id=$2 AND epoch=$3`, env, r.Computer, r.SourceEpoch).Scan(&sourceFenced, &sourceCredential); err != nil {
			return err
		}
		if !sourceFenced {
			return ErrNotReady
		}
		if bytes.Equal(sourceCredential, target.CredentialDigest) {
			return ErrDenied
		}
		fresh := prior == nil || *prior != epoch
		if fresh {
			if prior != nil {
				var fenced bool
				if err = tx.QueryRow(ctx, `SELECT fenced_at IS NOT NULL FROM computer_leases WHERE environment_id=$1 AND computer_id=$2 AND epoch=$3`, env, r.Computer, *prior).Scan(&fenced); err != nil {
					return err
				}
				if !fenced {
					return ErrNotReady
				}
			}
			var next int64
			if err = tx.QueryRow(ctx, `UPDATE computers SET next_control_version=next_control_version+1 WHERE environment_id=$1 AND id=$2 RETURNING next_control_version-1`, env, r.Computer).Scan(&next); err != nil {
				return err
			}
			version = &next
			if _, err = tx.Exec(ctx, `UPDATE computer_checkpoints SET status='restoring',target_lease_epoch=$3,restore_control_version=$4,restore_identity=NULL WHERE environment_id=$1 AND id=$2`, env, checkpoint, epoch, next); err != nil {
				return err
			}
		}
		// No resident may have been admitted outside this captured set.
		var added bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM session_processes p WHERE p.environment_id=$1 AND p.computer_id=$2 AND p.fenced_at IS NULL AND NOT EXISTS(SELECT 1 FROM computer_checkpoint_members m WHERE m.environment_id=p.environment_id AND m.checkpoint_id=$3 AND m.session_id=p.session_id AND m.process_epoch=p.epoch))`, env, r.Computer, checkpoint).Scan(&added); err != nil {
			return err
		}
		if added {
			return ErrNotReady
		}
		original := new(agentv1.ComputerSessionCapture)
		if err = proto.Unmarshal(capture, original); err != nil {
			return err
		}
		result = &agentv1.ComputerSessionInstallation{Capture: original, Envelope: &computerv0.ComputerOperationEnvelope{ComputerId: r.Computer.String(), ComputerInstanceId: target.Instance.String(), WriterGeneration: uint64(epoch), ChannelCredential: credential, OperationId: checkpoint.String(), OperationExpiresAtUnixNano: target.Expires.UnixNano()}, DesiredVersion: *version, BaseComputerDiskVersionId: r.Save.String()}
		for _, m := range r.Members {
			var generation int64
			var terminal, fenced bool
			if err = tx.QueryRow(ctx, `SELECT s.authority_generation,s.status IN ('closed','cancelled') OR d.execution_revoked_at IS NOT NULL OR EXISTS(SELECT 1 FROM computer_secret_revocations revoked WHERE revoked.environment_id=s.environment_id AND revoked.computer_id=s.computer_id) OR p.status IN ('stopping','lost','stopped'),p.fenced_at IS NOT NULL FROM sessions s JOIN deployments d ON (d.environment_id,d.id)=(s.environment_id,s.deployment_id) JOIN session_processes p ON (p.environment_id,p.session_id)=(s.environment_id,s.id) WHERE s.environment_id=$1 AND s.id=$2 AND p.epoch=$3`, env, m.Session, m.Epoch).Scan(&generation, &terminal, &fenced); err != nil {
				return err
			}
			if fenced && !terminal {
				return ErrNotReady
			}
			if fresh {
				if err = tx.QueryRow(ctx, `UPDATE sessions SET authority_generation=authority_generation+1 WHERE environment_id=$1 AND id=$2 RETURNING authority_generation`, env, m.Session).Scan(&generation); err != nil {
					return err
				}
				if _, err = tx.Exec(ctx, `UPDATE session_processes SET computer_lease_epoch=$4 WHERE environment_id=$1 AND session_id=$2 AND epoch=$3`, env, m.Session, m.Epoch, epoch); err != nil {
					return err
				}
			}
			identity := &agentv1.SessionIdentity{SessionId: m.Session.String(), ProcessEpoch: m.Epoch}
			result.Grants = append(result.Grants, &agentv1.SessionGrant{Identity: identity, ComputerId: r.Computer.String(), ComputerInstanceId: target.Instance.String(), WriterGeneration: epoch, ComputerLeaseEpoch: epoch, WorkerHostId: host.HostID.String(), AuthorityGeneration: generation, ExpiresAtUnixNano: target.Expires.UnixNano(), ChannelCredential: credential})
			if terminal {
				result.StoppedSessions = append(result.StoppedSessions, proto.Clone(identity).(*agentv1.SessionIdentity))
			}
		}
		_, err = restoreTargetLease(ctx, tx, host, env, r, epoch)
		return err
	})
	if err != nil {
		return nil, hideMissing(err)
	}
	return result, nil
}

func restoreTargetLease(ctx context.Context, tx pgx.Tx, host workergroup.HostPrincipal, env uuid.UUID, r captureRecord, epoch int64) (computerLeaseAuthority, error) {
	var lease computerLeaseAuthority
	if epoch <= r.SourceEpoch {
		return lease, ErrDenied
	}
	err := tx.QueryRow(ctx, `SELECT l.computer_instance_id,l.channel_credential_digest,l.restored_from_save_id::text,LEAST(l.expires_at,clock_timestamp()+interval '30 seconds')
 FROM computer_leases l
 JOIN computer_saves s ON (s.environment_id,s.computer_id,s.id,s.root_id)=(l.environment_id,l.computer_id,l.restored_from_save_id,l.base_root_id)
 WHERE l.environment_id=$1 AND l.computer_id=$2 AND l.epoch=$3 AND l.worker_host_id=$4 AND l.worker_epoch=$5
 AND l.disk_released_at IS NULL AND (l.status='acquiring' OR (l.status='active' AND $7)) AND l.fenced_at IS NULL AND l.expires_at>clock_timestamp() AND l.restored_from_save_id=$6`, env, r.Computer, epoch, host.HostID, host.Epoch, r.Save, r.ControlsReconciled).Scan(&lease.Instance, &lease.CredentialDigest, &lease.BaseVersion, &lease.Expires)
	return lease, err
}

// ValidateComputerRestore checks the owned physical guest before installation.
// The installed request's stable identity survives credential/generation renewal.
func ValidateComputerRestore(ctx context.Context, pool db.TxBeginner, host workergroup.HostPrincipal, env uuid.UUID, p *agentv1.ComputerSessionInstallation, observed *agentv1.ComputerSessionReceipt) error {
	identity, err := computerInstallationIdentity(p, false)
	if err != nil {
		return err
	}
	checkpoint, err := uuid.Parse(p.GetCapture().GetCheckpointId())
	if err != nil {
		return ErrInvalidInput
	}
	return hideMissing(db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		r, err := validateRestoreInstallation(ctx, tx, host, env, checkpoint, p, observed, false)
		if err != nil {
			return err
		}
		if err = requireComputerImageAllowed(ctx, tx, env, r.Computer); err != nil {
			return err
		}
		var stored []byte
		if err = tx.QueryRow(ctx, `SELECT restore_identity FROM computer_checkpoints WHERE environment_id=$1 AND id=$2`, env, checkpoint).Scan(&stored); err != nil {
			return err
		}
		if bytes.Equal(stored, identity) {
			return nil
		}
		// Before installation, current terminal controls may change the stop set.
		// Only a still-frozen, uninstalled receipt permits replacing this identity;
		// installed attempts retain their exact request through activation.
		if r.State != "restoring" || observed.GetInstalled() {
			return ErrConflict
		}
		_, err = tx.Exec(ctx, `UPDATE computer_checkpoints SET restore_identity=$3 WHERE environment_id=$1 AND id=$2`, env, checkpoint, identity)
		return err
	}))
}

// CommitComputerRestore consumes image eligibility before physical activation.
// A retry with an uninstalled guest is a stale snapshot, never a new attempt.
func CommitComputerRestore(ctx context.Context, pool db.TxBeginner, host workergroup.HostPrincipal, env uuid.UUID, p *agentv1.ComputerSessionInstallation, observed *agentv1.ComputerSessionReceipt) error {
	identity, err := computerInstallationIdentity(p, false)
	if err != nil {
		return err
	}
	checkpoint, err := uuid.Parse(p.GetCapture().GetCheckpointId())
	if err != nil {
		return ErrInvalidInput
	}
	return hideMissing(db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		r, err := validateRestoreInstallation(ctx, tx, host, env, checkpoint, p, observed, true)
		if err != nil {
			return err
		}
		if err = matchRestoreInstallation(ctx, tx, env, checkpoint, identity); err != nil {
			return err
		}
		if r.State == "consumed" {
			return nil
		}
		if err = requireComputerImageAllowed(ctx, tx, env, r.Computer); err != nil {
			return err
		}
		// A pre-freeze acknowledgement cannot release this continuation. Reset
		// the delivery identity once, so delayed old receipts also fail to match.
		// Retain a pending failure until lifecycle reconciliation records it.
		if _, err = tx.Exec(ctx, `UPDATE session_processes p SET control_kind=NULL,control_acknowledged_at=NULL,control_error=NULL
 FROM computer_checkpoint_members m WHERE m.environment_id=$1 AND m.checkpoint_id=$2
 AND (p.environment_id,p.session_id,p.epoch)=(m.environment_id,m.session_id,m.process_epoch)
 AND (p.control_error IS NULL OR p.failure_recorded_at IS NOT NULL)`, env, checkpoint); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE computer_checkpoints SET status='consumed' WHERE environment_id=$1 AND id=$2`, env, checkpoint)
		return err
	}))
}

// CompleteComputerRestore admits business only after physical activation and
// current durable controls are acknowledged for every retained process.
func CompleteComputerRestore(ctx context.Context, pool db.TxBeginner, host workergroup.HostPrincipal, env uuid.UUID, p *agentv1.ComputerSessionInstallation, observed *agentv1.ComputerSessionReceipt) error {
	identity, err := computerInstallationIdentity(p, false)
	if err != nil {
		return err
	}
	checkpoint, err := uuid.Parse(p.GetCapture().GetCheckpointId())
	if err != nil {
		return ErrInvalidInput
	}
	return hideMissing(db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		r, err := validateRestoreInstallation(ctx, tx, host, env, checkpoint, p, observed, true)
		if err != nil {
			return err
		}
		if r.State != "consumed" || !observed.GetActivated() || observed.GetFrozen() {
			return ErrNotReady
		}
		if err = matchRestoreInstallation(ctx, tx, env, checkpoint, identity); err != nil {
			return err
		}
		if r.ControlsReconciled {
			return nil
		}
		if err = requireComputerImageAllowed(ctx, tx, env, r.Computer); err != nil {
			return err
		}
		if err = requireComputerControlReceipts(ctx, tx, env, r); err != nil {
			return err
		}
		if _, err = restoreTargetLease(ctx, tx, host, env, r, int64(p.GetEnvelope().GetWriterGeneration())); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `UPDATE computer_leases SET status='active',initialized_at=COALESCE(initialized_at,clock_timestamp()) WHERE environment_id=$1 AND computer_id=$2 AND epoch=$3`, env, r.Computer, int64(p.GetEnvelope().GetWriterGeneration())); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE computer_checkpoints SET controls_reconciled_at=clock_timestamp(),capture_request=NULL WHERE environment_id=$1 AND id=$2`, env, checkpoint)
		return err
	}))
}

func matchRestoreInstallation(ctx context.Context, tx pgx.Tx, env, checkpoint uuid.UUID, identity []byte) error {
	var stored []byte
	if err := tx.QueryRow(ctx, `SELECT restore_identity FROM computer_checkpoints WHERE environment_id=$1 AND id=$2`, env, checkpoint).Scan(&stored); err != nil {
		return err
	}
	if !bytes.Equal(stored, identity) {
		return ErrConflict
	}
	return nil
}

func validateRestoreInstallation(ctx context.Context, tx pgx.Tx, host workergroup.HostPrincipal, env, checkpoint uuid.UUID, p *agentv1.ComputerSessionInstallation, observed *agentv1.ComputerSessionReceipt, commit bool) (captureRecord, error) {
	var r captureRecord
	if err := lockComputerHost(ctx, tx, host); err != nil {
		return r, err
	}
	r, err := lockCapture(ctx, tx, env, checkpoint)
	if err != nil {
		return r, err
	}
	if r.TargetEpoch == nil {
		return r, ErrDenied
	}
	target, err := restoreTargetLease(ctx, tx, host, env, r, *r.TargetEpoch)
	if err != nil {
		return r, err
	}
	epoch := int64(p.GetEnvelope().GetWriterGeneration())
	if epoch != *r.TargetEpoch {
		return r, ErrDenied
	}
	if r.State != "restoring" && r.State != "consumed" {
		return r, ErrNotReady
	}
	if err = validateComputerInstallationReceipt(r, p, observed, commit); err != nil {
		return r, err
	}
	if r.State != "consumed" && !observed.GetFrozen() {
		return r, ErrConflict
	}
	var retained []byte
	var recordedEpoch, version int64
	var healthy bool
	if err = tx.QueryRow(ctx, `SELECT p.capture_digest,p.target_lease_epoch,p.restore_control_version,c.integrity_fault_at IS NULL AND c.deleted_at IS NULL FROM computer_checkpoints p JOIN computers c ON (c.environment_id,c.id)=(p.environment_id,p.computer_id) WHERE p.environment_id=$1 AND p.id=$2`, env, checkpoint).Scan(&retained, &recordedEpoch, &version, &healthy); err != nil {
		return r, err
	}
	if !healthy {
		return r, ErrNotReady
	}
	raw, err := (proto.MarshalOptions{Deterministic: true}).Marshal(p.GetCapture())
	if err != nil || !bytes.Equal(retained, captureDigest(raw)) {
		return r, ErrConflict
	}
	envelope := p.GetEnvelope()
	digest := sha256.Sum256([]byte(envelope.GetChannelCredential()))
	if recordedEpoch != epoch || p.GetDesiredVersion() != version || envelope.GetOperationId() != checkpoint.String() || envelope.GetComputerId() != r.Computer.String() || envelope.GetComputerInstanceId() != target.Instance.String() || !bytes.Equal(digest[:], target.CredentialDigest) || p.GetBaseComputerDiskVersionId() != r.Save.String() {
		return r, ErrDenied
	}
	var now time.Time
	if err = tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return r, err
	}
	fresh := !observed.GetInstalled()
	if fresh && (envelope.GetOperationExpiresAtUnixNano() <= now.UnixNano() || envelope.GetOperationExpiresAtUnixNano() > target.Expires.UnixNano()) {
		return r, ErrNotReady
	}
	if len(p.GetGrants()) != len(r.Members) {
		return r, ErrConflict
	}
	grants := make(map[string]*agentv1.SessionGrant, len(r.Members))
	for _, g := range p.GetGrants() {
		id := g.GetIdentity().GetSessionId()
		if grants[id] != nil || g.GetComputerId() != r.Computer.String() || g.GetComputerInstanceId() != target.Instance.String() || g.GetComputerLeaseEpoch() != epoch || g.GetWriterGeneration() != epoch || g.GetWorkerHostId() != host.HostID.String() || g.GetChannelCredential() != envelope.GetChannelCredential() {
			return r, ErrConflict
		}
		grants[id] = g
	}
	stopped := make(map[string]bool)
	for _, m := range p.GetStoppedSessions() {
		id := m.GetSessionId()
		if stopped[id] || !proto.Equal(m, grants[id].GetIdentity()) {
			return r, ErrConflict
		}
		stopped[id] = true
	}
	for _, m := range r.Members {
		g := grants[m.Session.String()]
		if g.GetIdentity().GetProcessEpoch() != m.Epoch {
			return r, ErrConflict
		}
		var generation, processLease int64
		var terminal, fenced bool
		if err = tx.QueryRow(ctx, `SELECT s.authority_generation,p.computer_lease_epoch,s.status IN ('closed','cancelled') OR d.execution_revoked_at IS NOT NULL OR EXISTS(SELECT 1 FROM computer_secret_revocations revoked WHERE revoked.environment_id=s.environment_id AND revoked.computer_id=s.computer_id) OR p.status IN ('stopping','lost','stopped'),p.fenced_at IS NOT NULL FROM sessions s JOIN deployments d ON (d.environment_id,d.id)=(s.environment_id,s.deployment_id) JOIN session_processes p ON (p.environment_id,p.session_id)=(s.environment_id,s.id) WHERE s.environment_id=$1 AND s.id=$2 AND p.epoch=$3`, env, m.Session, m.Epoch).Scan(&generation, &processLease, &terminal, &fenced); err != nil {
			return r, err
		}
		if processLease != epoch || (fenced && !terminal) {
			return r, ErrNotReady
		}
		if fresh && (terminal != stopped[m.Session.String()] || g.GetAuthorityGeneration() != generation || g.GetExpiresAtUnixNano() <= now.UnixNano() || g.GetExpiresAtUnixNano() > envelope.GetOperationExpiresAtUnixNano()) {
			return r, ErrNotReady
		}
	}
	var added bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM session_processes p WHERE p.environment_id=$1 AND p.computer_id=$2 AND p.fenced_at IS NULL AND NOT EXISTS(SELECT 1 FROM computer_checkpoint_members m WHERE m.environment_id=p.environment_id AND m.checkpoint_id=$3 AND m.session_id=p.session_id AND m.process_epoch=p.epoch))`, env, r.Computer, checkpoint).Scan(&added); err != nil {
		return r, err
	}
	if added {
		return r, ErrNotReady
	}
	_, err = restoreTargetLease(ctx, tx, host, env, r, epoch)
	return r, err
}

// checkpointDiskEligible is shared by restore admission and logical-process loss.
// A newer or unresolved disk cut makes a retained RAM image ineligible.
func checkpointDiskEligible(ctx context.Context, tx pgx.Tx, env, computer, save uuid.UUID) (bool, error) {
	var eligible bool
	err := tx.QueryRow(ctx, `SELECT COALESCE(c.integrity_fault_at IS NULL AND c.deleted_at IS NULL AND NOT EXISTS(SELECT 1 FROM computer_secret_revocations revoked WHERE revoked.environment_id=c.environment_id AND revoked.computer_id=c.id) AND c.recovery_save_id=$3
   AND NOT EXISTS(SELECT 1 FROM computer_saves s WHERE s.environment_id=c.environment_id AND s.computer_id=c.id AND s.status IN ('requested','captured')),false)
   FROM computers c WHERE c.environment_id=$1 AND c.id=$2`, env, computer, save).Scan(&eligible)
	return eligible, err
}
