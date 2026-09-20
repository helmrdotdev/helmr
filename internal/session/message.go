package session

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/jsoncanon"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func ValidateTurnWork(ctx context.Context, q db.Querier, scope TurnScope) (db.SessionTurn, error) {
	turn, err := ValidateTurn(ctx, q, scope)
	if err == nil && turn.SettlementStartedAt.Valid {
		err = &OperationError{Code: "turn_unsettled"}
	}
	return turn, err
}

func DeclareMessageReady(ctx context.Context, q db.Querier, scope TurnScope, leaseID uuid.UUID) (db.SessionTurn, error) {
	if leaseID == uuid.Nil() {
		return db.SessionTurn{}, ErrTurnScope
	}
	if _, err := ValidateTurnWork(ctx, q, scope); err != nil {
		return db.SessionTurn{}, err
	}
	return q.SetSessionTurnMessageReady(ctx, db.SetSessionTurnMessageReadyParams{EnvironmentID: pgvalue.UUID(scope.EnvironmentID), SessionID: pgvalue.UUID(scope.SessionID), TurnID: pgvalue.UUID(scope.TurnID), RunLeaseID: pgvalue.UUID(leaseID)})
}

// Delivery IDs are immutable operation identities. A lost claim response is
// reconciled by the same ID, never by admitting the next callback.
func ClaimMessage(ctx context.Context, q db.Querier, scope TurnScope, leaseID, deliveryID uuid.UUID) (db.SessionMessage, error) {
	if leaseID == uuid.Nil() || deliveryID == uuid.Nil() {
		return db.SessionMessage{}, ErrTurnScope
	}
	if _, err := ValidateTurnWork(ctx, q, scope); err != nil {
		return db.SessionMessage{}, err
	}
	existing, err := q.GetSessionMessageDelivery(ctx, db.GetSessionMessageDeliveryParams{EnvironmentID: pgvalue.UUID(scope.EnvironmentID), SessionID: pgvalue.UUID(scope.SessionID), DeliveryID: pgvalue.UUID(deliveryID)})
	if err == nil {
		if existing.TurnID != pgvalue.UUID(scope.TurnID) || existing.RunID != pgvalue.UUID(scope.RunID) || existing.AttemptNumber != scope.AttemptNumber || existing.RunGeneration != scope.RunGeneration || existing.DeliveryRunLeaseID != pgvalue.UUID(leaseID) {
			return db.SessionMessage{}, ErrTurnScope
		}
		if existing.Status != "handling" {
			return db.SessionMessage{}, &OperationError{Code: "delivery_settled"}
		}
		return existing, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return db.SessionMessage{}, err
	}
	ready, err := q.SessionTurnMessageReady(ctx, db.SessionTurnMessageReadyParams{EnvironmentID: pgvalue.UUID(scope.EnvironmentID), SessionID: pgvalue.UUID(scope.SessionID), ID: pgvalue.UUID(scope.TurnID)})
	if err != nil {
		return db.SessionMessage{}, err
	}
	if !ready {
		return db.SessionMessage{}, &OperationError{Code: "turn_not_ready"}
	}
	return q.ClaimSessionMessage(ctx, db.ClaimSessionMessageParams{EnvironmentID: pgvalue.UUID(scope.EnvironmentID), SessionID: pgvalue.UUID(scope.SessionID), TurnID: pgvalue.UUID(scope.TurnID), RunLeaseID: pgvalue.UUID(leaseID), DeliveryID: pgvalue.UUID(deliveryID)})
}

type MessageOutcome struct {
	Status  string          `json:"status"`
	Code    string          `json:"code,omitempty"`
	Details json.RawMessage `json:"details,omitempty"`
}

func CompleteMessage(ctx context.Context, q db.Querier, scope TurnScope, leaseID, messageID, deliveryID uuid.UUID, outcome MessageOutcome) (db.SessionMessage, error) {
	switch outcome.Status {
	case "handled":
		if outcome.Code != "" {
			return db.SessionMessage{}, &OperationError{Code: "invalid_request"}
		}
	case "rejected":
		if outcome.Code != "handler_rejected" {
			return db.SessionMessage{}, &OperationError{Code: "invalid_request"}
		}
	case "unknown":
		if outcome.Code != "handler_failed" {
			return db.SessionMessage{}, &OperationError{Code: "invalid_request"}
		}
	default:
		return db.SessionMessage{}, &OperationError{Code: "invalid_request"}
	}
	if len(outcome.Details) > 0 && !json.Valid(outcome.Details) {
		return db.SessionMessage{}, &OperationError{Code: "invalid_request"}
	}
	actor, turn, err := lockTurn(ctx, q, scope)
	if err != nil {
		return db.SessionMessage{}, err
	}
	// An already admitted callback may acknowledge completion during stop or
	// settlement. That acknowledgment admits no output or new native operation.
	if actor.CurrentRunID != pgvalue.UUID(scope.RunID) || actor.RunGeneration != scope.RunGeneration || actor.ActiveTurnID != turn.ID || turn.Status != "running" {
		return db.SessionMessage{}, ErrTurnScope
	}
	message, err := q.LockSessionMessage(ctx, db.LockSessionMessageParams{EnvironmentID: actor.EnvironmentID, SessionID: actor.ID, ID: pgvalue.UUID(messageID)})
	if err != nil {
		return message, err
	}
	if message.TurnID != turn.ID || message.RunID != pgvalue.UUID(scope.RunID) || message.AttemptNumber != scope.AttemptNumber || message.RunGeneration != scope.RunGeneration || message.DeliveryID != pgvalue.UUID(deliveryID) || message.DeliveryRunLeaseID != pgvalue.UUID(leaseID) {
		return message, ErrTurnScope
	}
	raw, _ := json.Marshal(outcome)
	if message.Status != "handling" {
		var prior MessageOutcome
		if json.Unmarshal(message.Outcome, &prior) != nil || prior.Status != outcome.Status || prior.Code != outcome.Code || !jsonEqual(prior.Details, outcome.Details) {
			return message, &OperationError{Code: "idempotency_conflict"}
		}
		return message, nil
	}
	message, err = finishMessage(ctx, q, actor, message, outcome, raw)
	if err != nil {
		return message, err
	}
	if outcome.Status == "unknown" {
		_, err = run.HoldSessionExecution(ctx, q, actor, scope.AttemptNumber, "recovery_required")
	}
	return message, err
}

func finishMessage(ctx context.Context, q db.Querier, actor db.Session, message db.SessionMessage, outcome MessageOutcome, raw []byte) (db.SessionMessage, error) {
	updated, err := q.FinishSessionMessage(ctx, db.FinishSessionMessageParams{EnvironmentID: actor.EnvironmentID, SessionID: actor.ID, ID: message.ID, ExpectedStatus: message.Status, Status: outcome.Status, Outcome: raw})
	if err != nil {
		return updated, err
	}
	body, _ := json.Marshal(struct {
		MessageID uuid.UUID       `json:"message_id"`
		Code      string          `json:"code,omitempty"`
		Details   json.RawMessage `json:"details,omitempty"`
	}{pgvalue.MustUUIDValue(message.ID), outcome.Code, outcome.Details})
	_, err = appendLifecycleEvent(ctx, q, actor, message.TurnID, message.ID, "message."+outcome.Status, body, pgtype.UUID{})
	return updated, err
}

func rejectQueuedMessages(ctx context.Context, q db.Querier, actor db.Session, turnID pgtype.UUID, code string) error {
	messages, err := q.ListUnsettledSessionMessages(ctx, db.ListUnsettledSessionMessagesParams{EnvironmentID: actor.EnvironmentID, SessionID: actor.ID, TurnID: turnID})
	if err != nil {
		return err
	}
	for _, message := range messages {
		if message.Status != "accepted" {
			continue
		}
		outcome := MessageOutcome{Status: "rejected", Code: code}
		raw, _ := json.Marshal(outcome)
		if _, err := finishMessage(ctx, q, actor, message, outcome, raw); err != nil {
			return err
		}
	}
	return nil
}

func BeginSettlement(ctx context.Context, q db.Querier, scope TurnScope) (db.SessionTurn, error) {
	actor, turn, err := lockTurn(ctx, q, scope)
	if err != nil {
		return turn, err
	}
	if _, err = validateTurn(actor, turn, scope); err != nil {
		return turn, err
	}
	turn, err = q.BeginSessionTurnSettlement(ctx, db.BeginSessionTurnSettlementParams{EnvironmentID: actor.EnvironmentID, SessionID: actor.ID, ID: turn.ID})
	if err != nil {
		return turn, err
	}
	return turn, rejectQueuedMessages(ctx, q, actor, turn.ID, "turn_settling")
}

func jsonEqual(a, b []byte) bool {
	if len(a) == 0 || len(b) == 0 {
		return len(a) == len(b)
	}
	x, e := jsoncanon.Transform(a)
	if e != nil {
		return false
	}
	y, e := jsoncanon.Transform(b)
	return e == nil && bytes.Equal(x, y)
}

// Reconciliation never retries an admitted callback. After writer exclusion,
// an unacknowledged delivery remains unknown and unstarted work is rejected.
func finishRecoveredMessages(ctx context.Context, q db.Querier, actor db.Session, turnID pgtype.UUID) error {
	messages, err := q.ListUnsettledSessionMessages(ctx, db.ListUnsettledSessionMessagesParams{EnvironmentID: actor.EnvironmentID, SessionID: actor.ID, TurnID: turnID})
	if err != nil {
		return err
	}
	for _, message := range messages {
		outcome := MessageOutcome{Status: "rejected", Code: "turn_stopping"}
		if message.Status == "handling" {
			outcome = MessageOutcome{Status: "unknown", Code: "execution_lost"}
		}
		raw, _ := json.Marshal(outcome)
		if _, err = finishMessage(ctx, q, actor, message, outcome, raw); err != nil {
			return err
		}
	}
	return nil
}
