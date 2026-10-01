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

// validateTurnWork is run.ValidateTurnWork with a Turn that began settlement
// reported as the turn_unsettled operation code.
func validateTurnWork(ctx context.Context, tx pgx.Tx, scope run.TurnScope) error {
	_, err := run.ValidateTurnWork(ctx, tx, scope)
	return turnWorkError(err)
}

func turnWorkError(err error) error {
	if errors.Is(err, run.ErrTurnUnsettled) {
		return &OperationError{Code: "turn_unsettled"}
	}
	return err
}

func DeclareMessageReady(ctx context.Context, tx pgx.Tx, scope run.TurnScope, leaseID uuid.UUID) (db.SessionTurn, error) {
	q := db.New(tx)
	if leaseID == uuid.Nil() {
		return db.SessionTurn{}, run.ErrTurnScope
	}
	if err := validateTurnWork(ctx, tx, scope); err != nil {
		return db.SessionTurn{}, err
	}
	return q.SetSessionTurnMessageReady(ctx, db.SetSessionTurnMessageReadyParams{EnvironmentID: pgvalue.UUID(scope.EnvironmentID), SessionID: pgvalue.UUID(scope.SessionID), TurnID: pgvalue.UUID(scope.TurnID), RunLeaseID: pgvalue.UUID(leaseID)})
}

// Delivery IDs are immutable operation identities. A lost claim response is
// reconciled by the same ID, never by admitting the next callback.
func ClaimMessage(ctx context.Context, tx pgx.Tx, scope run.TurnScope, leaseID, deliveryID uuid.UUID) (db.SessionMessage, error) {
	q := db.New(tx)
	if leaseID == uuid.Nil() || deliveryID == uuid.Nil() {
		return db.SessionMessage{}, run.ErrTurnScope
	}
	if err := validateTurnWork(ctx, tx, scope); err != nil {
		return db.SessionMessage{}, err
	}
	existing, err := q.GetSessionMessageDelivery(ctx, db.GetSessionMessageDeliveryParams{EnvironmentID: pgvalue.UUID(scope.EnvironmentID), SessionID: pgvalue.UUID(scope.SessionID), DeliveryID: pgvalue.UUID(deliveryID)})
	if err == nil {
		if existing.TurnID != pgvalue.UUID(scope.TurnID) || existing.RunID != pgvalue.UUID(scope.RunID) || existing.AttemptNumber != scope.AttemptNumber || existing.RunGeneration != scope.RunGeneration || existing.DeliveryRunLeaseID != pgvalue.UUID(leaseID) {
			return db.SessionMessage{}, run.ErrTurnScope
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

func CompleteMessage(ctx context.Context, tx pgx.Tx, scope run.TurnScope, leaseID, messageID, deliveryID uuid.UUID, outcome MessageOutcome) (db.SessionMessage, error) {
	q := db.New(tx)
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
	locked, err := run.LockTurn(ctx, tx, scope)
	if err != nil {
		return db.SessionMessage{}, err
	}
	session, turn := locked.Session(), locked.Turn()
	// An already admitted callback may acknowledge completion during stop or
	// settlement. That acknowledgment admits no output or new native operation.
	if session.CurrentRunID != pgvalue.UUID(scope.RunID) || session.RunGeneration != scope.RunGeneration || session.ActiveTurnID != turn.ID || turn.Status != "running" {
		return db.SessionMessage{}, run.ErrTurnScope
	}
	message, err := q.LockSessionMessage(ctx, db.LockSessionMessageParams{EnvironmentID: session.EnvironmentID, SessionID: session.ID, ID: pgvalue.UUID(messageID)})
	if err != nil {
		return message, err
	}
	if message.TurnID != turn.ID || message.RunID != pgvalue.UUID(scope.RunID) || message.AttemptNumber != scope.AttemptNumber || message.RunGeneration != scope.RunGeneration || message.DeliveryID != pgvalue.UUID(deliveryID) || message.DeliveryRunLeaseID != pgvalue.UUID(leaseID) {
		return message, run.ErrTurnScope
	}
	raw, _ := json.Marshal(outcome)
	if message.Status != "handling" {
		var prior MessageOutcome
		if json.Unmarshal(message.Outcome, &prior) != nil || prior.Status != outcome.Status || prior.Code != outcome.Code || !jsonEqual(prior.Details, outcome.Details) {
			return message, &OperationError{Code: "idempotency_conflict"}
		}
		return message, nil
	}
	message, err = finishMessage(ctx, q, session, message, outcome, raw)
	if err != nil {
		return message, err
	}
	if outcome.Status == "unknown" {
		// A callback exception is an application failure, not evidence that the
		// Computer was lost. Stop admitting Turn work while the runtime drains
		// and publishes its captured failure. Keep any existing stop intent.
		if !turn.InterruptRequestedAt.Valid {
			if _, err = q.BeginSessionTurnSettlement(ctx, db.BeginSessionTurnSettlementParams{
				EnvironmentID: session.EnvironmentID, SessionID: session.ID, ID: turn.ID,
			}); err != nil {
				return message, err
			}
		}
		err = rejectQueuedMessages(ctx, q, session, turn.ID, "handler_failed")
	}
	return message, err
}

func finishMessage(ctx context.Context, q db.Querier, session db.Session, message db.SessionMessage, outcome MessageOutcome, raw []byte) (db.SessionMessage, error) {
	updated, err := q.FinishSessionMessage(ctx, db.FinishSessionMessageParams{EnvironmentID: session.EnvironmentID, SessionID: session.ID, ID: message.ID, ExpectedStatus: message.Status, Status: outcome.Status, Outcome: raw})
	if err != nil {
		return updated, err
	}
	body, _ := json.Marshal(struct {
		MessageID uuid.UUID       `json:"message_id"`
		Code      string          `json:"code,omitempty"`
		Details   json.RawMessage `json:"details,omitempty"`
	}{pgvalue.MustUUIDValue(message.ID), outcome.Code, outcome.Details})
	_, err = appendLifecycleEvent(ctx, q, session, message.TurnID, message.ID, "message."+outcome.Status, body, pgtype.UUID{})
	return updated, err
}

func rejectQueuedMessages(ctx context.Context, q db.Querier, session db.Session, turnID pgtype.UUID, code string) error {
	messages, err := q.ListUnsettledSessionMessages(ctx, db.ListUnsettledSessionMessagesParams{EnvironmentID: session.EnvironmentID, SessionID: session.ID, TurnID: turnID})
	if err != nil {
		return err
	}
	for _, message := range messages {
		if message.Status != "accepted" {
			continue
		}
		outcome := MessageOutcome{Status: "rejected", Code: code}
		raw, _ := json.Marshal(outcome)
		if _, err := finishMessage(ctx, q, session, message, outcome, raw); err != nil {
			return err
		}
	}
	return nil
}

func BeginSettlement(ctx context.Context, tx pgx.Tx, scope run.TurnScope) (db.SessionTurn, error) {
	q := db.New(tx)
	locked, err := run.LockTurn(ctx, tx, scope)
	if err != nil {
		return db.SessionTurn{}, err
	}
	session, turn := locked.Session(), locked.Turn()
	if _, err = locked.Validate(); err != nil {
		return turn, err
	}
	turn, err = q.BeginSessionTurnSettlement(ctx, db.BeginSessionTurnSettlementParams{EnvironmentID: session.EnvironmentID, SessionID: session.ID, ID: turn.ID})
	if err != nil {
		return turn, err
	}
	return turn, rejectQueuedMessages(ctx, q, session, turn.ID, "turn_settling")
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
func finishUnsettledMessages(ctx context.Context, q db.Querier, session db.Session, turnID pgtype.UUID, reason string) error {
	messages, err := q.ListUnsettledSessionMessages(ctx, db.ListUnsettledSessionMessagesParams{EnvironmentID: session.EnvironmentID, SessionID: session.ID, TurnID: turnID})
	if err != nil {
		return err
	}
	for _, message := range messages {
		outcome := MessageOutcome{Status: "rejected", Code: "turn_stopping"}
		if message.Status == "handling" {
			outcome = MessageOutcome{Status: "unknown", Code: reason}
		}
		raw, _ := json.Marshal(outcome)
		if _, err = finishMessage(ctx, q, session, message, outcome, raw); err != nil {
			return err
		}
	}
	return nil
}
