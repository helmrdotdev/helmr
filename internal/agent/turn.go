// Package agent owns Agent admission, Session control and Turn outcome transactions.
package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/conversation"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/jsoncanon"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

var (
	ErrInvalidInput = errors.New("invalid operation input")
	ErrDenied       = errors.New("operation not authorized")
	ErrConflict     = errors.New("request identity conflicts with its original request")
	ErrNotReady     = errors.New("execution is not ready")
	ErrTerminal     = errors.New("turn is terminal")
)

// Caller is transport-authenticated identity. Runtime requests carry the stable
// Session and its current authority generation; a transport request ID is not identity.
type Caller struct {
	Kind string
	ID   uuid.UUID
	// TurnID is present only for direct Turn-scoped native requests. MCP uses
	// Session authority without guessing a currently running Turn.
	TurnID    uuid.UUID
	Execution Execution
	// Host is supplied by authenticated worker transport, never request JSON.
	Host *workergroup.HostPrincipal
}

type EnqueueRequest struct {
	EnvironmentID uuid.UUID
	SessionID     uuid.UUID
	RetryKey      string
	Input         json.RawMessage
}

type Admission struct {
	SessionID uuid.UUID
	TurnID    uuid.UUID
	Sequence  int64
	Created   bool
}

type owners struct {
	root       uuid.UUID
	computer   uuid.UUID
	lifecycle  string
	generation int64
}

// lockSession follows the ownership-root -> Computer -> Session order. Ownership
// and placement are immutable; deciding state is read again after locking.
func lockSession(ctx context.Context, tx pgx.Tx, env, session uuid.UUID) (owners, error) {
	rows, err := lockSessions(ctx, tx, env, []uuid.UUID{session})
	return rows[session], err
}

func lockSessions(ctx context.Context, tx pgx.Tx, env uuid.UUID, ids []uuid.UUID) (map[uuid.UUID]owners, error) {
	return lockSessionsAndComputers(ctx, tx, env, ids, nil)
}

func lockSessionsAndComputers(ctx context.Context, tx pgx.Tx, env uuid.UUID, ids, additionalComputers []uuid.UUID) (map[uuid.UUID]owners, error) {
	rows := make(map[uuid.UUID]owners, len(ids))
	roots, computers := make(map[uuid.UUID]bool), make(map[uuid.UUID]bool)
	for _, id := range additionalComputers {
		computers[id] = true
	}
	for _, id := range ids {
		var o owners
		if err := tx.QueryRow(ctx, `SELECT root_session_id,computer_id FROM sessions WHERE environment_id=$1 AND id=$2`, env, id).Scan(&o.root, &o.computer); err != nil {
			return nil, err
		}
		rows[id] = o
		roots[o.root] = true
		computers[o.computer] = true
	}
	sorted := func(set map[uuid.UUID]bool) []uuid.UUID {
		ids := make([]uuid.UUID, 0, len(set))
		for id := range set {
			ids = append(ids, id)
		}
		slices.SortFunc(ids, func(a, b uuid.UUID) int { return bytes.Compare(a[:], b[:]) })
		return ids
	}
	for _, id := range sorted(roots) {
		if _, err := tx.Exec(ctx, `SELECT id FROM sessions WHERE environment_id=$1 AND id=$2 FOR NO KEY UPDATE`, env, id); err != nil {
			return nil, err
		}
	}
	for _, id := range sorted(computers) {
		if _, err := tx.Exec(ctx, `SELECT id FROM computers WHERE environment_id=$1 AND id=$2 FOR NO KEY UPDATE`, env, id); err != nil {
			return nil, err
		}
	}
	sessionSet := make(map[uuid.UUID]bool)
	for id := range rows {
		sessionSet[id] = true
	}
	for _, id := range sorted(sessionSet) {
		o := rows[id]
		if err := tx.QueryRow(ctx, `SELECT status,authority_generation FROM sessions WHERE environment_id=$1 AND id=$2 FOR NO KEY UPDATE`, env, id).Scan(&o.lifecycle, &o.generation); err != nil {
			return nil, err
		}
		rows[id] = o
	}
	return rows, nil
}

func event(ctx context.Context, tx pgx.Tx, env, session, turn uuid.UUID, kind string) error {
	return eventData(ctx, tx, env, session, turn, kind, json.RawMessage(`{}`))
}
func eventData(ctx context.Context, tx pgx.Tx, env, session, turn uuid.UUID, kind string, data json.RawMessage) error {
	_, err := tx.Exec(ctx, `WITH allocated AS (
        UPDATE sessions SET next_event_seq=next_event_seq+1 WHERE environment_id=$1 AND id=$2 RETURNING next_event_seq-1 AS seq
    ) INSERT INTO session_events(environment_id,session_id,seq,turn_id,kind,data)
      SELECT $1,$2,seq,$3,$4,$5 FROM allocated`, env, session, nullableID(turn), kind, data)
	return err
}

func digestJSON(input json.RawMessage) ([32]byte, error) {
	canonical, err := jsoncanon.Transform(input)
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(canonical), nil
}

// Enqueue atomically accepts one input and records its observation event. The
// Environment lock serializes capacity/admission; the Session lock fixes queue
// order and the model-owned retry receipt before live eligibility is considered.
func Enqueue(ctx context.Context, pool db.TxBeginner, caller Caller, req EnqueueRequest) (Admission, error) {
	result, err := admitSessionInput(ctx, pool, caller, req, "enqueue", uuid.Nil())
	return result.Admission, err
}

func admitSessionInput(ctx context.Context, pool db.TxBeginner, caller Caller, req EnqueueRequest, method string, exactTurn uuid.UUID) (SendReceipt, error) {
	var result SendReceipt
	target := req.SessionID
	if method == "turn_send" {
		target = exactTurn
	}
	digest, err := digestJSON(req.Input)
	if err != nil {
		return result, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	if len(req.RetryKey) > 512 {
		return result, ErrInvalidInput
	}
	if caller.ID == uuid.Nil() {
		return result, ErrDenied
	}
	err = db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		if caller.Kind == "session" {
			if err := lockRuntimeHost(ctx, tx, caller); err != nil {
				return err
			}
		}
		if caller.Kind != "session" {
			if err := authorizeEnqueue(ctx, tx, caller, req.EnvironmentID, owners{}); err != nil {
				return err
			}
		} else if caller.Execution.EnvironmentID != req.EnvironmentID || caller.Execution.SessionID != caller.ID {
			return ErrDenied
		}
		if caller.Kind == "session" && method == "turn_send" {
			if err := requireOwnedTarget(ctx, tx, req.EnvironmentID, caller.ID, req.SessionID); err != nil {
				return err
			}
		}
		policy, err := lockAdmissionEnvironment(ctx, tx, req.EnvironmentID)
		if err != nil {
			return err
		}
		ids := []uuid.UUID{req.SessionID}
		if caller.Kind == "session" {
			ids = append(ids, caller.ID)
		}
		locked, err := lockSessions(ctx, tx, req.EnvironmentID, ids)
		if err != nil {
			return err
		}
		o := locked[req.SessionID]
		if caller.Kind == "session" {
			// Historical acceptance needs current physical and generation
			// authority, but does not reopen the original processing admission.
			if err = executionReceipt(ctx, tx, caller.Execution, locked[caller.ID]); err != nil {
				return err
			}
		} else if err = authorizeEnqueue(ctx, tx, caller, req.EnvironmentID, locked[caller.ID]); err != nil {
			return err
		}
		if req.RetryKey != "" {
			var prior []byte
			err = tx.QueryRow(ctx, `SELECT session_id,id,seq,request_digest FROM turns
              WHERE environment_id=$1 AND caller_kind=$2 AND caller_id=$3 AND admission_method=$6 AND target_id=$4 AND retry_key=$5`, req.EnvironmentID, caller.Kind, caller.ID, target, req.RetryKey, method).Scan(&result.SessionID, &result.TurnID, &result.Sequence, &prior)
			if err == nil {
				if !bytes.Equal(prior, digest[:]) {
					return ErrConflict
				}
				return nil
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
		}
		if method != "enqueue" && req.RetryKey != "" {
			found, err := messageRetry(ctx, tx, caller, req, method, target, digest, &result)
			if err != nil || found {
				return err
			}
		}
		if caller.Kind == "session" {
			if err = authorizeSessionInput(ctx, tx, caller, req.EnvironmentID, locked[caller.ID], method != "enqueue"); err != nil {
				return err
			}
		}
		req.Input, err = conversation.Input(req.Input)
		if err != nil {
			return err
		}
		if o.lifecycle != "open" && o.lifecycle != "closing" {
			return ErrNotReady
		}
		var computerAvailable bool
		if err = tx.QueryRow(ctx, `SELECT preparation_failed_at IS NULL AND deleted_at IS NULL AND NOT EXISTS(SELECT 1 FROM computer_secret_revocations WHERE environment_id=$1 AND computer_id=$2) FROM computers WHERE environment_id=$1 AND id=$2`, req.EnvironmentID, o.computer).Scan(&computerAvailable); err != nil {
			return err
		}
		if !computerAvailable {
			return ErrNotReady
		}
		if method != "enqueue" {
			admitted, err := admitMessage(ctx, tx, caller, req, method, exactTurn, digest, o, &result)
			if err != nil || admitted {
				return err
			}
			if method == "turn_send" {
				return ErrMessageClosed
			}
		}
		if o.lifecycle != "open" {
			return ErrNotReady
		}
		if err = authorizeEnqueue(ctx, tx, caller, req.EnvironmentID, locked[caller.ID]); err != nil {
			return err
		}
		if err = consumeAdmission(ctx, tx, req.EnvironmentID, policy); err != nil {
			return err
		}
		result.SessionID = req.SessionID
		result.TurnID = uuid.NewV7()
		if err = tx.QueryRow(ctx, `UPDATE sessions SET next_turn_seq=next_turn_seq+1 WHERE environment_id=$1 AND id=$2 RETURNING next_turn_seq-1`, req.EnvironmentID, req.SessionID).Scan(&result.Sequence); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO turns(environment_id,id,session_id,computer_id,seq,caller_kind,caller_id,admission_method,target_id,retry_key,request_digest,input,origin_turn_id)
            VALUES($1,$2,$3,$4,$5,$6,$7,$12,$3,NULLIF($8,''),$9,$10,$11)`, req.EnvironmentID, result.TurnID, req.SessionID, o.computer, result.Sequence, caller.Kind, caller.ID, req.RetryKey, digest[:], req.Input, nullableID(caller.TurnID), method)
		if err != nil {
			return err
		}
		return event(ctx, tx, req.EnvironmentID, req.SessionID, result.TurnID, "turn.queued")
	})
	if err != nil {
		var invalid *pgconn.PgError
		if errors.As(err, &invalid) && strings.HasPrefix(invalid.Code, "22") {
			return SendReceipt{}, fmt.Errorf("%w: input cannot be stored", ErrInvalidInput)
		}
		return SendReceipt{}, hideMissing(err)
	}
	return result, nil
}

func authorizeEnqueue(ctx context.Context, tx pgx.Tx, caller Caller, env uuid.UUID, o owners) error {
	return authorizeSessionInput(ctx, tx, caller, env, o, false)
}

func authorizeSessionInput(ctx context.Context, tx pgx.Tx, caller Caller, env uuid.UUID, o owners, steering bool) error {
	var allowed bool
	switch caller.Kind {
	case "user":
		err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM environments e JOIN org_members m ON m.org_id=e.org_id JOIN users u ON u.id=m.user_id
          WHERE e.id=$1 AND m.user_id=$2 AND m.disabled_at IS NULL AND u.disabled_at IS NULL AND m.role IN ('owner','admin','developer'))`, env, caller.ID).Scan(&allowed)
		if err != nil {
			return err
		}
	case "api_key":
		err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM api_keys k JOIN environments e ON (e.id,e.project_id,e.org_id)=(k.environment_id,k.project_id,k.org_id)
          WHERE e.id=$1 AND k.id=$2 AND k.revoked_at IS NULL AND (k.expires_at IS NULL OR k.expires_at>clock_timestamp())
          AND k.role IN ('owner','admin','developer') AND 'sessions.send'=ANY(k.permissions))`, env, caller.ID).Scan(&allowed)
		if err != nil {
			return err
		}
	case "session":
		// Enqueue may address any Session in the same Environment. Exact-Turn
		// steering additionally requires owned ancestry before target admission.
		if caller.Execution.EnvironmentID != env || caller.Execution.SessionID != caller.ID {
			return ErrDenied
		}
		if err := executionAvailable(ctx, tx, caller.Execution, o, caller.TurnID == uuid.Nil()); err != nil {
			if errors.Is(err, ErrNotReady) || errors.Is(err, ErrDenied) {
				return ErrDenied
			}
			return err
		}
		if caller.Execution.AuthorityGeneration != o.generation || (o.lifecycle != "open" && !(steering && o.lifecycle == "closing" && caller.TurnID != uuid.Nil())) {
			return ErrDenied
		}
		err := tx.QueryRow(ctx, `WITH RECURSIVE ancestors AS (
          SELECT id,parent_session_id FROM sessions WHERE environment_id=$1 AND id=$2
          UNION ALL SELECT s.id,s.parent_session_id FROM sessions s JOIN ancestors a ON s.id=a.parent_session_id WHERE s.environment_id=$1
        ) SELECT NOT EXISTS(SELECT 1 FROM session_holds h JOIN ancestors a ON a.id=h.session_id WHERE h.environment_id=$1 AND h.released_at IS NULL AND (h.session_id=$2 OR h.scope='subtree'))
        AND EXISTS(SELECT 1 FROM sessions s JOIN deployments d ON (d.environment_id,d.id)=(s.environment_id,s.deployment_id)
          JOIN computers c ON (c.environment_id,c.id)=(s.environment_id,s.computer_id)
          WHERE s.environment_id=$1 AND s.id=$2 AND d.execution_revoked_at IS NULL AND NOT EXISTS(SELECT 1 FROM computer_secret_revocations revoked WHERE revoked.environment_id=c.environment_id AND revoked.computer_id=c.id) AND c.integrity_fault_at IS NULL AND c.deleted_at IS NULL)
        AND ($3::uuid IS NULL OR EXISTS(SELECT 1 FROM turns WHERE environment_id=$1 AND session_id=$2 AND id=$3 AND process_epoch=$4 AND status='running' AND processing_closed_at IS NULL AND (deadline_at IS NULL OR deadline_at>clock_timestamp())))`, env, caller.ID, nullableID(caller.TurnID), caller.Execution.ProcessEpoch).Scan(&allowed)
		if err != nil {
			return err
		}
	default:
		return ErrDenied
	}
	if !allowed {
		return ErrDenied
	}
	return nil
}

// Execution identifies the exact native process authorized by a Computer lease.
type Execution struct {
	EnvironmentID       uuid.UUID
	SessionID           uuid.UUID
	ProcessEpoch        int64
	LeaseEpoch          int64
	WorkerHostID        uuid.UUID
	WorkerEpoch         int64
	AuthorityGeneration int64
}

func executionReady(ctx context.Context, tx pgx.Tx, e Execution, o owners) error {
	return executionAvailable(ctx, tx, e, o, false)
}
func executionAvailable(ctx context.Context, tx pgx.Tx, e Execution, o owners, allowStarting bool) error {
	if e.AuthorityGeneration != o.generation {
		return ErrDenied
	}
	if err := executionPhysical(ctx, tx, e, allowStarting, true); err != nil {
		return err
	}
	return computerDispatchAvailable(ctx, tx, e.EnvironmentID, o.computer)
}

// A receipt observes prior acceptance, so ordinary Turn completion or a target
// hold does not make it fresh work. Caller generation and physical identity still
// fence every request, including same-key recovery after a lost response.
func executionReceipt(ctx context.Context, tx pgx.Tx, e Execution, o owners) error {
	if e.AuthorityGeneration != o.generation {
		return ErrDenied
	}
	err := executionPhysical(ctx, tx, e, true, false)
	if errors.Is(err, ErrNotReady) {
		return ErrDenied
	}
	return err
}

func executionPhysical(ctx context.Context, tx pgx.Tx, e Execution, allowStarting, requireLiveDeployment bool) error {
	var allowed bool
	err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM session_processes p JOIN computer_leases l
      ON (l.environment_id,l.computer_id,l.epoch)=(p.environment_id,p.computer_id,p.computer_lease_epoch)
      JOIN computers c ON (c.environment_id,c.id)=(l.environment_id,l.computer_id)
      JOIN worker_hosts h ON h.id=l.worker_host_id AND h.current_epoch=l.worker_epoch AND h.status IN ('active','draining')
      JOIN sessions s ON (s.environment_id,s.id)=(p.environment_id,p.session_id)
      JOIN deployments d ON (d.environment_id,d.id)=(s.environment_id,s.deployment_id)
      WHERE p.environment_id=$1 AND p.session_id=$2 AND p.epoch=$3 AND l.epoch=$4 AND l.worker_host_id=$5
        AND (p.status='ready' OR ($7 AND p.status='starting')) AND p.fenced_at IS NULL AND l.status='active' AND l.fenced_at IS NULL
        AND l.worker_epoch=$6 AND l.expires_at>clock_timestamp()
        AND c.initial_root_id IS NOT NULL AND c.preparation_failed_at IS NULL AND c.integrity_fault_at IS NULL AND c.deleted_at IS NULL AND (NOT $8 OR (d.execution_revoked_at IS NULL AND NOT EXISTS(SELECT 1 FROM computer_secret_revocations revoked WHERE revoked.environment_id=c.environment_id AND revoked.computer_id=c.id))))`, e.EnvironmentID, e.SessionID, e.ProcessEpoch, e.LeaseEpoch, e.WorkerHostID, e.WorkerEpoch, allowStarting, requireLiveDeployment).Scan(&allowed)
	if err != nil {
		return err
	}
	if !allowed {
		return ErrNotReady
	}
	return nil
}

// Dispatch returns a stable execution assignment. Repeating the same process
// claim reconciles a lost reply; it is not permission to invoke a handler twice.
// The guest deduplicates delivery by Turn identity and reports the recorded phase.
type TurnSource struct {
	Kind               string     `json:"kind"`
	RequesterSessionID *uuid.UUID `json:"requesterSessionId,omitempty"`
	OriginTurnID       *uuid.UUID `json:"originTurnId,omitempty"`
}

type TurnDispatch struct {
	TurnID    uuid.UUID
	Status    string
	Sequence  int64
	CreatedAt time.Time
	Input     json.RawMessage
	Source    TurnSource
}

func Dispatch(ctx context.Context, pool db.TxBeginner, e Execution) (TurnDispatch, error) {
	var result TurnDispatch
	err := db.RunTx(ctx, pool, func(tx pgx.Tx) error { return dispatchTurn(ctx, tx, e, &result) })
	if err != nil {
		return TurnDispatch{}, err
	}
	return result, nil
}

func dispatchTurn(ctx context.Context, tx pgx.Tx, e Execution, result *TurnDispatch) error {
	o, err := lockSession(ctx, tx, e.EnvironmentID, e.SessionID)
	if err != nil {
		return err
	}
	if o.lifecycle != "open" && o.lifecycle != "closing" {
		return ErrNotReady
	}
	if err = executionReady(ctx, tx, e, o); err != nil {
		return err
	}
	var blocked bool
	err = tx.QueryRow(ctx, `WITH RECURSIVE ancestors AS (
          SELECT id,parent_session_id FROM sessions WHERE environment_id=$1 AND id=$2
          UNION ALL SELECT s.id,s.parent_session_id FROM sessions s JOIN ancestors a ON s.id=a.parent_session_id WHERE s.environment_id=$1
        ) SELECT EXISTS(SELECT 1 FROM session_holds h JOIN ancestors a ON a.id=h.session_id WHERE h.environment_id=$1 AND h.released_at IS NULL AND (h.session_id=$2 OR h.scope='subtree'))
        OR EXISTS(SELECT 1 FROM computer_saves WHERE environment_id=$1 AND computer_id=$3 AND computer_lease_epoch<>$4 AND status IN ('requested','captured'))`, e.EnvironmentID, e.SessionID, o.computer, e.LeaseEpoch).Scan(&blocked)
	if err != nil {
		return err
	}
	if blocked {
		return ErrNotReady
	}
	var process int64
	err = tx.QueryRow(ctx, `SELECT id,status,process_epoch FROM turns WHERE environment_id=$1 AND session_id=$2 AND status IN ('running','finalizing') FOR NO KEY UPDATE`, e.EnvironmentID, e.SessionID).Scan(&result.TurnID, &result.Status, &process)
	if err == nil {
		if process != e.ProcessEpoch {
			return ErrNotReady
		}
		return loadTurnDispatch(ctx, tx, e.EnvironmentID, result)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	err = tx.QueryRow(ctx, `SELECT id FROM turns WHERE environment_id=$1 AND session_id=$2 AND status='queued' ORDER BY seq LIMIT 1 FOR NO KEY UPDATE`, e.EnvironmentID, e.SessionID).Scan(&result.TurnID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotReady
	}
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE turns SET status='running',process_epoch=$3,started_at=clock_timestamp(),
          deadline_at=CASE WHEN d.max_turn_duration_ms IS NOT NULL THEN clock_timestamp()+d.max_turn_duration_ms*interval '1 millisecond' END
          FROM sessions s JOIN agent_definitions d ON (d.environment_id,d.agent_id,d.deployment_id)=(s.environment_id,s.agent_id,s.deployment_id)
          WHERE turns.environment_id=$1 AND turns.id=$2 AND (s.environment_id,s.id)=(turns.environment_id,turns.session_id)`, e.EnvironmentID, result.TurnID, e.ProcessEpoch)
	if err != nil {
		return err
	}
	result.Status = "running"
	if err := event(ctx, tx, e.EnvironmentID, e.SessionID, result.TurnID, "turn.running"); err != nil {
		return err
	}
	return loadTurnDispatch(ctx, tx, e.EnvironmentID, result)
}

// SaveRequest is durable before the worker may flush/capture. An uncertain reply
// is reconciled with this identity, never replaced by another cut.
type SaveRequest struct {
	ID          uuid.UUID
	ComputerID  uuid.UUID
	LeaseEpoch  int64
	Sequence    int64
	RequestedAt time.Time
}

func nullableID(id uuid.UUID) any {
	if id == uuid.Nil() {
		return nil
	}
	return id
}

func hideMissing(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrDenied
	}
	return err
}

// Worker and Group claims are locked in the same transaction as Session
// admission. HTTP authentication alone cannot protect a request delayed by locks.
func lockRuntimeHost(ctx context.Context, tx pgx.Tx, caller Caller) error {
	if caller.Host == nil || caller.Host.HostID != caller.Execution.WorkerHostID || caller.Host.Epoch != caller.Execution.WorkerEpoch {
		return ErrDenied
	}
	eligible, err := workergroup.LockRuntimeHost(ctx, tx, *caller.Host)
	if err != nil {
		return err
	}
	if !eligible {
		return ErrDenied
	}
	return nil
}

func loadTurnDispatch(ctx context.Context, tx pgx.Tx, environment uuid.UUID, dispatch *TurnDispatch) error {
	return tx.QueryRow(ctx, `SELECT seq,created_at,input,caller_kind,CASE WHEN caller_kind='session' THEN caller_id END,origin_turn_id FROM turns WHERE environment_id=$1 AND id=$2`, environment, dispatch.TurnID).Scan(&dispatch.Sequence, &dispatch.CreatedAt, &dispatch.Input, &dispatch.Source.Kind, &dispatch.Source.RequesterSessionID, &dispatch.Source.OriginTurnID)
}
