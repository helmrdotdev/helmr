package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
)

var ErrMessageClosed = errors.New("turn is not accepting messages")

type SendReceipt struct {
	Admission
	MessageID uuid.UUID
}

// Send resolves steering or enqueue once under the same destination owner lock.
func Send(ctx context.Context, pool db.TxBeginner, caller Caller, req EnqueueRequest) (SendReceipt, error) {
	return admitSessionInput(ctx, pool, caller, req, "send", uuid.Nil())
}
func SendTurn(ctx context.Context, pool db.TxBeginner, caller Caller, req EnqueueRequest, turn uuid.UUID) (SendReceipt, error) {
	if turn == uuid.Nil() {
		return SendReceipt{}, ErrInvalidInput
	}
	return admitSessionInput(ctx, pool, caller, req, "turn_send", turn)
}

func messageRetry(ctx context.Context, tx pgx.Tx, caller Caller, req EnqueueRequest, method string, target uuid.UUID, digest [32]byte, result *SendReceipt) (bool, error) {
	var prior []byte
	err := tx.QueryRow(ctx, `SELECT session_id,turn_id,id,request_digest FROM turn_messages WHERE environment_id=$1 AND caller_kind=$2 AND caller_id=$3 AND admission_method=$4 AND target_id=$5 AND retry_key=$6 AND session_id=$7`, req.EnvironmentID, caller.Kind, caller.ID, method, target, req.RetryKey, req.SessionID).Scan(&result.SessionID, &result.TurnID, &result.MessageID, &prior)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !bytes.Equal(prior, digest[:]) {
		return true, ErrConflict
	}
	return true, nil
}

func admitMessage(ctx context.Context, tx pgx.Tx, caller Caller, req EnqueueRequest, method string, exactTurn uuid.UUID, digest [32]byte, o owners, result *SendReceipt) (bool, error) {
	e := Execution{EnvironmentID: req.EnvironmentID, SessionID: req.SessionID, AuthorityGeneration: o.generation}
	var turn uuid.UUID
	err := tx.QueryRow(ctx, `SELECT t.id,t.process_epoch,p.computer_lease_epoch,l.worker_host_id,l.worker_epoch FROM turns t
 JOIN session_processes p ON (p.environment_id,p.session_id,p.epoch)=(t.environment_id,t.session_id,t.process_epoch)
 JOIN computer_leases l ON (l.environment_id,l.computer_id,l.epoch)=(p.environment_id,p.computer_id,p.computer_lease_epoch)
 WHERE t.environment_id=$1 AND t.session_id=$2 AND ($3::uuid IS NULL OR t.id=$3) AND t.status='running' AND t.messages_registered_at IS NOT NULL AND t.processing_closed_at IS NULL AND (t.deadline_at IS NULL OR t.deadline_at>clock_timestamp()) FOR NO KEY UPDATE OF t`, req.EnvironmentID, req.SessionID, nullableID(exactTurn)).Scan(&turn, &e.ProcessEpoch, &e.LeaseEpoch, &e.WorkerHostID, &e.WorkerEpoch)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err = executionReady(ctx, tx, e, o); errors.Is(err, ErrNotReady) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	var held bool
	if err = tx.QueryRow(ctx, `WITH RECURSIVE ancestors AS (
 SELECT id,parent_session_id FROM sessions WHERE environment_id=$1 AND id=$2
 UNION ALL SELECT s.id,s.parent_session_id FROM sessions s JOIN ancestors a ON s.id=a.parent_session_id WHERE s.environment_id=$1)
 SELECT EXISTS(SELECT 1 FROM session_holds h JOIN ancestors a ON a.id=h.session_id WHERE h.environment_id=$1 AND h.released_at IS NULL AND (h.session_id=$2 OR h.scope='subtree'))`, req.EnvironmentID, req.SessionID).Scan(&held); err != nil {
		return false, err
	}
	if held {
		return false, nil
	}
	var sequence int64
	if err = tx.QueryRow(ctx, `UPDATE turns SET next_message_seq=next_message_seq+1 WHERE environment_id=$1 AND id=$2 RETURNING next_message_seq-1`, req.EnvironmentID, turn).Scan(&sequence); err != nil {
		return false, err
	}
	result.SessionID, result.TurnID, result.MessageID = req.SessionID, turn, uuid.NewV7()
	target := req.SessionID
	if method == "turn_send" {
		target = turn
	}
	if _, err = tx.Exec(ctx, `INSERT INTO turn_messages(environment_id,id,session_id,turn_id,process_epoch,seq,message,caller_kind,caller_id,origin_turn_id,admission_method,target_id,retry_key,request_digest)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,NULLIF($13,''),$14)`, req.EnvironmentID, result.MessageID, req.SessionID, turn, e.ProcessEpoch, sequence, []byte(req.Input), caller.Kind, caller.ID, nullableID(caller.TurnID), method, target, req.RetryKey, digest[:]); err != nil {
		return false, err
	}
	return true, messageEvent(ctx, tx, req.EnvironmentID, req.SessionID, turn, result.MessageID, "admitted", "")
}

func messageEvent(ctx context.Context, tx pgx.Tx, env, session, turn, id uuid.UUID, status, reason string) error {
	delivery := status
	if status == "rejected" {
		var started bool
		if err := tx.QueryRow(ctx, `SELECT started_at IS NOT NULL FROM turn_messages WHERE environment_id=$1 AND session_id=$2 AND turn_id=$3 AND id=$4`, env, session, turn, id).Scan(&started); err != nil {
			return err
		}
		delivery = "not_delivered"
		if started && reason != "message_rejected" && reason != "turn_closed" {
			delivery = "uncertain"
		}
	}
	value := map[string]any{"message_id": id, "status": status, "delivery": delivery}
	if reason != "" {
		value["reason"] = reason
	}
	raw, _ := json.Marshal(value)
	return eventData(ctx, tx, env, session, turn, "message."+status, raw)
}

func RuntimeRegisterMessages(ctx context.Context, pool db.TxBeginner, host workergroup.HostPrincipal, e Execution, turn uuid.UUID) error {
	return hideMissing(db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		if err := lockRuntimeOperationOwner(ctx, tx, host, e); err != nil {
			return err
		}
		if err := lockRuntimeContentTurn(ctx, tx, e, turn); err != nil {
			return err
		}
		var registered *time.Time
		if err := tx.QueryRow(ctx, `SELECT messages_registered_at FROM turns WHERE environment_id=$1 AND id=$2`, e.EnvironmentID, turn).Scan(&registered); err != nil {
			return err
		}
		if registered != nil {
			return nil
		}
		if err := runtimeContentReady(ctx, tx, e, turn, true); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE turns SET messages_registered_at=clock_timestamp() WHERE environment_id=$1 AND id=$2`, e.EnvironmentID, turn)
		return err
	}))
}

type MessageDelivery struct {
	ID, TurnID uuid.UUID
	Input      json.RawMessage
}

// ClaimRuntimeMessage serializes callbacks; an uncertain delivery returns the
// same started identity until its disposition is durable.
func ClaimRuntimeMessage(ctx context.Context, pool db.TxBeginner, host workergroup.HostPrincipal, e Execution, attachment int64, turn uuid.UUID) (*MessageDelivery, error) {
	var result *MessageDelivery
	err := db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		if err := lockRuntimeOperationOwner(ctx, tx, host, e); err != nil {
			return err
		}
		if err := lockRuntimeContentTurn(ctx, tx, e, turn); err != nil {
			return err
		}
		if err := currentMessageAttachment(ctx, tx, e, attachment); err != nil {
			return err
		}
		if err := runtimeContentReady(ctx, tx, e, turn, false); err != nil {
			return err
		}
		var m MessageDelivery
		err := tx.QueryRow(ctx, `SELECT id,turn_id,message FROM turn_messages WHERE environment_id=$1 AND session_id=$2 AND turn_id=$3 AND status='started'`, e.EnvironmentID, e.SessionID, turn).Scan(&m.ID, &m.TurnID, &m.Input)
		if err == nil {
			result = &m
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		err = tx.QueryRow(ctx, `UPDATE turn_messages SET status='started',started_at=clock_timestamp() WHERE environment_id=$1 AND id=(SELECT m.id FROM turn_messages m JOIN turns t ON t.environment_id=m.environment_id AND t.id=m.turn_id WHERE m.environment_id=$1 AND m.session_id=$2 AND m.turn_id=$3 AND m.status='admitted' AND t.processing_closed_at IS NULL ORDER BY m.seq LIMIT 1) RETURNING id,turn_id,message`, e.EnvironmentID, e.SessionID, turn).Scan(&m.ID, &m.TurnID, &m.Input)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		result = &m
		return messageEvent(ctx, tx, e.EnvironmentID, e.SessionID, turn, m.ID, "started", "")
	})
	return result, hideMissing(err)
}
func currentMessageAttachment(ctx context.Context, tx pgx.Tx, e Execution, attachment int64) error {
	var current int64
	if err := tx.QueryRow(ctx, `SELECT attachment_sequence FROM session_processes WHERE environment_id=$1 AND session_id=$2 AND epoch=$3`, e.EnvironmentID, e.SessionID, e.ProcessEpoch).Scan(&current); err != nil {
		return err
	}
	if attachment <= 0 || attachment != current {
		return ErrDenied
	}
	return nil
}

func CompleteRuntimeMessage(ctx context.Context, pool db.TxBeginner, host workergroup.HostPrincipal, e Execution, attachment int64, turn, id uuid.UUID, reason string) error {
	if reason != "" && reason != "message_rejected" && reason != "callback_failed" && reason != "turn_closed" {
		return ErrInvalidInput
	}
	return hideMissing(db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		if err := lockRuntimeOperationOwner(ctx, tx, host, e); err != nil {
			return err
		}
		if err := lockRuntimeContentTurn(ctx, tx, e, turn); err != nil {
			return err
		}
		if err := currentMessageAttachment(ctx, tx, e, attachment); err != nil {
			return err
		}
		var status string
		if err := tx.QueryRow(ctx, `SELECT status FROM turn_messages WHERE environment_id=$1 AND session_id=$2 AND turn_id=$3 AND id=$4 AND process_epoch=$5 FOR UPDATE`, e.EnvironmentID, e.SessionID, turn, id, e.ProcessEpoch).Scan(&status); err != nil {
			return err
		}
		if status == "delivered" || status == "rejected" {
			return nil
		}
		if status != "started" {
			return ErrNotReady
		}
		status = "delivered"
		if reason != "" {
			status = "rejected"
		}
		if _, err := tx.Exec(ctx, `UPDATE turn_messages SET status=$3,rejection_reason=NULLIF($4,''),terminal_at=clock_timestamp() WHERE environment_id=$1 AND id=$2`, e.EnvironmentID, id, status, reason); err != nil {
			return err
		}
		return messageEvent(ctx, tx, e.EnvironmentID, e.SessionID, turn, id, status, reason)
	}))
}

func rejectTurnMessages(ctx context.Context, tx pgx.Tx, env, session, turn uuid.UUID, includeStarted bool, reason string) error {
	rows, err := tx.Query(ctx, `UPDATE turn_messages SET status='rejected',rejection_reason=$5,terminal_at=clock_timestamp() WHERE environment_id=$1 AND session_id=$2 AND turn_id=$3 AND (status='admitted' OR ($4 AND status='started')) RETURNING id`, env, session, turn, includeStarted, reason)
	if err != nil {
		return err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err = messageEvent(ctx, tx, env, session, turn, id, "rejected", reason); err != nil {
			return err
		}
	}
	return nil
}
