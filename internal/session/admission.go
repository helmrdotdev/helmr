package session

import (
	"context"
	"encoding/json"
	"errors"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/idempotency"
	"github.com/helmrdotdev/helmr/internal/jsoncanon"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func lockSession(ctx context.Context, q db.Querier, target Target) (db.Session, error) {
	s, err := q.LockSessionTurnAuthority(ctx, db.LockSessionTurnAuthorityParams{EnvironmentID: pgvalue.UUID(target.EnvironmentID), ID: pgvalue.UUID(target.SessionID)})
	if errors.Is(err, pgx.ErrNoRows) {
		return s, &OperationError{Code: "session_not_found"}
	}
	return s, err
}

func claimOperation(ctx context.Context, q db.Querier, request ControlRequest, name string, fingerprint any) (db.IdempotencyClaim, error) {
	key := request.IdempotencyKey
	if key == "" {
		key = uuid.NewV7().String()
	}
	r, err := idempotency.NewSessionOperationRequest(request.EnvironmentID, request.SessionID, key, name, fingerprint)
	if err != nil {
		return db.IdempotencyClaim{}, err
	}
	tx, err := idempotency.TransactionForQueries(q)
	if err != nil {
		return db.IdempotencyClaim{}, err
	}
	result, err := tx.Acquire(ctx, r)
	return result.Claim, err
}

func finishOperation(ctx context.Context, q db.Querier, claim db.IdempotencyClaim, receipt any) error {
	raw, err := json.Marshal(receipt)
	if err != nil {
		return err
	}
	tx, err := idempotency.TransactionForQueries(q)
	if err != nil {
		return err
	}
	_, err = tx.Complete(ctx, claim, raw)
	return err
}

// Admit binds auto routing and all business rejections while holding the same
// Session row used by stop, activation and terminal settlement.
func Admit(ctx context.Context, q db.Querier, request AdmissionRequest) (AdmissionReceipt, error) {
	var name string
	switch request.Mode {
	case SendMessageOrEnqueue:
		name = "session.send"
	case EnqueueOnly:
		name = "session.enqueue"
	case ExactMessage:
		name = "turn.message"
		if request.TurnID == uuid.Nil() {
			return AdmissionReceipt{}, &OperationError{Code: "invalid_request"}
		}
	default:
		return AdmissionReceipt{}, &OperationError{Code: "invalid_request"}
	}
	data, err := jsoncanon.Transform(request.Data)
	if err != nil || len(data) > 1<<20 {
		return AdmissionReceipt{}, &OperationError{Code: "invalid_request"}
	}
	actor, err := lockSession(ctx, q, request.Target)
	if err != nil {
		return AdmissionReceipt{}, err
	}
	claim, err := claimOperation(ctx, q, ControlRequest{Target: request.Target, IdempotencyKey: request.IdempotencyKey}, name, struct {
		TurnID      uuid.UUID       `json:"turn_id"`
		SourceRunID uuid.UUID       `json:"source_run_id"`
		Data        json.RawMessage `json:"data"`
	}{request.TurnID, request.SourceRunID, data})
	if err != nil {
		return AdmissionReceipt{}, err
	}
	var receipt AdmissionReceipt
	if claim.Status == "completed" {
		err = json.Unmarshal(claim.Receipt, &receipt)
		return receipt, err
	}
	receipt.ID = pgvalue.MustUUIDValue(claim.ID)
	if actor.NextEventSequence > 9007199254740991 {
		receipt.Code = "invalid_request"
	} else if request.Mode != ExactMessage && actor.Status != "open" {
		receipt.Code = "session_not_open"
	} else if request.Mode != EnqueueOnly && actor.DispatchHoldID.Valid {
		receipt.Code = "session_held"
	} else if request.Mode == ExactMessage && actor.ActiveTurnID != pgvalue.UUID(request.TurnID) {
		receipt.Code = "turn_not_active"
	} else if request.Mode == ExactMessage || request.Mode == SendMessageOrEnqueue && actor.ActiveTurnID.Valid {
		receipt.TurnID = pgvalue.MustUUIDValue(actor.ActiveTurnID)
		ready, err := q.SessionTurnMessageReady(ctx, db.SessionTurnMessageReadyParams{EnvironmentID: actor.EnvironmentID, SessionID: actor.ID, ID: actor.ActiveTurnID})
		if err != nil {
			return receipt, err
		}
		if !ready {
			receipt.Code = "turn_not_ready"
		} else {
			turn, err := q.GetSessionTurn(ctx, db.GetSessionTurnParams{EnvironmentID: actor.EnvironmentID, SessionID: actor.ID, ID: actor.ActiveTurnID})
			if err != nil {
				return receipt, err
			}
			messageID := uuid.NewV7()
			// The locked event allocator gives message delivery its public order.
			_, err = q.CreateSessionMessage(ctx, db.CreateSessionMessageParams{ID: pgvalue.UUID(messageID), EnvironmentID: actor.EnvironmentID, SessionID: actor.ID, TurnID: turn.ID, RunID: turn.RunID, AttemptNumber: turn.AttemptNumber.Int32, RunGeneration: turn.RunGeneration.Int64, Data: data, AcceptedSequence: actor.NextEventSequence})
			if err != nil {
				return receipt, err
			}
			body, _ := json.Marshal(map[string]any{"message_id": messageID, "message": json.RawMessage(data)})
			if _, err := appendLifecycleEvent(ctx, q, actor, turn.ID, pgvalue.UUID(messageID), "message.accepted", body, pgtype.UUID{}); err != nil {
				return receipt, err
			}
			receipt.Kind, receipt.MessageID = "messaged", &messageID
		}
	} else if actor.NextInputSequence > 9007199254740991 || actor.NextEventSequence > 9007199254740991 {
		receipt.Code = "invalid_request"
	} else {
		turnID := uuid.NewV7()
		var source pgtype.UUID
		if request.SourceRunID != uuid.Nil() {
			source = pgvalue.UUID(request.SourceRunID)
		}
		turn, err := q.EnqueueSessionTurn(ctx, db.EnqueueSessionTurnParams{EnvironmentID: actor.EnvironmentID, SessionID: actor.ID, ID: pgvalue.UUID(turnID), Data: data, SourceRunID: source})
		if err != nil {
			return receipt, err
		}
		body, _ := json.Marshal(map[string]any{"input": json.RawMessage(data)})
		if _, err := appendLifecycleEvent(ctx, q, actor, turn.ID, pgtype.UUID{}, "turn.enqueued", body, pgtype.UUID{}); err != nil {
			return receipt, err
		}
		if err := q.CreateActorInputReconcileOutbox(ctx, db.CreateActorInputReconcileOutboxParams{ID: turn.ID, SessionID: actor.ID, EnvironmentID: actor.EnvironmentID, RecordID: turn.ID}); err != nil {
			return receipt, err
		}
		receipt.Kind, receipt.TurnID = "enqueued", turnID
	}
	return receipt, finishOperation(ctx, q, claim, receipt)
}

func appendLifecycleEvent(ctx context.Context, q db.Querier, actor db.Session, turnID, messageID pgtype.UUID, kind string, data json.RawMessage, version pgtype.UUID) (db.SessionEvent, error) {
	return q.AppendSessionEvent(ctx, db.AppendSessionEventParams{ID: pgvalue.UUID(uuid.NewV7()), EnvironmentID: actor.EnvironmentID, SessionID: actor.ID, TurnID: turnID, MessageID: messageID, Kind: kind, Data: data, WorkspaceVersionID: version})
}
