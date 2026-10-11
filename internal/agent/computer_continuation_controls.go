package agent

import (
	"context"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	agentv1 "github.com/helmrdotdev/helmr/internal/proto/agent/v1"
	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/proto"
)

// ReadComputerContinuationControls returns current authority and effective controls
// for the already-consumed, installed physical attempt. The host applies this
// complete set in the guest before activation; it does not rebuild installation
// from a later snapshot or treat this read as a physical control acknowledgement.
func ReadComputerContinuationControls(ctx context.Context, pool db.TxBeginner, host workergroup.HostPrincipal, env uuid.UUID, p *agentv1.ComputerSessionInstallation, observed *agentv1.ComputerSessionReceipt) (*agentv1.ComputerSessionControls, error) {
	identity, err := computerInstallationIdentity(p, p.GetSourceAbort())
	if err != nil {
		return nil, err
	}
	checkpoint, err := uuid.Parse(p.GetCapture().GetCheckpointId())
	if err != nil {
		return nil, ErrInvalidInput
	}
	var result *agentv1.ComputerSessionControls
	err = db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		var r captureRecord
		var err error
		if p.GetSourceAbort() {
			if err = lockComputerHost(ctx, tx, host); err != nil {
				return err
			}
			r, err = lockCapture(ctx, tx, env, checkpoint)
			if err != nil {
				return err
			}
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
			if err = validateComputerInstallationReceipt(r, p, observed, true); err != nil {
				return err
			}
		} else {
			r, err = validateRestoreInstallation(ctx, tx, host, env, checkpoint, p, observed, true)
			if err != nil {
				return err
			}
			if err = matchRestoreInstallation(ctx, tx, env, checkpoint, identity); err != nil {
				return err
			}
		}
		if r.State != "consumed" || r.ControlsReconciled || observed.GetActivated() {
			return ErrNotReady
		}
		result = &agentv1.ComputerSessionControls{Envelope: proto.Clone(p.GetEnvelope()).(*computerv0.ComputerOperationEnvelope), CheckpointId: checkpoint.String(), DesiredVersion: p.GetDesiredVersion()}
		for _, member := range r.Members {
			control := &agentv1.SessionContinuationControl{Identity: &agentv1.SessionIdentity{SessionId: member.Session.String(), ProcessEpoch: member.Epoch}}
			if err = tx.QueryRow(ctx, `WITH RECURSIVE ancestors AS (
    SELECT id,parent_session_id FROM sessions WHERE environment_id=$1 AND id=$2
    UNION ALL SELECT s.id,s.parent_session_id FROM sessions s JOIN ancestors a ON s.id=a.parent_session_id WHERE s.environment_id=$1
   ) SELECT s.authority_generation,s.status IN ('closed','cancelled') OR d.execution_revoked_at IS NOT NULL OR EXISTS(SELECT 1 FROM computer_secret_revocations revoked WHERE revoked.environment_id=s.environment_id AND revoked.computer_id=s.computer_id) OR p.status IN ('stopping','lost','stopped'),
    EXISTS(SELECT 1 FROM session_holds h JOIN ancestors a ON a.id=h.session_id WHERE h.environment_id=$1 AND h.released_at IS NULL AND (h.session_id=$2 OR h.scope='subtree'))
   FROM sessions s JOIN deployments d ON (d.environment_id,d.id)=(s.environment_id,s.deployment_id) JOIN session_processes p ON (p.environment_id,p.session_id)=(s.environment_id,s.id) WHERE s.environment_id=$1 AND s.id=$2 AND p.epoch=$3`, env, member.Session, member.Epoch).Scan(&control.AuthorityGeneration, &control.Stopped, &control.Held); err != nil {
				return err
			}
			control.Held = control.Held || control.Stopped
			result.Sessions = append(result.Sessions, control)
		}
		var lease computerLeaseAuthority
		if p.GetSourceAbort() {
			lease, err = currentComputerLease(ctx, tx, host, env, r.Computer, r.SourceEpoch)
		} else {
			lease, err = restoreTargetLease(ctx, tx, host, env, r, *r.TargetEpoch)
		}
		if err != nil {
			return err
		}
		result.Envelope.OperationExpiresAtUnixNano = lease.Expires.UnixNano()
		if proto.Size(result) > 16*1024*1024 {
			return ErrInvalidInput
		}
		return nil
	})
	if err != nil {
		return nil, hideMissing(err)
	}
	return result, nil
}
