package session

import (
	"context"
	"encoding/json"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/jsoncanon"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
)

func validateOutput(ctx context.Context, q db.Querier, actor db.Session, turn db.SessionTurn, scope TurnScope) error {
	if _, err := validateTurn(actor, turn, scope); err != nil {
		return err
	}
	if scope.MessageDeliveryID == uuid.Nil() {
		if turn.SettlementStartedAt.Valid {
			return &OperationError{Code: "turn_unsettled"}
		}
		return nil
	}
	message, err := q.GetSessionMessageDelivery(ctx, db.GetSessionMessageDeliveryParams{EnvironmentID: actor.EnvironmentID, SessionID: actor.ID, DeliveryID: pgvalue.UUID(scope.MessageDeliveryID)})
	if err != nil {
		return turnError(err)
	}
	current, err := q.GetRun(ctx, db.GetRunParams{EnvironmentID: actor.EnvironmentID, ID: actor.CurrentRunID})
	if err != nil {
		return err
	}
	if message.Status != "handling" || message.TurnID != turn.ID || message.RunID != turn.RunID || message.AttemptNumber != scope.AttemptNumber || message.RunGeneration != scope.RunGeneration || message.DeliveryRunLeaseID != current.CurrentRunLeaseID {
		return ErrTurnScope
	}
	return nil
}

// AppendSessionOutput belongs to the current Actor entrypoint outside a Turn.
// Its historical event receipt never authorizes a later execution or a held Run.
func AppendSessionOutput(ctx context.Context, q db.Querier, scope TurnScope, key string, data json.RawMessage) (OutputReceipt, error) {
	if scope.TurnID != uuid.Nil() || scope.RunID == uuid.Nil() || scope.AttemptNumber <= 0 || scope.RunGeneration <= 0 || !json.Valid(data) || len(data) > 1<<20 {
		return OutputReceipt{}, &OperationError{Code: "invalid_request"}
	}
	data, err := jsoncanon.Transform(data)
	if err != nil {
		return OutputReceipt{}, &OperationError{Code: "invalid_request"}
	}
	actor, err := lockSession(ctx, q, Target{EnvironmentID: scope.EnvironmentID, SessionID: scope.SessionID})
	if err != nil {
		return OutputReceipt{}, err
	}
	claim, err := claimOperation(ctx, q, ControlRequest{Target: Target{EnvironmentID: scope.EnvironmentID, SessionID: scope.SessionID}, IdempotencyKey: key}, "session.output.write", struct {
		RunID         uuid.UUID       `json:"run_id"`
		AttemptNumber int32           `json:"attempt_number"`
		RunGeneration int64           `json:"run_generation"`
		Data          json.RawMessage `json:"data"`
	}{scope.RunID, scope.AttemptNumber, scope.RunGeneration, data})
	if err != nil {
		return OutputReceipt{}, err
	}
	var receipt OutputReceipt
	if claim.Status == "completed" {
		if err = json.Unmarshal(claim.Receipt, &receipt); err != nil {
			return receipt, err
		}
		if receipt.Code != "" {
			return receipt, nil
		}
	}
	current, err := q.GetRun(ctx, db.GetRunParams{EnvironmentID: actor.EnvironmentID, ID: pgvalue.UUID(scope.RunID)})
	if err != nil {
		return receipt, err
	}
	switch {
	case actor.CurrentRunID != current.ID || actor.RunGeneration != scope.RunGeneration || current.CurrentAttemptNumber != scope.AttemptNumber:
		receipt.Code = "stale_execution"
	case actor.DispatchHoldID.Valid:
		receipt.Code = "session_held"
	case actor.ActiveTurnID.Valid:
		receipt.Code = "turn_active"
	case actor.Status != "open" && actor.Status != "closing":
		receipt.Code = "session_not_open"
	}
	if receipt.Code != "" {
		if claim.Status != "completed" {
			err = finishOperation(ctx, q, claim, receipt)
		}
		return receipt, err
	}
	if claim.Status == "completed" {
		receipt.Event, err = q.GetSessionEvent(ctx, db.GetSessionEventParams{EnvironmentID: actor.EnvironmentID, SessionID: actor.ID, ID: pgvalue.UUID(receipt.EventID)})
		return receipt, err
	}
	event, err := appendEvent(ctx, q, scope, "output", data)
	if err != nil {
		return receipt, err
	}
	receipt.EventID, receipt.Event = pgvalue.MustUUIDValue(event.ID), event
	return receipt, finishOperation(ctx, q, claim, receipt)
}
