package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"slices"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	agentv1 "github.com/helmrdotdev/helmr/internal/proto/agent/v1"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/proto"
)

// Source abort retains the existing physical machine and mounted filesystem.
// The host can refresh grants after inspecting an uninstalled guest. Once the
// guest installs, the host retains that exact request for its live-machine
// lifetime; further refreshes use ordinary Session renewal.
// The durable identity excludes expiring grants, which are transport authority.
func computerInstallationIdentity(p *agentv1.ComputerSessionInstallation, sourceAbort bool) ([]byte, error) {
	if p == nil || p.GetSourceAbort() != sourceAbort || p.GetCapture() == nil || p.GetEnvelope() == nil || p.GetDesiredVersion() <= p.GetCapture().GetDesiredVersion() {
		return nil, ErrInvalidInput
	}
	if proto.Size(p) > 16*1024*1024 {
		return nil, ErrInvalidInput
	}
	for _, g := range p.GetGrants() {
		if g == nil || g.GetIdentity() == nil {
			return nil, ErrInvalidInput
		}
	}
	for _, m := range p.GetStoppedSessions() {
		if m == nil {
			return nil, ErrInvalidInput
		}
	}
	c := proto.Clone(p).(*agentv1.ComputerSessionInstallation)
	c.Capture = nil // Independently checked against the exact retained request.
	c.Envelope.ChannelCredential = ""
	c.Envelope.OperationExpiresAtUnixNano = 0
	for _, g := range c.Grants {
		g.ChannelCredential = ""
		g.ExpiresAtUnixNano = 0
		g.AuthorityGeneration = 0
	}
	slices.SortFunc(c.Grants, func(a, b *agentv1.SessionGrant) int {
		return bytes.Compare([]byte(a.GetIdentity().GetSessionId()), []byte(b.GetIdentity().GetSessionId()))
	})
	slices.SortFunc(c.StoppedSessions, func(a, b *agentv1.SessionIdentity) int {
		return bytes.Compare([]byte(a.GetSessionId()), []byte(b.GetSessionId()))
	})
	encoded, err := (proto.MarshalOptions{Deterministic: true}).Marshal(c)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(encoded)
	return digest[:], nil
}

// validateSourceAbort runs with supply and captured ownership locks held. No
// public caller can supply this installation: worker authentication owns it.
func validateSourceAbort(ctx context.Context, tx pgx.Tx, host workergroup.HostPrincipal, env, checkpoint uuid.UUID, r captureRecord, p *agentv1.ComputerSessionInstallation, fresh, initial bool) error {
	if p.GetDesiredVersion() != r.Version+1 {
		return ErrConflict
	}
	var healthy bool
	if err := tx.QueryRow(ctx, `SELECT integrity_fault_at IS NULL AND deleted_at IS NULL FROM computers WHERE environment_id=$1 AND id=$2`, env, r.Computer).Scan(&healthy); err != nil {
		return err
	}
	if !healthy {
		return ErrNotReady
	}
	lease, err := currentComputerLease(ctx, tx, host, env, r.Computer, r.SourceEpoch)
	if err != nil {
		return err
	}
	var retained []byte
	if err = tx.QueryRow(ctx, `SELECT capture_digest FROM computer_checkpoints WHERE environment_id=$1 AND id=$2`, env, checkpoint).Scan(&retained); err != nil {
		return err
	}
	encoded, err := (proto.MarshalOptions{Deterministic: true}).Marshal(p.GetCapture())
	if err != nil || !bytes.Equal(retained, captureDigest(encoded)) {
		return ErrConflict
	}
	target := p.GetEnvelope()
	digest := sha256.Sum256([]byte(target.GetChannelCredential()))
	if target.GetComputerId() != r.Computer.String() || target.GetComputerInstanceId() != lease.Instance.String() || target.GetWriterGeneration() != uint64(r.SourceEpoch) || !bytes.Equal(digest[:], lease.CredentialDigest) || p.GetBaseComputerDiskVersionId() != lease.BaseVersion || target.GetOperationId() != checkpoint.String() {
		return ErrDenied
	}
	var now time.Time
	if err = tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return err
	}
	if fresh && (target.GetOperationExpiresAtUnixNano() <= now.UnixNano() || target.GetOperationExpiresAtUnixNano() > lease.Expires.UnixNano()) {
		return ErrNotReady
	}
	if len(p.GetGrants()) != len(r.Members) {
		return ErrConflict
	}
	grants := make(map[string]*agentv1.SessionGrant, len(r.Members))
	for _, g := range p.GetGrants() {
		id := g.GetIdentity().GetSessionId()
		if grants[id] != nil || g.GetComputerId() != r.Computer.String() || g.GetComputerInstanceId() != lease.Instance.String() || g.GetComputerLeaseEpoch() != r.SourceEpoch || g.GetWriterGeneration() != r.SourceEpoch || g.GetWorkerHostId() != host.HostID.String() || g.GetChannelCredential() != target.GetChannelCredential() {
			return ErrConflict
		}
		grants[id] = g
	}
	stopped := make(map[string]bool)
	for _, s := range p.GetStoppedSessions() {
		id := s.GetSessionId()
		if stopped[id] || !proto.Equal(s, grants[id].GetIdentity()) {
			return ErrConflict
		}
		stopped[id] = true
	}
	for _, m := range r.Members {
		g := grants[m.Session.String()]
		if g.GetIdentity().GetProcessEpoch() != m.Epoch {
			return ErrConflict
		}
		var generation, processLease int64
		var terminal, fenced bool
		if err = tx.QueryRow(ctx, `SELECT s.authority_generation,s.status IN ('closed','cancelled') OR d.execution_revoked_at IS NOT NULL OR EXISTS(SELECT 1 FROM computer_secret_revocations revoked WHERE revoked.environment_id=s.environment_id AND revoked.computer_id=s.computer_id) OR p.status IN ('stopping','lost','stopped'),p.computer_lease_epoch,p.fenced_at IS NOT NULL
   FROM sessions s JOIN deployments d ON (d.environment_id,d.id)=(s.environment_id,s.deployment_id)
   JOIN session_processes p ON (p.environment_id,p.session_id)=(s.environment_id,s.id)
   WHERE s.environment_id=$1 AND s.id=$2 AND p.epoch=$3`, env, m.Session, m.Epoch).Scan(&generation, &terminal, &processLease, &fenced); err != nil {
			return err
		}
		if processLease != r.SourceEpoch || (fenced && (!terminal || (initial && !stopped[m.Session.String()]))) {
			return ErrNotReady
		}
		if initial && terminal != stopped[m.Session.String()] {
			return ErrNotReady
		}
		if fresh && (g.GetAuthorityGeneration() != generation || g.GetExpiresAtUnixNano() <= now.UnixNano() || g.GetExpiresAtUnixNano() > target.GetOperationExpiresAtUnixNano()) {
			return ErrNotReady
		}
	}
	var added bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM session_processes p WHERE p.environment_id=$1 AND p.computer_id=$2 AND p.fenced_at IS NULL AND NOT EXISTS(SELECT 1 FROM computer_checkpoint_members m WHERE m.environment_id=p.environment_id AND m.checkpoint_id=$3 AND m.session_id=p.session_id AND m.process_epoch=p.epoch))`, env, r.Computer, checkpoint).Scan(&added); err != nil {
		return err
	}
	if added {
		return ErrNotReady
	}
	// Owner locks stabilize identity, but not wall-clock lease expiry during
	// the member checks. Recheck that deadline before issuing authority.
	_, err = currentComputerLease(ctx, tx, host, env, r.Computer, r.SourceEpoch)
	return err
}

// ValidateComputerSourceAbort inspects retained guest evidence and seals the
// exact same-machine continuation before any guest mutation. Its version is the successor to the capture version.
// Disk saves keep their independent reconciliation owner, including missing
// acknowledgements. No physical thaw or business dispatch is authorized.
func ValidateComputerSourceAbort(ctx context.Context, pool db.TxBeginner, host workergroup.HostPrincipal, env uuid.UUID, p *agentv1.ComputerSessionInstallation, observed *agentv1.ComputerSessionReceipt) error {
	identity, err := computerInstallationIdentity(p, true)
	if err != nil {
		return err
	}
	checkpoint, err := uuid.Parse(p.GetCapture().GetCheckpointId())
	if err != nil {
		return ErrInvalidInput
	}
	return hideMissing(db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		if err := lockComputerHost(ctx, tx, host); err != nil {
			return err
		}
		r, err := lockCapture(ctx, tx, env, checkpoint)
		if err != nil {
			return err
		}
		// Do not disclose continuation state to another physical owner.
		if _, err = currentComputerLease(ctx, tx, host, env, r.Computer, r.SourceEpoch); err != nil {
			return err
		}
		if r.State != "capturing" && r.State != "sealed" && r.State != "ready" && r.State != "aborting" && r.State != "consumed" {
			return ErrNotReady
		}
		fresh := r.State != "aborting" && r.State != "consumed"
		if err = validateComputerInstallationReceipt(r, p, observed, false); err != nil {
			return err
		}
		if fresh && observed.GetInstalled() {
			return ErrConflict
		}
		if err = validateSourceAbort(ctx, tx, host, env, checkpoint, r, p, !observed.GetInstalled(), !observed.GetInstalled()); err != nil {
			return err
		}
		if !fresh {
			if r.State == "aborting" && !observed.GetInstalled() {
				// A live observation that no installation exists permits refreshing the
				// terminal set before installation. The physical owner, membership and
				// successor version remain fixed by validateSourceAbort.
				_, err = tx.Exec(ctx, `UPDATE computer_checkpoints SET abort_identity=$3 WHERE environment_id=$1 AND id=$2`, env, checkpoint, identity)
				return err
			}
			return matchSourceAbort(ctx, tx, env, checkpoint, identity)
		}
		tag, err := tx.Exec(ctx, `UPDATE computers SET next_control_version=next_control_version+1 WHERE environment_id=$1 AND id=$2 AND next_control_version=$3 AND integrity_fault_at IS NULL AND deleted_at IS NULL`, env, r.Computer, p.GetDesiredVersion())
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return ErrConflict
		}
		_, err = tx.Exec(ctx, `UPDATE computer_checkpoints SET status='aborting',abort_identity=$3 WHERE environment_id=$1 AND id=$2`, env, checkpoint, identity)
		return err
	}))
}

func matchSourceAbort(ctx context.Context, tx pgx.Tx, env, checkpoint uuid.UUID, identity []byte) error {
	var stored []byte
	if err := tx.QueryRow(ctx, `SELECT abort_identity FROM computer_checkpoints WHERE environment_id=$1 AND id=$2`, env, checkpoint).Scan(&stored); err != nil {
		return err
	}
	if !bytes.Equal(stored, identity) {
		return ErrConflict
	}
	return nil
}

// CommitComputerSourceAbort consumes checkpoint eligibility before physical
// activation. Dispatch remains sealed until current controls are reconciled.
func CommitComputerSourceAbort(ctx context.Context, pool db.TxBeginner, host workergroup.HostPrincipal, env uuid.UUID, p *agentv1.ComputerSessionInstallation, observed *agentv1.ComputerSessionReceipt) error {

	identity, err := computerInstallationIdentity(p, true)
	if err != nil {
		return err
	}
	checkpoint, err := uuid.Parse(p.GetCapture().GetCheckpointId())
	if err != nil {
		return ErrInvalidInput
	}
	return hideMissing(db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		if err := lockComputerHost(ctx, tx, host); err != nil {
			return err
		}
		r, err := lockCapture(ctx, tx, env, checkpoint)
		if err != nil {
			return err
		}
		// Do not disclose continuation state to another physical owner.
		if _, err = currentComputerLease(ctx, tx, host, env, r.Computer, r.SourceEpoch); err != nil {
			return err
		}
		if r.State != "aborting" && r.State != "consumed" {
			return ErrNotReady
		}
		if err = validateSourceAbort(ctx, tx, host, env, checkpoint, r, p, false, false); err != nil {
			return err
		}
		if err = matchSourceAbort(ctx, tx, env, checkpoint, identity); err != nil {
			return err
		}
		if err = validateComputerInstallationReceipt(r, p, observed, true); err != nil {
			return err
		}
		if r.State == "consumed" {
			return nil
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

// CompleteComputerSourceAbort opens dispatch only after physical activation and
// reconciliation of every member's current durable control generation. A control
// changed while the worker was reconciling forces another reconciliation pass.
func CompleteComputerSourceAbort(ctx context.Context, pool db.TxBeginner, host workergroup.HostPrincipal, env uuid.UUID, p *agentv1.ComputerSessionInstallation, observed *agentv1.ComputerSessionReceipt) error {
	identity, err := computerInstallationIdentity(p, true)
	if err != nil {
		return err
	}
	checkpoint, err := uuid.Parse(p.GetCapture().GetCheckpointId())
	if err != nil {
		return ErrInvalidInput
	}
	return hideMissing(db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		if err := lockComputerHost(ctx, tx, host); err != nil {
			return err
		}
		r, err := lockCapture(ctx, tx, env, checkpoint)
		if err != nil {
			return err
		}
		// Do not disclose continuation state to another physical owner.
		if _, err = currentComputerLease(ctx, tx, host, env, r.Computer, r.SourceEpoch); err != nil {
			return err
		}
		if r.State != "consumed" {
			return ErrNotReady
		}
		if err = validateSourceAbort(ctx, tx, host, env, checkpoint, r, p, false, false); err != nil {
			return err
		}
		if err = matchSourceAbort(ctx, tx, env, checkpoint, identity); err != nil {
			return err
		}
		if observed.GetCheckpointId() != checkpoint.String() || observed.GetDesiredVersion() != p.GetDesiredVersion() || !observed.GetInstalled() || !observed.GetActivationStarted() || !observed.GetActivated() || observed.GetError() != "" {
			return ErrConflict
		}
		if r.ControlsReconciled {
			return nil
		}
		if err = requireComputerControlReceipts(ctx, tx, env, r); err != nil {
			return err
		}
		// The receipt reads above can outlast the lease while owner locks
		// remain held; those locks do not stop wall-clock expiry.
		if _, err = currentComputerLease(ctx, tx, host, env, r.Computer, r.SourceEpoch); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE computer_checkpoints SET controls_reconciled_at=clock_timestamp(),capture_request=NULL WHERE environment_id=$1 AND id=$2`, env, checkpoint)
		return err
	}))
}

func captureDigest(encoded []byte) []byte { digest := sha256.Sum256(encoded); return digest[:] }

func validateComputerInstallationReceipt(r captureRecord, p *agentv1.ComputerSessionInstallation, observed *agentv1.ComputerSessionReceipt, commit bool) error {
	if observed.GetCheckpointId() != p.GetCapture().GetCheckpointId() || observed.GetError() != "" || (observed.GetActivated() && !observed.GetActivationStarted()) || (observed.GetActivationStarted() && !observed.GetInstalled()) {
		return ErrConflict
	}
	if observed.GetInstalled() {
		if observed.GetDesiredVersion() != p.GetDesiredVersion() {
			return ErrConflict
		}
	} else if observed.GetDesiredVersion() != r.Version {
		return ErrConflict
	}
	if (commit || r.State == "consumed") && !observed.GetInstalled() {
		return ErrNotReady
	}
	if r.State != "consumed" && observed.GetActivationStarted() {
		return ErrConflict
	}
	return nil
}

// RecordCheckpointSaveAbsence records the owned capture pipeline's definitive
// no-cut receipt after all disk capture attempts have joined. A requested DB
// state, expired envelope, or physical fence alone is never such evidence. This
// current-source worker operation does not resolve storage uncertainty after
// owner loss; that remains with the independent publication reconciler.
func RecordCheckpointSaveAbsence(ctx context.Context, pool db.TxBeginner, host workergroup.HostPrincipal, env, checkpoint uuid.UUID, evidence string) error {
	if len(evidence) == 0 || len(evidence) > 4096 {
		return ErrInvalidInput
	}
	return hideMissing(db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		if err := lockComputerHost(ctx, tx, host); err != nil {
			return err
		}
		r, err := lockCapture(ctx, tx, env, checkpoint)
		if err != nil {
			return err
		}
		// Do not disclose continuation state to another physical owner.
		if _, err = currentComputerLease(ctx, tx, host, env, r.Computer, r.SourceEpoch); err != nil {
			return err
		}
		if r.State != "aborting" && r.State != "consumed" {
			return ErrNotReady
		}
		var state string
		var prior *string
		if err = tx.QueryRow(ctx, `SELECT status,failure_evidence FROM computer_saves WHERE environment_id=$1 AND id=$2 FOR NO KEY UPDATE`, env, r.Save).Scan(&state, &prior); err != nil {
			return err
		}
		if state == "failed" && prior != nil && *prior == evidence {
			return nil
		}
		if state != "requested" {
			return ErrConflict
		}
		_, err = tx.Exec(ctx, `UPDATE computer_saves SET status='failed',failure_evidence=$3 WHERE environment_id=$1 AND id=$2`, env, r.Save, evidence)
		return err
	}))
}
