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

var ErrTurnNotActive = errors.New("Turn is not active")
var ErrTurnStopped = errors.New("Turn interruption has been accepted")
var ErrTurnScope = errors.New("Turn producer scope is stale")

// TurnScope addresses one input under one execution. It is not a capability.
// Callers must also validate their authenticated worker/lease authority in this transaction.
type TurnScope struct {
	EnvironmentID     uuid.UUID
	SessionID         uuid.UUID
	TurnID            uuid.UUID
	RunID             uuid.UUID
	AttemptNumber     int32
	RunGeneration     int64
	MessageDeliveryID uuid.UUID
}

// ActivateTurn is part of input delivery's transaction. It does not acknowledge input.
func ActivateTurn(ctx context.Context, q db.Querier, scope TurnScope) (db.SessionTurn, error) {
	actor, input, err := lockTurn(ctx, q, scope)
	if err != nil {
		return db.SessionTurn{}, err
	}
	currentRun, err := q.GetRun(ctx, db.GetRunParams{EnvironmentID: actor.EnvironmentID, ID: actor.CurrentRunID})
	if err != nil {
		return db.SessionTurn{}, turnError(err)
	}
	if currentRun.ID != pgvalue.UUID(scope.RunID) || currentRun.CurrentAttemptNumber != scope.AttemptNumber || (currentRun.Status != db.RunStatusRunning && currentRun.Status != db.RunStatusWaiting) {
		return db.SessionTurn{}, ErrTurnScope
	}
	if actor.DispatchHoldID.Valid {
		return db.SessionTurn{}, ErrTurnStopped
	}
	if actor.ActiveTurnID.Valid {
		if actor.CurrentRunID == input.RunID && input.RunGeneration.Int64 == actor.RunGeneration && input.Status == "running" && actor.ActiveTurnID == input.ID && input.RunID == pgvalue.UUID(scope.RunID) && input.AttemptNumber.Int32 == scope.AttemptNumber {
			return input, nil
		}
		return db.SessionTurn{}, ErrTurnNotActive
	}
	if input.Status != "queued" || input.Sequence != actor.CommittedInputSequence+1 {
		return db.SessionTurn{}, ErrTurnNotActive
	}
	result, err := q.ActivateSessionTurn(ctx, db.ActivateSessionTurnParams{EnvironmentID: actor.EnvironmentID, SessionID: actor.ID, TurnID: input.ID, RunID: pgvalue.UUID(scope.RunID), AttemptNumber: pgtype.Int4{Int32: scope.AttemptNumber, Valid: true}, InputSequence: input.Sequence})
	if err != nil {
		return result, turnError(err)
	}
	scope.RunGeneration = actor.RunGeneration
	_, err = appendEvent(ctx, q, scope, "turn.started", []byte(`{}`), pgtype.UUID{})
	return result, err
}

func lockTurn(ctx context.Context, q db.Querier, scope TurnScope) (db.Session, db.SessionTurn, error) {
	if scope.EnvironmentID == uuid.Nil() || scope.SessionID == uuid.Nil() || scope.TurnID == uuid.Nil() {
		return db.Session{}, db.SessionTurn{}, ErrTurnScope
	}
	actor, err := q.LockSessionTurnAuthority(ctx, db.LockSessionTurnAuthorityParams{EnvironmentID: pgvalue.UUID(scope.EnvironmentID), ID: pgvalue.UUID(scope.SessionID)})
	if err != nil {
		return actor, db.SessionTurn{}, turnError(err)
	}
	input, err := q.LockSessionTurnInput(ctx, db.LockSessionTurnInputParams{EnvironmentID: actor.EnvironmentID, SessionID: actor.ID, ID: pgvalue.UUID(scope.TurnID)})
	return actor, input, turnError(err)
}

func ValidateTurn(ctx context.Context, q db.Querier, scope TurnScope) (db.SessionTurn, error) {
	actor, input, err := lockTurn(ctx, q, scope)
	if err != nil {
		return input, err
	}
	return validateTurn(actor, input, scope)
}

func validateTurn(actor db.Session, input db.SessionTurn, scope TurnScope) (db.SessionTurn, error) {
	if scope.RunGeneration <= 0 || scope.RunID == uuid.Nil() || scope.AttemptNumber <= 0 || input.RunGeneration.Int64 != scope.RunGeneration || input.RunID != pgvalue.UUID(scope.RunID) || input.AttemptNumber.Int32 != scope.AttemptNumber || actor.CurrentRunID != input.RunID || actor.RunGeneration != scope.RunGeneration {
		return input, ErrTurnScope
	}
	if actor.ActiveTurnID != input.ID || input.Status != "running" || (actor.Status != "open" && actor.Status != "closing") {
		return input, ErrTurnNotActive
	}
	if actor.DispatchHoldID.Valid || input.InterruptRequestedAt.Valid {
		return input, ErrTurnStopped
	}
	return input, nil
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
func InterruptTurn(ctx context.Context, q db.Querier, environmentID, sessionID, turnID uuid.UUID, key string, graph run.OwnedFinalization) (InterruptReceipt, error) {
	actor, err := lockSession(ctx, q, Target{EnvironmentID: environmentID, SessionID: sessionID})
	if err != nil {
		return InterruptReceipt{}, err
	}
	input, err := q.LockSessionTurnInput(ctx, db.LockSessionTurnInputParams{EnvironmentID: actor.EnvironmentID, SessionID: actor.ID, ID: pgvalue.UUID(turnID)})
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
	claims, err := idempotency.TransactionForQueries(q)
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
	if actor.ActiveTurnID != input.ID || input.Status != "running" || actor.CurrentRunID != input.RunID || actor.RunGeneration != input.RunGeneration.Int64 || (actor.Status != "open" && actor.Status != "closing") {
		receipt.Code = "turn_not_active"
	} else if actor.DispatchHoldID.Valid || input.InterruptRequestedAt.Valid {
		receipt.Code = "turn_stopping"
	}
	if receipt.Code != "" {
		raw, _ := json.Marshal(receipt)
		_, err = claims.Complete(ctx, claim.Claim, raw)
		return receipt, err
	}
	if _, err = q.RequestSessionTurnInterrupt(ctx, db.RequestSessionTurnInterruptParams{EnvironmentID: actor.EnvironmentID, SessionID: actor.ID, TurnID: input.ID}); err != nil {
		return InterruptReceipt{}, turnError(err)
	}
	actor, err = run.HoldSessionExecution(ctx, q, actor, input.AttemptNumber.Int32, "interrupt_requested")
	if err != nil {
		return InterruptReceipt{}, err
	}
	holdID := pgvalue.MustUUIDValue(actor.DispatchHoldID)
	scope := TurnScope{EnvironmentID: environmentID, SessionID: sessionID, TurnID: turnID, RunID: pgvalue.MustUUIDValue(input.RunID), AttemptNumber: input.AttemptNumber.Int32, RunGeneration: input.RunGeneration.Int64}
	data, _ := json.Marshal(struct {
		HoldID uuid.UUID `json:"hold_id"`
	}{holdID})
	event, err := appendEvent(ctx, q, scope, "turn.interrupt_requested", data, pgtype.UUID{})
	if err != nil {
		return InterruptReceipt{}, err
	}
	if err := rejectQueuedMessages(ctx, q, actor, input.ID, "turn_stopping"); err != nil {
		return InterruptReceipt{}, err
	}
	if _, err = graph.RequestHeldActorStop(ctx, holdID); err != nil {
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

func AppendTurnOutput(ctx context.Context, q db.Querier, scope TurnScope, key string, data json.RawMessage) (OutputReceipt, error) {
	// New Turn operations lock Session/input before their disjoint claim namespaces.
	// No code may acquire a turn.output.write or turn.interrupt claim before Session.
	actor, input, err := lockTurn(ctx, q, scope)
	if err != nil {
		return OutputReceipt{}, err
	}
	request, err := idempotency.NewTurnOutputRequest(scope.EnvironmentID, scope.SessionID, key, idempotency.TurnProducer{TurnID: scope.TurnID, RunID: scope.RunID, AttemptNumber: scope.AttemptNumber, RunGeneration: scope.RunGeneration, MessageDeliveryID: scope.MessageDeliveryID}, data)
	if err != nil {
		return OutputReceipt{}, err
	}
	claims, err := idempotency.TransactionForQueries(q)
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
	if err = validateOutput(ctx, q, actor, input, scope); err != nil {
		switch {
		case errors.As(err, new(*OperationError)):
			var operation *OperationError
			errors.As(err, &operation)
			receipt.Code = operation.Code
		case errors.Is(err, ErrTurnStopped):
			receipt.Code = "turn_stopping"
		case errors.Is(err, ErrTurnScope):
			receipt.Code = "stale_execution"
		case errors.Is(err, ErrTurnNotActive):
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
			err = ErrTurnScope
		}
		receipt.Event = event
		return receipt, err
	}
	event, err := appendEvent(ctx, q, scope, "output", data, pgtype.UUID{})
	if err != nil {
		return receipt, err
	}
	receipt.EventID, receipt.Event = pgvalue.MustUUIDValue(event.ID), event
	raw, _ := json.Marshal(receipt)
	_, err = claims.Complete(ctx, claim.Claim, raw)
	return receipt, err
}

// SettleTurn must share the transaction that validated and advanced the physical
// Workspace proof. It never accepts an arbitrary public Workspace version claim.
func SettleTurn(ctx context.Context, q db.Querier, scope TurnScope, status string, data json.RawMessage, workspaceVersion pgtype.UUID, fingerprint string) (db.SessionEvent, error) {
	if (status != "completed" && status != "failed") || !workspaceVersion.Valid || (len(data) != 0 && !json.Valid(data)) || (status == "failed" && len(data) == 0) {
		return db.SessionEvent{}, ErrTurnScope
	}
	input, err := ValidateTurn(ctx, q, scope)
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
		WorkspaceVersionID string          `json:"workspace_version_id"`
		Result             json.RawMessage `json:"result,omitempty"`
		Error              json.RawMessage `json:"error,omitempty"`
	}{WorkspaceVersionID: pgvalue.UUIDString(workspaceVersion)}
	if status == "completed" {
		body.Result = data
	} else {
		body.Error = data
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return db.SessionEvent{}, err
	}
	event, err := appendEvent(ctx, q, scope, "turn."+status, encoded, workspaceVersion)
	if err != nil {
		return event, err
	}
	_, err = q.SettleSessionTurn(ctx, db.SettleSessionTurnParams{EnvironmentID: pgvalue.UUID(scope.EnvironmentID), SessionID: pgvalue.UUID(scope.SessionID), TurnID: input.ID, RunGeneration: scope.RunGeneration, InputSequence: input.Sequence, Status: status, EventID: event.ID, Fingerprint: pgvalue.Text(fingerprint)})
	return event, turnError(err)
}

func appendEvent(ctx context.Context, q db.Querier, scope TurnScope, kind string, data []byte, workspaceVersion pgtype.UUID) (db.SessionEvent, error) {
	turnID := pgtype.UUID{}
	if scope.TurnID != uuid.Nil() {
		turnID = pgvalue.UUID(scope.TurnID)
	}
	return q.AppendSessionEvent(ctx, db.AppendSessionEventParams{ID: pgvalue.UUID(uuid.NewV7()), EnvironmentID: pgvalue.UUID(scope.EnvironmentID), SessionID: pgvalue.UUID(scope.SessionID), TurnID: turnID, Kind: kind, Data: data, ProducerRunID: pgvalue.UUID(scope.RunID), ProducerAttemptNumber: pgtype.Int4{Int32: scope.AttemptNumber, Valid: true}, RunGeneration: pgtype.Int8{Int64: scope.RunGeneration, Valid: true}, WorkspaceVersionID: workspaceVersion})
}
func turnError(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrTurnScope
	}
	return err
}
