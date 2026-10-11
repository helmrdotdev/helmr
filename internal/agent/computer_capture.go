package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"slices"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	agentv1 "github.com/helmrdotdev/helmr/internal/proto/agent/v1"
	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/proto"
)

// ComputerCaptureRequest is supplied only by authenticated worker transport.
// The channel credential is checked against the lease and never logged.
type ComputerCaptureRequest struct {
	EnvironmentID     uuid.UUID
	ComputerID        uuid.UUID
	CheckpointID      uuid.UUID
	LeaseEpoch        int64
	ChannelCredential string
}

type ComputerCapture struct {
	// Request is the exact secret protobuf retained by the guest. Transports carry
	// these bytes unchanged; rebuilding an envelope would break uncertain retries.
	Request []byte
	SaveID  uuid.UUID
}

type computerMember struct {
	Session    uuid.UUID
	Epoch      int64
	Status     string
	LeaseEpoch int64
}

// BeginComputerCapture seals dispatch and allocates the coherent disk save in
// one transaction. The guest must still qualify and freeze every resident; an
// idle database alone is not capture evidence. Public input may continue queuing.
func BeginComputerCapture(ctx context.Context, pool db.TxBeginner, host workergroup.HostPrincipal, req ComputerCaptureRequest) (ComputerCapture, error) {
	var result ComputerCapture
	if req.EnvironmentID == uuid.Nil() || req.ComputerID == uuid.Nil() || req.CheckpointID == uuid.Nil() || req.LeaseEpoch <= 0 || len(req.ChannelCredential) == 0 || len(req.ChannelCredential) > 4096 {
		return result, ErrInvalidInput
	}
	err := db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		if err := lockComputerHost(ctx, tx, host); err != nil {
			return err
		}
		members, err := lockComputerMembers(ctx, tx, req.EnvironmentID, req.ComputerID)
		if err != nil {
			return err
		}
		lease, err := currentComputerLease(ctx, tx, host, req.EnvironmentID, req.ComputerID, req.LeaseEpoch)
		if err != nil {
			return err
		}
		supplied := sha256.Sum256([]byte(req.ChannelCredential))
		if !bytes.Equal(lease.CredentialDigest, supplied[:]) {
			return ErrDenied
		}
		if err = requireComputerImageAllowed(ctx, tx, req.EnvironmentID, req.ComputerID); err != nil {
			return err
		}
		var priorComputer uuid.UUID
		var priorEpoch int64
		var priorState string
		err = tx.QueryRow(ctx, `SELECT computer_id,source_lease_epoch,status,capture_request,disk_save_id FROM computer_checkpoints WHERE environment_id=$1 AND id=$2`, req.EnvironmentID, req.CheckpointID).Scan(&priorComputer, &priorEpoch, &priorState, &result.Request, &result.SaveID)
		if err == nil {
			if priorComputer != req.ComputerID || priorEpoch != req.LeaseEpoch {
				return ErrConflict
			}
			if priorState != "capturing" && priorState != "sealed" && priorState != "ready" {
				return ErrNotReady
			}
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if err = computerDispatchAvailable(ctx, tx, req.EnvironmentID, req.ComputerID); err != nil {
			return err
		}
		if len(members) == 0 {
			return ErrNotReady
		}
		for _, m := range members {
			if m.Status != "ready" || m.LeaseEpoch != req.LeaseEpoch {
				return ErrNotReady
			}
		}
		var busy bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM turns WHERE environment_id=$1 AND computer_id=$2 AND status IN ('running','finalizing'))
   OR EXISTS(SELECT 1 FROM sessions s JOIN turns t ON (t.environment_id,t.session_id)=(s.environment_id,s.id)
     WHERE s.environment_id=$1 AND s.computer_id=$2 AND s.status IN ('open','closing') AND t.status='queued' AND `+noSessionHoldsSQL+`)
   OR EXISTS(SELECT 1 FROM computer_commands WHERE environment_id=$1 AND computer_id=$2 AND status='pending')
   OR EXISTS(SELECT 1 FROM computer_saves WHERE environment_id=$1 AND computer_id=$2 AND status IN ('requested','captured'))
   OR EXISTS(SELECT 1 FROM computer_commands WHERE environment_id=$1 AND computer_id=$2 AND computer_lease_epoch IS NOT NULL AND process_reconciled_at IS NULL)
   OR EXISTS(SELECT 1 FROM sessions s JOIN deployments d ON (d.environment_id,d.id)=(s.environment_id,s.deployment_id)
     JOIN session_processes p ON (p.environment_id,p.session_id)=(s.environment_id,s.id)
     WHERE s.environment_id=$1 AND s.computer_id=$2 AND p.fenced_at IS NULL AND (s.status IN ('closed','cancelled') OR d.execution_revoked_at IS NOT NULL OR EXISTS(SELECT 1 FROM computer_secret_revocations revoked WHERE revoked.environment_id=s.environment_id AND revoked.computer_id=s.computer_id)))`, req.EnvironmentID, req.ComputerID).Scan(&busy); err != nil {
			return err
		}
		if busy {
			return ErrNotReady
		}
		// The Group share lock acquired with Host authority excludes Pool
		// retirement while a new checkpoint reserves its restore profile.
		var supplier bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(
 SELECT 1 FROM computer_leases l JOIN worker_groups g ON g.id=$4 AND g.status='active'
 JOIN worker_pools p ON p.worker_group_id=g.id AND p.status='active'
  AND p.vm_platform_id=l.vm_platform_id AND p.per_vm_cpu_millis>=l.reserved_cpu_millis
  AND p.per_vm_memory_bytes>=l.reserved_memory_bytes AND p.per_vm_guest_ephemeral_disk_bytes>=l.reserved_scratch_bytes
 JOIN worker_pool_cpu_shapes shape ON shape.worker_pool_id=p.id AND shape.vcpu_count=l.vm_vcpu_count AND shape.cpu_config_digest=l.cpu_config_digest
 WHERE l.environment_id=$1 AND l.computer_id=$2 AND l.epoch=$3)
 `, req.EnvironmentID, req.ComputerID, req.LeaseEpoch, host.GroupID).Scan(&supplier); err != nil {
			return err
		}
		if !supplier {
			return ErrNotReady
		}
		var version, saveSeq int64
		if err = tx.QueryRow(ctx, `UPDATE computers SET next_control_version=next_control_version+1,next_save_seq=next_save_seq+1
   WHERE environment_id=$1 AND id=$2 AND integrity_fault_at IS NULL AND deleted_at IS NULL
   RETURNING next_control_version-1,next_save_seq-1`, req.EnvironmentID, req.ComputerID).Scan(&version, &saveSeq); err != nil {
			return err
		}
		request := &agentv1.ComputerSessionCapture{CheckpointId: req.CheckpointID.String(), DesiredVersion: version, MembershipRevision: version,
			Envelope: &computerv0.ComputerOperationEnvelope{OperationId: req.CheckpointID.String(), ComputerId: req.ComputerID.String(), ComputerInstanceId: lease.Instance.String(), WriterGeneration: uint64(req.LeaseEpoch), ChannelCredential: req.ChannelCredential, OperationExpiresAtUnixNano: lease.Expires.UnixNano()},
		}
		for _, m := range members {
			request.Sessions = append(request.Sessions, &agentv1.SessionIdentity{SessionId: m.Session.String(), ProcessEpoch: m.Epoch})
		}
		if proto.Size(request) > 16*1024*1024 {
			return ErrInvalidInput
		}
		result.Request, err = (proto.MarshalOptions{Deterministic: true}).Marshal(request)
		if err != nil {
			return err
		}
		result.SaveID = uuid.NewV7()
		if _, err = tx.Exec(ctx, `INSERT INTO computer_saves(environment_id,id,computer_id,computer_lease_epoch,seq) VALUES($1,$2,$3,$4,$5)`, req.EnvironmentID, result.SaveID, req.ComputerID, req.LeaseEpoch, saveSeq); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `INSERT INTO computer_checkpoints(environment_id,id,computer_id,source_lease_epoch,control_version,disk_save_id,status,capture_request,capture_expires_at,capture_digest)
   VALUES($1,$2,$3,$4,$5,$6,'capturing',$7,$8,$9)`, req.EnvironmentID, req.CheckpointID, req.ComputerID, req.LeaseEpoch, version, result.SaveID, result.Request, lease.Expires, captureDigest(result.Request)); err != nil {
			return err
		}
		for _, m := range members {
			if _, err = tx.Exec(ctx, `INSERT INTO computer_checkpoint_members(environment_id,computer_id,checkpoint_id,session_id,process_epoch) VALUES($1,$2,$3,$4,$5)`, req.EnvironmentID, req.ComputerID, req.CheckpointID, m.Session, m.Epoch); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return ComputerCapture{}, hideMissing(err)
	}
	return result, nil
}

func lockComputerHost(ctx context.Context, tx pgx.Tx, host workergroup.HostPrincipal) error {
	return lockRuntimeHost(ctx, tx, Caller{Host: &host, Execution: Execution{WorkerHostID: host.HostID, WorkerEpoch: host.Epoch}})
}

// Membership discovery precedes ownership-root locks. Re-read under the Computer
// lock: an admission that added an unseen root makes this attempt ineligible.
// Process admissions must take the same Computer lock and reject pending checkpoints.
func lockComputerMembers(ctx context.Context, tx pgx.Tx, env, computer uuid.UUID) ([]computerMember, error) {
	return lockComputerCheckpointMembers(ctx, tx, env, computer, uuid.Nil())
}

// A failed restore must lock immutable captured members, including already fenced
// members, before taking the Computer lock. Late Session locking reverses the
// shared ownership-tree lock order.
func lockComputerCheckpointMembers(ctx context.Context, tx pgx.Tx, env, computer, checkpoint uuid.UUID) ([]computerMember, error) {
	read := func() ([]computerMember, error) {
		rows, err := tx.Query(ctx, `SELECT p.session_id,p.epoch,p.status,p.computer_lease_epoch FROM session_processes p WHERE p.environment_id=$1 AND p.computer_id=$2
 AND (p.fenced_at IS NULL OR EXISTS(SELECT 1 FROM computer_checkpoint_members m WHERE m.environment_id=p.environment_id AND m.computer_id=p.computer_id AND m.checkpoint_id=$3 AND m.session_id=p.session_id AND m.process_epoch=p.epoch)) ORDER BY p.session_id,p.epoch`, env, computer, checkpoint)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var result []computerMember
		for rows.Next() {
			var m computerMember
			if err := rows.Scan(&m.Session, &m.Epoch, &m.Status, &m.LeaseEpoch); err != nil {
				return nil, err
			}
			result = append(result, m)
		}
		return result, rows.Err()
	}
	before, err := read()
	if err != nil {
		return nil, err
	}
	ids := make([]uuid.UUID, len(before))
	for i, m := range before {
		ids[i] = m.Session
	}
	if _, err = lockSessions(ctx, tx, env, ids); err != nil {
		return nil, err
	}
	// lockSessions has no Computer to lock when no resident is present.
	if len(ids) == 0 {
		var id uuid.UUID
		if err = tx.QueryRow(ctx, `SELECT id FROM computers WHERE environment_id=$1 AND id=$2 FOR NO KEY UPDATE`, env, computer).Scan(&id); err != nil {
			return nil, err
		}
	}
	after, err := read()
	if err != nil {
		return nil, err
	}
	if !slices.EqualFunc(before, after, func(a, b computerMember) bool {
		return a.Session == b.Session && a.Epoch == b.Epoch && a.LeaseEpoch == b.LeaseEpoch
	}) {
		return nil, ErrNotReady
	}
	return after, nil
}

type computerLeaseAuthority struct {
	Instance         uuid.UUID
	CredentialDigest []byte
	BaseVersion      string
	Expires          time.Time
}

func currentComputerLease(ctx context.Context, tx pgx.Tx, host workergroup.HostPrincipal, env, computer uuid.UUID, epoch int64) (computerLeaseAuthority, error) {
	var lease computerLeaseAuthority
	err := tx.QueryRow(ctx, `SELECT l.computer_instance_id,l.channel_credential_digest,COALESCE(l.restored_from_save_id::text,'sha256:'||encode(c.initial_root_digest,'hex')),LEAST(l.expires_at,clock_timestamp()+interval '30 seconds')
  FROM computer_leases l JOIN computers c ON (c.environment_id,c.id)=(l.environment_id,l.computer_id)
  WHERE l.environment_id=$1 AND l.computer_id=$2 AND l.epoch=$3 AND l.worker_host_id=$4 AND l.worker_epoch=$5
   AND l.status='active' AND l.fenced_at IS NULL AND l.expires_at>clock_timestamp()`, env, computer, epoch, host.HostID, host.Epoch).Scan(&lease.Instance, &lease.CredentialDigest, &lease.BaseVersion, &lease.Expires)
	return lease, err
}

func computerDispatchAvailable(ctx context.Context, tx pgx.Tx, env, computer uuid.UUID) error {
	var sealed bool
	err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM computer_secret_revocations WHERE environment_id=$1 AND computer_id=$2) OR EXISTS(SELECT 1 FROM computer_checkpoints WHERE environment_id=$1 AND computer_id=$2 AND (status IN ('capturing','sealed','ready','restoring','aborting') OR (status='consumed' AND controls_reconciled_at IS NULL)))`, env, computer).Scan(&sealed)
	if err != nil {
		return err
	}
	if sealed {
		return ErrNotReady
	}
	return nil
}
