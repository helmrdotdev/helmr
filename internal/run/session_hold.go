package run

import (
	"context"
	"encoding/json"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/idempotency"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

type SessionCancellationReceipt struct {
	ID        uuid.UUID `json:"id"`
	RunID     uuid.UUID `json:"run_id"`
	SessionID uuid.UUID `json:"session_id"`
	HoldID    uuid.UUID `json:"hold_id"`
	Status    string    `json:"status"`
	Code      string    `json:"code,omitempty"`
}

type CancellationRejectionError struct{ Code string }

func (e *CancellationRejectionError) Error() string { return e.Code }

func acceptSessionRunCancellation(ctx context.Context, tx pgx.Tx, request CancellationRequest, run db.Run, graph OwnedFinalization) (SessionCancellationReceipt, error) {
	q := db.New(tx)
	session, err := q.LockSessionTurnAuthority(ctx, db.LockSessionTurnAuthorityParams{EnvironmentID: run.EnvironmentID, ID: run.SessionID})
	if err != nil {
		return SessionCancellationReceipt{}, err
	}
	// Dispatch and Run lifecycle mutation share this Session lock. Reload the
	// attempt/Lease after acquiring it so pre-lock observations cannot orphan a
	// stopped Run that just parked or completed an assignment transition.
	run, err = q.GetRun(ctx, db.GetRunParams{EnvironmentID: run.EnvironmentID, ID: run.ID})
	if err != nil {
		return SessionCancellationReceipt{}, err
	}
	key := request.IdempotencyKey
	if key == "" {
		key = uuid.NewV7().String()
	}
	claimRequest, err := idempotency.NewSessionOperationRequest(request.EnvironmentID, pgvalue.MustUUIDValue(session.ID), key, "session.run.cancel", struct {
		RunID uuid.UUID `json:"run_id"`
	}{request.RunID})
	if err != nil {
		return SessionCancellationReceipt{}, err
	}
	claims, err := idempotency.TransactionForQueries(q)
	if err != nil {
		return SessionCancellationReceipt{}, err
	}
	claim, err := claims.Acquire(ctx, claimRequest)
	if err != nil {
		return SessionCancellationReceipt{}, err
	}
	var receipt SessionCancellationReceipt
	if claim.Claim.Status == "completed" {
		err = json.Unmarshal(claim.Claim.Receipt, &receipt)
		return receipt, err
	}
	receipt = SessionCancellationReceipt{ID: pgvalue.MustUUIDValue(claim.Claim.ID), RunID: request.RunID, SessionID: pgvalue.MustUUIDValue(session.ID), Status: "accepted"}
	switch {
	case session.CurrentRunID != run.ID:
		receipt.Code = "stale_execution"
	case session.ActiveTurnID.Valid:
		receipt.Code = "turn_not_active"
	case session.DispatchHoldID.Valid:
		receipt.Code = "session_held"
	case session.Status != "open" && session.Status != "closing":
		receipt.Code = "session_not_open"
	default:
		session, err = HoldSessionExecution(ctx, q, session, run.CurrentAttemptNumber, "interrupt_requested")
		if err != nil {
			return receipt, err
		}
		if _, err = graph.RequestHeldSessionStop(ctx, pgvalue.MustUUIDValue(session.DispatchHoldID)); err != nil {
			return receipt, err
		}
		receipt.HoldID = pgvalue.MustUUIDValue(session.DispatchHoldID)
	}
	if receipt.Code != "" {
		receipt.Status = "rejected"
	}
	raw, _ := json.Marshal(receipt)
	_, err = claims.Complete(ctx, claim.Claim, raw)
	return receipt, err
}

// HoldSessionExecution records loss before the caller fences physical resources.
// The caller must hold the Session row lock and supply its current Run attempt.
// It retains Session/input/head authority and durable interrupt intent for
// lifecycle convergence; the hold itself proves no physical stop.
func HoldSessionExecution(ctx context.Context, q db.Querier, session db.Session, attempt int32, reason string) (db.Session, error) {
	if session.DispatchHoldID.Valid && session.DispatchHoldReason.String == reason {
		return session, nil
	}
	hold := uuid.NewV7()
	session, err := q.HoldSessionExecution(ctx, db.HoldSessionExecutionParams{EnvironmentID: session.EnvironmentID, SessionID: session.ID, RunID: session.CurrentRunID, AttemptNumber: pgtype.Int4{Int32: attempt, Valid: true}, HoldID: pgvalue.UUID(hold), Reason: pgvalue.Text(reason)})
	if err != nil {
		return session, err
	}
	body, _ := json.Marshal(map[string]any{"hold_id": hold, "reason": reason})
	_, err = q.AppendSessionEvent(ctx, db.AppendSessionEventParams{ID: pgvalue.UUID(uuid.NewV7()), EnvironmentID: session.EnvironmentID, SessionID: session.ID, TurnID: session.ActiveTurnID, Kind: "session.held", Data: body, ProducerRunID: session.CurrentRunID, ProducerAttemptNumber: pgtype.Int4{Int32: attempt, Valid: true}, RunGeneration: pgtype.Int8{Int64: session.RunGeneration, Valid: true}})
	if err == nil && (reason == "recovery_required" || reason == "interrupt_requested") {
		err = q.CreateSessionLifecycleReconcileOutbox(ctx, db.CreateSessionLifecycleReconcileOutboxParams{ID: pgvalue.UUID(hold), EnvironmentID: session.EnvironmentID, SessionID: session.ID})
	}
	return session, err
}

// RequestHeldSessionStop cancels owned descendants and consuming wait associations
// without revoking a live parent's capture authority. With no parent Lease it also
// retires the parked Run. Neither action claims physical or external convergence.
// The caller acquires the owned graph before Session admission/recovery and passes
// the exact hold. Session/Turn/head stay bound for finalization or explicit repair.
func (g OwnedFinalization) RequestHeldSessionStop(ctx context.Context, holdID uuid.UUID) (bool, error) {
	if g.tx == nil || len(g.descendants) == 0 || g.descendants[0].id != g.currentRun || holdID == uuid.Nil() {
		return false, cancellationAuthority("held Actor retirement graph is invalid", nil)
	}
	target := g.descendants[0]
	if !target.sessionID.Valid {
		return false, cancellationAuthority("held Actor retirement requires an Actor", nil)
	}
	session, err := db.New(g.tx).LockSessionTurnAuthority(ctx, db.LockSessionTurnAuthorityParams{EnvironmentID: pgvalue.UUID(target.environmentID), ID: target.sessionID})
	if err != nil {
		return false, err
	}
	if session.CurrentRunID != pgvalue.UUID(target.id) || session.DispatchHoldID != pgvalue.UUID(holdID) ||
		session.DispatchHoldRunID != session.CurrentRunID || !session.DispatchHoldAttemptNumber.Valid ||
		session.DispatchHoldAttemptNumber.Int32 != target.currentAttemptNumber ||
		!session.DispatchHoldRunGeneration.Valid || session.DispatchHoldRunGeneration.Int64 != session.RunGeneration {
		return false, cancellationAuthority("held Actor retirement address is stale", nil)
	}
	if runStatusTerminal(target.status) {
		return false, nil
	}
	if _, err := g.CancelDescendants(ctx); err != nil {
		return false, err
	}
	if target.currentRunLeaseID.Valid {
		q := db.New(g.tx)
		waits, err := q.LockCancellationWaits(ctx, db.LockCancellationWaitsParams{RunIDs: []pgtype.UUID{pgvalue.UUID(target.id)}})
		if err != nil {
			return false, err
		}
		for _, wait := range waits {
			// Mid-checkpoint/resume execution cannot establish a cooperative
			// capture. Leave it fenced for the existing forced-loss path.
			if wait.SuspensionStatus != db.RunWaitStatusHot || wait.ConditionStatus != db.WaitStatusPending {
				continue
			}
			if _, err := q.FailHotRunWait(ctx, db.FailHotRunWaitParams{
				ID: wait.ID, RunID: wait.RunID, AttemptNumber: wait.AttemptNumber,
				CurrentRunLeaseID: wait.CurrentRunLeaseID, ExpectedRunRevision: wait.ExpectedRunRevision,
				ReasonCode:     pgvalue.Text("session_stopped"),
				ConditionError: []byte(`{"code":"session_stopped","retryable":false}`),
			}); err != nil {
				return false, err
			}
		}
		return false, nil
	}
	if err := cancelLockedRun(ctx, g.tx, target); err != nil {
		return false, err
	}
	return true, nil
}

// RetireHeldSessionIfUnleased leaves a live execution untouched during privileged recovery.
func (g OwnedFinalization) RetireHeldSessionIfUnleased(ctx context.Context, holdID uuid.UUID) (bool, error) {
	if len(g.descendants) == 0 {
		return false, cancellationAuthority("held Actor graph is invalid", nil)
	}
	if g.descendants[0].currentRunLeaseID.Valid {
		return false, nil
	}
	return g.RequestHeldSessionStop(ctx, holdID)
}
