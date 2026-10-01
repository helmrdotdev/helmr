package session

import (
	"context"
	"encoding/json"
	"errors"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/idempotency"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// ActivateTurn is part of input delivery's transaction. It does not acknowledge input.
func ActivateTurn(ctx context.Context, tx pgx.Tx, scope run.TurnScope) (db.SessionTurn, error) {
	q := db.New(tx)
	locked, err := run.LockTurn(ctx, tx, scope)
	if err != nil {
		return db.SessionTurn{}, err
	}
	session, input := locked.Session(), locked.Turn()
	currentRun, err := q.GetRun(ctx, db.GetRunParams{EnvironmentID: session.EnvironmentID, ID: session.CurrentRunID})
	if err != nil {
		return db.SessionTurn{}, turnError(err)
	}
	if currentRun.ID != pgvalue.UUID(scope.RunID) || currentRun.CurrentAttemptNumber != scope.AttemptNumber || (currentRun.Status != db.RunStatusRunning && currentRun.Status != db.RunStatusWaiting) {
		return db.SessionTurn{}, run.ErrTurnScope
	}
	if session.DispatchHoldID.Valid {
		return db.SessionTurn{}, run.ErrTurnStopped
	}
	if session.ActiveTurnID.Valid {
		if session.CurrentRunID == input.RunID && input.RunGeneration.Int64 == session.RunGeneration && input.Status == "running" && session.ActiveTurnID == input.ID && input.RunID == pgvalue.UUID(scope.RunID) && input.AttemptNumber.Int32 == scope.AttemptNumber {
			return input, nil
		}
		return db.SessionTurn{}, run.ErrTurnNotActive
	}
	if input.Status != "queued" || input.Sequence != session.CommittedInputSequence+1 {
		return db.SessionTurn{}, run.ErrTurnNotActive
	}
	result, err := q.ActivateSessionTurn(ctx, db.ActivateSessionTurnParams{EnvironmentID: session.EnvironmentID, SessionID: session.ID, TurnID: input.ID, RunID: pgvalue.UUID(scope.RunID), AttemptNumber: pgtype.Int4{Int32: scope.AttemptNumber, Valid: true}, InputSequence: input.Sequence})
	if err != nil {
		return result, turnError(err)
	}
	scope.RunGeneration = session.RunGeneration
	_, err = appendEvent(ctx, q, scope, "turn.started", []byte(`{}`))
	return result, err
}

type InterruptReceipt struct {
	ID        uuid.UUID `json:"id"`
	Status    string    `json:"status"`
	Code      string    `json:"code,omitempty"`
	SessionID uuid.UUID `json:"session_id"`
	TurnID    uuid.UUID `json:"turn_id"`
	RunID     uuid.UUID `json:"run_id"`
	HoldID    uuid.UUID `json:"hold_id"`
	EventID   uuid.UUID `json:"event_id"`
}

// InterruptTurn admits exact stop intent under the pre-acquired owned graph.
// A parked execution is retired by that same graph; a live Lease converges later.
// Historical receipts admit no new retirement and prove no native quiescence.
func InterruptTurn(ctx context.Context, tx pgx.Tx, environmentID, sessionID, turnID uuid.UUID, key string, graph run.OwnedFinalization) (InterruptReceipt, error) {
	q := db.New(tx)
	session, err := lockSession(ctx, tx, Target{EnvironmentID: environmentID, SessionID: sessionID})
	if err != nil {
		return InterruptReceipt{}, err
	}
	input, err := q.LockSessionTurnInput(ctx, db.LockSessionTurnInputParams{EnvironmentID: session.EnvironmentID, SessionID: session.ID, ID: pgvalue.UUID(turnID)})
	if errors.Is(err, pgx.ErrNoRows) {
		return InterruptReceipt{}, &OperationError{Code: "turn_not_found"}
	}
	if err != nil {
		return InterruptReceipt{}, err
	}
	if key == "" {
		key = uuid.NewV7().String()
	}
	request, err := idempotency.NewTurnInterruptRequest(environmentID, sessionID, turnID, key)
	if err != nil {
		return InterruptReceipt{}, err
	}
	claims, err := idempotency.TransactionFor(tx)
	if err != nil {
		return InterruptReceipt{}, err
	}
	claim, err := claims.Acquire(ctx, request)
	if err != nil {
		return InterruptReceipt{}, err
	}
	if claim.Claim.Status == "completed" {
		var receipt InterruptReceipt
		err = json.Unmarshal(claim.Claim.Receipt, &receipt)
		return receipt, err
	}
	receipt := InterruptReceipt{ID: pgvalue.MustUUIDValue(claim.Claim.ID), SessionID: sessionID, TurnID: turnID, Status: "rejected"}
	if session.ActiveTurnID != input.ID || input.Status != "running" || session.CurrentRunID != input.RunID || session.RunGeneration != input.RunGeneration.Int64 || (session.Status != "open" && session.Status != "closing") {
		receipt.Code = "turn_not_active"
	} else if session.DispatchHoldID.Valid || input.InterruptRequestedAt.Valid {
		receipt.Code = "turn_stopping"
	}
	if receipt.Code != "" {
		raw, _ := json.Marshal(receipt)
		_, err = claims.Complete(ctx, claim.Claim, raw)
		return receipt, err
	}
	if _, err = q.RequestSessionTurnInterrupt(ctx, db.RequestSessionTurnInterruptParams{EnvironmentID: session.EnvironmentID, SessionID: session.ID, TurnID: input.ID}); err != nil {
		return InterruptReceipt{}, turnError(err)
	}
	session, err = run.HoldSessionExecution(ctx, q, session, input.AttemptNumber.Int32, "interrupt_requested")
	if err != nil {
		return InterruptReceipt{}, err
	}
	holdID := pgvalue.MustUUIDValue(session.DispatchHoldID)
	scope := run.TurnScope{EnvironmentID: environmentID, SessionID: sessionID, TurnID: turnID, RunID: pgvalue.MustUUIDValue(input.RunID), AttemptNumber: input.AttemptNumber.Int32, RunGeneration: input.RunGeneration.Int64}
	data, _ := json.Marshal(struct {
		HoldID uuid.UUID `json:"hold_id"`
	}{holdID})
	event, err := appendEvent(ctx, q, scope, "turn.interrupt_requested", data)
	if err != nil {
		return InterruptReceipt{}, err
	}
	if err := rejectQueuedMessages(ctx, q, session, input.ID, "turn_stopping"); err != nil {
		return InterruptReceipt{}, err
	}
	if _, err = graph.RequestHeldSessionStop(ctx, holdID); err != nil {
		return InterruptReceipt{}, err
	}
	receipt = InterruptReceipt{ID: pgvalue.MustUUIDValue(claim.Claim.ID), Status: "accepted", SessionID: sessionID, TurnID: turnID, RunID: scope.RunID, HoldID: holdID, EventID: pgvalue.MustUUIDValue(event.ID)}
	raw, _ := json.Marshal(receipt)
	_, err = claims.Complete(ctx, claim.Claim, raw)
	return receipt, err
}

// OutputReceipt separates committed business rejection from transaction failure.
// Event receipts are historical; a replay is admitted only while authority remains live.
type OutputReceipt struct {
	EventID uuid.UUID       `json:"event_id,omitempty"`
	Code    string          `json:"code,omitempty"`
	Event   db.SessionEvent `json:"-"`
}

func AppendTurnOutput(ctx context.Context, tx pgx.Tx, scope run.TurnScope, key string, data json.RawMessage) (OutputReceipt, error) {
	q := db.New(tx)
	// New Turn operations lock Session/input before their disjoint claim namespaces.
	// No code may acquire a turn.output.write or turn.interrupt claim before Session.
	locked, err := run.LockTurn(ctx, tx, scope)
	if err != nil {
		return OutputReceipt{}, err
	}
	request, err := idempotency.NewTurnOutputRequest(scope.EnvironmentID, scope.SessionID, key, idempotency.TurnProducer{TurnID: scope.TurnID, RunID: scope.RunID, AttemptNumber: scope.AttemptNumber, RunGeneration: scope.RunGeneration, MessageDeliveryID: scope.MessageDeliveryID}, data)
	if err != nil {
		return OutputReceipt{}, err
	}
	claims, err := idempotency.TransactionFor(tx)
	if err != nil {
		return OutputReceipt{}, err
	}
	claim, err := claims.Acquire(ctx, request)
	if err != nil {
		return OutputReceipt{}, err
	}
	var receipt OutputReceipt
	if claim.Claim.Status == "completed" {
		if err := json.Unmarshal(claim.Claim.Receipt, &receipt); err != nil {
			return receipt, err
		}
		if receipt.Code != "" {
			return receipt, nil
		}
	}
	if err = validateOutput(ctx, q, locked, scope); err != nil {
		switch {
		case errors.As(err, new(*OperationError)):
			var operation *OperationError
			errors.As(err, &operation)
			receipt.Code = operation.Code
		case errors.Is(err, run.ErrTurnStopped):
			receipt.Code = "turn_stopping"
		case errors.Is(err, run.ErrTurnScope):
			receipt.Code = "stale_execution"
		case errors.Is(err, run.ErrTurnNotActive):
			receipt.Code = "turn_not_active"
		default:
			return OutputReceipt{}, err
		}
		// Do not overwrite an earlier accepted receipt after authority was revoked.
		if claim.Claim.Status != "completed" {
			raw, _ := json.Marshal(receipt)
			_, err = claims.Complete(ctx, claim.Claim, raw)
			if err != nil {
				return OutputReceipt{}, err
			}
		}
		return receipt, nil
	}
	if claim.Claim.Status == "completed" {
		event, err := q.GetSessionEvent(ctx, db.GetSessionEventParams{EnvironmentID: pgvalue.UUID(scope.EnvironmentID), SessionID: pgvalue.UUID(scope.SessionID), ID: pgvalue.UUID(receipt.EventID)})
		if err == nil && (event.TurnID != pgvalue.UUID(scope.TurnID) || event.ProducerRunID != pgvalue.UUID(scope.RunID) || event.ProducerAttemptNumber.Int32 != scope.AttemptNumber || event.RunGeneration.Int64 != scope.RunGeneration) {
			err = run.ErrTurnScope
		}
		receipt.Event = event
		return receipt, err
	}
	event, err := appendEvent(ctx, q, scope, "output", data)
	if err != nil {
		return receipt, err
	}
	receipt.EventID, receipt.Event = pgvalue.MustUUIDValue(event.ID), event
	raw, _ := json.Marshal(receipt)
	_, err = claims.Complete(ctx, claim.Claim, raw)
	return receipt, err
}

// SettleTurn commits the logical result and input cursor under execution authority.
// Computer persistence has a separate lifecycle.
func SettleTurn(ctx context.Context, tx pgx.Tx, scope run.TurnScope, status string, data json.RawMessage, fingerprint string) (db.SessionEvent, error) {
	q := db.New(tx)
	if (status != "completed" && status != "failed") || (len(data) != 0 && !json.Valid(data)) || (status == "failed" && len(data) == 0) {
		return db.SessionEvent{}, run.ErrTurnScope
	}
	input, err := run.ValidateTurn(ctx, tx, scope)
	if err != nil {
		return db.SessionEvent{}, err
	}
	if !input.SettlementStartedAt.Valid {
		return db.SessionEvent{}, &OperationError{Code: "turn_unsettled"}
	}
	unsettled, err := q.SessionTurnHasUnsettledWork(ctx, db.SessionTurnHasUnsettledWorkParams{SessionID: input.SessionID, TurnID: input.ID})
	if err != nil {
		return db.SessionEvent{}, err
	}
	if !unsettled.Valid || unsettled.Bool {
		return db.SessionEvent{}, &OperationError{Code: "turn_unsettled"}
	}
	body := struct {
		Result json.RawMessage `json:"result,omitempty"`
		Error  json.RawMessage `json:"error,omitempty"`
	}{}
	if status == "completed" {
		body.Result = data
	} else {
		body.Error = data
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return db.SessionEvent{}, err
	}
	event, err := appendEvent(ctx, q, scope, "turn."+status, encoded)
	if err != nil {
		return event, err
	}
	_, err = q.SettleSessionTurn(ctx, db.SettleSessionTurnParams{EnvironmentID: pgvalue.UUID(scope.EnvironmentID), SessionID: pgvalue.UUID(scope.SessionID), TurnID: input.ID, RunGeneration: scope.RunGeneration, InputSequence: input.Sequence, Status: status, EventID: event.ID, Fingerprint: pgvalue.Text(fingerprint)})
	return event, turnError(err)
}

func appendEvent(ctx context.Context, q db.Querier, scope run.TurnScope, kind string, data []byte) (db.SessionEvent, error) {
	turnID := pgtype.UUID{}
	if scope.TurnID != uuid.Nil() {
		turnID = pgvalue.UUID(scope.TurnID)
	}
	return q.AppendSessionEvent(ctx, db.AppendSessionEventParams{ID: pgvalue.UUID(uuid.NewV7()), EnvironmentID: pgvalue.UUID(scope.EnvironmentID), SessionID: pgvalue.UUID(scope.SessionID), TurnID: turnID, Kind: kind, Data: data, ProducerRunID: pgvalue.UUID(scope.RunID), ProducerAttemptNumber: pgtype.Int4{Int32: scope.AttemptNumber, Valid: true}, RunGeneration: pgtype.Int8{Int64: scope.RunGeneration, Valid: true}})
}
func turnError(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return run.ErrTurnScope
	}
	return err
}
