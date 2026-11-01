package session

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"slices"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/computer"

	"github.com/helmrdotdev/helmr/internal/db"
	agentv1 "github.com/helmrdotdev/helmr/internal/proto/agent/v1"
	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/proto"
)

// UnusedComputerHibernateAfter is how long a Computer with no open Session and
// no unsettled Command stays running before it hibernates.
const UnusedComputerHibernateAfter = 10 * time.Minute

// captureAllowedSQL decides whether a Computer may hibernate now. An accepted
// hibernate request always allows it. Otherwise, while no wake is pending:
//   - with open Sessions, every one of them must allow automatic hibernation
//     and have been idle for its own period. A Session's clock is the latest
//     of its idle clock, the last Command settlement on the Computer and the
//     last wake request. A closing Session always waits for its close to
//     finish;
//   - with no such Session and no unsettled Command, the Computer must have
//     been unused for UnusedComputerHibernateAfter since its last Session closed,
//     its last Command settled, its last wake request or its creation.
//
// Work in progress (running Turns, runnable queued Turns, unsettled Commands
// and saves) is rejected separately by the busy check. Internal saves never
// move any clock.
const captureAllowedSQL = `WITH c AS (SELECT
  (SELECT max(terminal_at) FROM commands WHERE environment_id=$1 AND computer_id=$2) AS settled,
  (SELECT max(accepted_at) FROM computer_controls WHERE environment_id=$1 AND computer_id=$2 AND kind='wake') AS woke)
SELECT
 EXISTS(SELECT 1 FROM computer_controls WHERE environment_id=$1 AND computer_id=$2 AND kind='hibernate' AND status='accepted')
 OR (NOT EXISTS(SELECT 1 FROM computer_controls WHERE environment_id=$1 AND computer_id=$2 AND kind='wake' AND status='accepted')
 AND CASE WHEN EXISTS(SELECT 1 FROM sessions WHERE environment_id=$1 AND computer_id=$2 AND status IN ('open','closing'))
  THEN NOT EXISTS(SELECT 1 FROM sessions s JOIN agent_definitions d ON (d.environment_id,d.agent_id,d.deployment_id)=(s.environment_id,s.agent_id,s.deployment_id)
   WHERE s.environment_id=$1 AND s.computer_id=$2 AND s.status IN ('open','closing')
   AND (s.status='closing' OR d.hibernate_after_idle_ms IS NULL OR s.idle_since IS NULL
    OR GREATEST(s.idle_since,c.settled,c.woke)+d.hibernate_after_idle_ms*interval '1 millisecond'>clock_timestamp()))
  ELSE NOT EXISTS(SELECT 1 FROM commands WHERE environment_id=$1 AND computer_id=$2 AND terminal_at IS NULL)
   AND GREATEST(
    (SELECT created_at FROM computers WHERE environment_id=$1 AND id=$2),
    (SELECT max(e.created_at) FROM sessions s JOIN session_events e ON (e.environment_id,e.session_id)=(s.environment_id,s.id) WHERE s.environment_id=$1 AND s.computer_id=$2 AND e.kind='session.closed'),
    c.settled,c.woke)+$3::bigint*interval '1 millisecond'<=clock_timestamp()
  END)
FROM c`

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
	Request       []byte
	SaveID        uuid.UUID
	WorkPending   bool
	ReleaseSource bool
}

type ComputerMember struct {
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
		if err := computer.LockHost(ctx, tx, host); err != nil {
			return err
		}
		members, err := LockComputerMembers(ctx, tx, req.EnvironmentID, req.ComputerID)
		if err != nil {
			return err
		}
		lease, err := computer.CurrentLease(ctx, tx, host, req.EnvironmentID, req.ComputerID, req.LeaseEpoch)
		if err != nil {
			return err
		}
		supplied := sha256.Sum256([]byte(req.ChannelCredential))
		if !bytes.Equal(lease.CredentialDigest, supplied[:]) {
			return ErrDenied
		}
		var priorComputer uuid.UUID
		var priorEpoch int64
		var priorState string
		err = tx.QueryRow(ctx, `SELECT computer_id,source_lease_epoch,status,capture_request,disk_save_id FROM checkpoints WHERE environment_id=$1 AND id=$2`, req.EnvironmentID, req.CheckpointID).Scan(&priorComputer, &priorEpoch, &priorState, &result.Request, &result.SaveID)
		if err == nil {
			if priorComputer != req.ComputerID || priorEpoch != req.LeaseEpoch {
				return ErrConflict
			}
			if priorState != "capturing" && priorState != "sealed" && priorState != "ready" {
				return ErrNotReady
			}
			result.WorkPending, err = computerExecutionDemand(ctx, tx, req.EnvironmentID, req.ComputerID)
			if err == nil && priorState == "ready" && result.WorkPending {
				// Terminal-member cleanup must not resume unrelated sleeping work.
				// The published cut survives releasing its optional physical source.
				err = tx.QueryRow(ctx, `SELECT `+computer.RunnableExecutionDemandSQL+` FROM computers c WHERE environment_id=$1 AND id=$2`, req.EnvironmentID, req.ComputerID).Scan(&result.WorkPending)
				result.ReleaseSource = err == nil && !result.WorkPending
			}
			return err
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if err = computer.DispatchAvailable(ctx, tx, req.EnvironmentID, req.ComputerID); err != nil {
			return err
		}
		var permitted bool
		if err = tx.QueryRow(ctx, captureAllowedSQL, req.EnvironmentID, req.ComputerID, UnusedComputerHibernateAfter.Milliseconds()).Scan(&permitted); err != nil {
			return err
		}
		if !permitted {
			return ErrNotReady
		}
		for _, m := range members {
			if m.Status != "ready" || m.LeaseEpoch != req.LeaseEpoch {
				return ErrNotReady
			}
		}
		var busy bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM turns WHERE environment_id=$1 AND computer_id=$2 AND status IN ('running'))
   OR EXISTS(SELECT 1 FROM sessions s JOIN turns t ON (t.environment_id,t.session_id)=(s.environment_id,s.id)
     WHERE s.environment_id=$1 AND s.computer_id=$2 AND s.status IN ('open','closing') AND t.status='queued' AND `+db.NoSessionHoldsSQL+`)
   OR EXISTS(SELECT 1 FROM commands WHERE environment_id=$1 AND computer_id=$2 AND status='pending')
   OR EXISTS(SELECT 1 FROM computer_saves WHERE environment_id=$1 AND computer_id=$2 AND status IN ('requested','captured'))
   OR EXISTS(SELECT 1 FROM commands WHERE environment_id=$1 AND computer_id=$2 AND computer_lease_epoch IS NOT NULL AND process_reconciled_at IS NULL)
   OR EXISTS(SELECT 1 FROM sessions s
     JOIN session_processes p ON (p.environment_id,p.session_id)=(s.environment_id,s.id)
     WHERE s.environment_id=$1 AND s.computer_id=$2 AND p.fenced_at IS NULL AND (s.status='closed'))`, req.EnvironmentID, req.ComputerID).Scan(&busy); err != nil {
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
		version, saveSeq, err := computer.ReserveCapture(ctx, tx, req.EnvironmentID, req.ComputerID)
		if err != nil {
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
		start := computer.CheckpointStart{EnvironmentID: req.EnvironmentID, ComputerID: req.ComputerID, CheckpointID: req.CheckpointID, SaveID: result.SaveID,
			LeaseEpoch: req.LeaseEpoch, ControlVersion: version, SaveSeq: saveSeq, Request: result.Request, RequestDigest: captureDigest(result.Request), Expires: lease.Expires}
		for _, m := range members {
			start.Members = append(start.Members, computer.CheckpointMember{Session: m.Session, Epoch: m.Epoch})
		}
		return computer.CreateCheckpoint(ctx, tx, start)
	})
	if err != nil {
		return ComputerCapture{}, hideMissing(err)
	}
	return result, nil
}

// Membership discovery precedes ownership-root locks. Re-read under the Computer
// lock: an admission that added an unseen root makes this attempt ineligible.
// Process admissions must take the same Computer lock and reject pending checkpoints.
func LockComputerMembers(ctx context.Context, tx pgx.Tx, env, computerID uuid.UUID) ([]ComputerMember, error) {
	return LockCheckpointMembers(ctx, tx, env, computerID, uuid.Nil())
}

// A failed restore must lock immutable captured members, including already fenced
// members, before taking the Computer lock. Late Session locking reverses the
// shared ownership-tree lock order.
func LockCheckpointMembers(ctx context.Context, tx pgx.Tx, env, computerID, checkpoint uuid.UUID) ([]ComputerMember, error) {
	read := func() ([]ComputerMember, error) {
		rows, err := tx.Query(ctx, `SELECT p.session_id,p.epoch,p.status,p.computer_lease_epoch FROM session_processes p WHERE p.environment_id=$1 AND p.computer_id=$2
 AND (p.fenced_at IS NULL OR EXISTS(SELECT 1 FROM checkpoint_processes m WHERE m.environment_id=p.environment_id AND m.computer_id=p.computer_id AND m.checkpoint_id=$3 AND m.session_id=p.session_id AND m.process_epoch=p.epoch)) ORDER BY p.session_id,p.epoch`, env, computerID, checkpoint)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var result []ComputerMember
		for rows.Next() {
			var m ComputerMember
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
	rows, err := tx.Query(ctx, `SELECT id FROM sessions WHERE environment_id=$1 AND computer_id=$2 AND status IN ('open','closing') ORDER BY id`, env, computerID)
	if err != nil {
		return nil, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return nil, err
	}
	known := append([]uuid.UUID(nil), ids...)
	for _, m := range before {
		ids = append(ids, m.Session)
	}
	if _, err = LockSessionsAndComputers(ctx, tx, env, ids, []uuid.UUID{computerID}); err != nil {
		return nil, err
	}
	var changed bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM sessions WHERE environment_id=$1 AND computer_id=$2 AND status IN ('open','closing') AND NOT(id=ANY($3::uuid[])))`, env, computerID, known).Scan(&changed); err != nil {
		return nil, err
	}
	if changed {
		return nil, ErrNotReady
	}
	after, err := read()
	if err != nil {
		return nil, err
	}
	if !slices.EqualFunc(before, after, func(a, b ComputerMember) bool {
		return a.Session == b.Session && a.Epoch == b.Epoch && a.LeaseEpoch == b.LeaseEpoch
	}) {
		return nil, ErrNotReady
	}
	return after, nil
}

// Demand is read under the same Computer/member locks as capture. Notifications
// only prompt this read; they cannot grant execution or cancel a sealed cut.
func computerExecutionDemand(ctx context.Context, tx pgx.Tx, env, computerID uuid.UUID) (bool, error) {
	var pending bool
	err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM computer_controls WHERE environment_id=$1 AND computer_id=$2 AND kind='wake' AND status='accepted')
 OR EXISTS(SELECT 1 FROM turns WHERE environment_id=$1 AND computer_id=$2 AND status='running')
 OR EXISTS(SELECT 1 FROM sessions s JOIN turns t ON (t.environment_id,t.session_id)=(s.environment_id,s.id)
 WHERE s.environment_id=$1 AND s.computer_id=$2 AND s.status IN ('open','closing') AND t.status='queued' AND `+db.NoSessionHoldsSQL+`)
 OR EXISTS(SELECT 1 FROM commands WHERE environment_id=$1 AND computer_id=$2 AND (status='pending' OR (computer_lease_epoch IS NOT NULL AND process_reconciled_at IS NULL)))
 OR EXISTS(SELECT 1 FROM sessions s JOIN session_processes p ON (p.environment_id,p.session_id)=(s.environment_id,s.id)
 WHERE s.environment_id=$1 AND s.computer_id=$2 AND p.fenced_at IS NULL AND (s.status='closed'))`, env, computerID).Scan(&pending)
	return pending, err
}

var ErrComputerCaptureDemand = errors.New("computer has pending execution during capture")
