package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	agentv1 "github.com/helmrdotdev/helmr/internal/proto/agent/v1"
	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/proto"
)

// PrepareComputerSourceAbort supplies fresh same-owner grants only after the
// host observes that the retained guest has not installed continuation authority.
// It does not consume the checkpoint or reserve an abort: Validate arbitrates
// against restoration before any guest mutation. Installed attempts retain their
// exact request and use ordinary renewal and current-control delivery instead.
func PrepareComputerSourceAbort(ctx context.Context, pool db.TxBeginner, host workergroup.HostPrincipal, env, checkpoint uuid.UUID, epoch int64, credential string, observed *agentv1.ComputerSessionReceipt) (*agentv1.ComputerSessionInstallation, error) {
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
		lease, err := currentComputerLease(ctx, tx, host, env, r.Computer, r.SourceEpoch)
		if err != nil {
			return err
		}
		supplied := sha256.Sum256([]byte(credential))
		if epoch != r.SourceEpoch || !bytes.Equal(supplied[:], lease.CredentialDigest) {
			return ErrDenied
		}
		if r.State != "capturing" && r.State != "sealed" && r.State != "ready" && r.State != "aborting" {
			return ErrNotReady
		}
		if observed.GetCheckpointId() != checkpoint.String() || observed.GetDesiredVersion() != r.Version || observed.GetInstalled() || observed.GetActivationStarted() || observed.GetActivated() || observed.GetError() != "" {
			return ErrConflict
		}
		var raw []byte
		if err = tx.QueryRow(ctx, `SELECT capture_request FROM computer_checkpoints WHERE environment_id=$1 AND id=$2`, env, checkpoint).Scan(&raw); err != nil {
			return err
		}
		capture := new(agentv1.ComputerSessionCapture)
		if err = proto.Unmarshal(raw, capture); err != nil {
			return err
		}
		result = &agentv1.ComputerSessionInstallation{Capture: capture, Envelope: &computerv0.ComputerOperationEnvelope{ComputerId: r.Computer.String(), ComputerInstanceId: lease.Instance.String(), WriterGeneration: uint64(epoch), ChannelCredential: credential, OperationId: checkpoint.String(), OperationExpiresAtUnixNano: lease.Expires.UnixNano()}, DesiredVersion: r.Version + 1, SourceAbort: true, BaseComputerDiskVersionId: lease.BaseVersion}
		for _, member := range r.Members {
			var generation int64
			var terminal bool
			if err = tx.QueryRow(ctx, `SELECT s.authority_generation,s.status IN ('closed','cancelled') OR d.execution_revoked_at IS NOT NULL OR EXISTS(SELECT 1 FROM computer_secret_revocations revoked WHERE revoked.environment_id=s.environment_id AND revoked.computer_id=s.computer_id) OR p.status IN ('stopping','lost','stopped') FROM sessions s JOIN deployments d ON (d.environment_id,d.id)=(s.environment_id,s.deployment_id) JOIN session_processes p ON (p.environment_id,p.session_id)=(s.environment_id,s.id) WHERE s.environment_id=$1 AND s.id=$2 AND p.epoch=$3`, env, member.Session, member.Epoch).Scan(&generation, &terminal); err != nil {
				return err
			}
			identity := &agentv1.SessionIdentity{SessionId: member.Session.String(), ProcessEpoch: member.Epoch}
			result.Grants = append(result.Grants, &agentv1.SessionGrant{Identity: identity, ComputerId: r.Computer.String(), ComputerInstanceId: lease.Instance.String(), WriterGeneration: epoch, ComputerLeaseEpoch: epoch, WorkerHostId: host.HostID.String(), AuthorityGeneration: generation, ExpiresAtUnixNano: lease.Expires.UnixNano(), ChannelCredential: credential})
			if terminal {
				result.StoppedSessions = append(result.StoppedSessions, proto.Clone(identity).(*agentv1.SessionIdentity))
			}
		}
		// Reuse the same complete membership, process-fence and current-owner checks
		// as validation, including a final wall-clock deadline check.
		if err = validateSourceAbort(ctx, tx, host, env, checkpoint, r, result, true, true); err != nil {
			return err
		}
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
