package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/idempotency"
	"github.com/helmrdotdev/helmr/internal/jsoncanon"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// ErrComputerAuthority reports that a Session operation found its Session
// but could not lock the Session's Computer for admission: the Computer is
// gone from the Environment or has no initializing or committed head disk
// version. It wraps computer.ErrNotFound.
var ErrComputerAuthority = errors.New("session computer authority is unavailable")

// lockSession locks the Session's Computer and its unreclaimed Instance,
// then the Session. The Session is read without a lock first to find its
// Computer.
func lockSession(ctx context.Context, tx pgx.Tx, target Target) (db.Session, error) {
	q := db.New(tx)
	locator, err := q.GetSession(ctx, db.GetSessionParams{EnvironmentID: pgvalue.UUID(target.EnvironmentID), ID: pgvalue.UUID(target.SessionID)})
	if errors.Is(err, pgx.ErrNoRows) {
		return db.Session{}, &OperationError{Code: "session_not_found"}
	}
	if err != nil {
		return db.Session{}, err
	}
	if err := lockSessionComputer(ctx, tx, locator.EnvironmentID, locator.ComputerID); err != nil {
		return db.Session{}, err
	}
	s, err := q.LockSessionTurnAuthority(ctx, db.LockSessionTurnAuthorityParams{EnvironmentID: pgvalue.UUID(target.EnvironmentID), ID: pgvalue.UUID(target.SessionID)})
	if errors.Is(err, pgx.ErrNoRows) {
		return s, &OperationError{Code: "session_not_found"}
	}
	return s, err
}

// lockSessionComputer locks the Session's Computer for admission, then its
// unreclaimed Instance when it has one. A Computer that cannot be locked for
// admission returns ErrComputerAuthority.
func lockSessionComputer(ctx context.Context, tx pgx.Tx, environmentID, computerID pgtype.UUID) error {
	admission, err := computer.LockForAdmission(ctx, tx, pgvalue.MustUUIDValue(environmentID), pgvalue.MustUUIDValue(computerID))
	if errors.Is(err, computer.ErrNotFound) {
		return fmt.Errorf("%w: %w", ErrComputerAuthority, err)
	}
	if err != nil {
		return err
	}
	return admission.LockLiveInstance(ctx)
}

func claimOperation(ctx context.Context, tx pgx.Tx, request ControlRequest, name string, fingerprint any) (db.IdempotencyClaim, error) {
	key := request.IdempotencyKey
	if key == "" {
		key = uuid.NewV7().String()
	}
	r, err := idempotency.NewSessionOperationRequest(request.EnvironmentID, request.SessionID, key, name, fingerprint)
	if err != nil {
		return db.IdempotencyClaim{}, err
	}
	claims, err := idempotency.TransactionFor(tx)
	if err != nil {
		return db.IdempotencyClaim{}, err
	}
	result, err := claims.Acquire(ctx, r)
	return result.Claim, err
}

func finishOperation(ctx context.Context, tx pgx.Tx, claim db.IdempotencyClaim, receipt any) error {
	raw, err := json.Marshal(receipt)
	if err != nil {
		return err
	}
	claims, err := idempotency.TransactionFor(tx)
	if err != nil {
		return err
	}
	_, err = claims.Complete(ctx, claim, raw)
	return err
}

// Admit binds auto routing and all business rejections while holding the same
// Session row used by stop, activation and terminal settlement.
func Admit(ctx context.Context, tx pgx.Tx, request AdmissionRequest) (AdmissionReceipt, error) {
	q := db.New(tx)
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
	session, err := lockSession(ctx, tx, request.Target)
	if err != nil {
		return AdmissionReceipt{}, err
	}
	claim, err := claimOperation(ctx, tx, ControlRequest{Target: request.Target, IdempotencyKey: request.IdempotencyKey}, name, struct {
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
	if session.CancelRequestedAt.Valid {
		receipt.Code = "session_not_open"
	} else if session.NextEventSequence > 9007199254740991 {
		receipt.Code = "invalid_request"
	} else if request.Mode != ExactMessage && session.Status != "open" {
		receipt.Code = "session_not_open"
	} else if request.Mode != EnqueueOnly && session.DispatchHoldID.Valid {
		receipt.Code = "session_held"
	} else if request.Mode == ExactMessage && session.ActiveTurnID != pgvalue.UUID(request.TurnID) {
		receipt.Code = "turn_not_active"
	} else if request.Mode == ExactMessage || request.Mode == SendMessageOrEnqueue && session.ActiveTurnID.Valid {
		receipt.TurnID = pgvalue.MustUUIDValue(session.ActiveTurnID)
		accepts, err := q.SessionTurnAcceptsMessages(ctx, db.SessionTurnAcceptsMessagesParams{EnvironmentID: session.EnvironmentID, SessionID: session.ID, ID: session.ActiveTurnID})
		if err != nil {
			return receipt, err
		}
		turn, err := q.GetSessionTurn(ctx, db.GetSessionTurnParams{EnvironmentID: session.EnvironmentID, SessionID: session.ID, ID: session.ActiveTurnID})
		if err != nil {
			return receipt, err
		}
		if turn.SettlementStartedAt.Valid {
			receipt.Code = "turn_settling"
		} else if !accepts {
			receipt.Code = "turn_not_active"
		} else {
			messageID := uuid.NewV7()
			// The locked event allocator gives message delivery its public order.
			_, err = q.CreateSessionMessage(ctx, db.CreateSessionMessageParams{ID: pgvalue.UUID(messageID), EnvironmentID: session.EnvironmentID, SessionID: session.ID, TurnID: turn.ID, RunID: turn.RunID, AttemptNumber: turn.AttemptNumber.Int32, RunGeneration: turn.RunGeneration.Int64, Data: data, AcceptedSequence: session.NextEventSequence})
			if err != nil {
				return receipt, err
			}
			body, _ := json.Marshal(map[string]any{"message_id": messageID, "message": json.RawMessage(data)})
			if _, err := appendLifecycleEvent(ctx, q, session, turn.ID, pgvalue.UUID(messageID), "message.accepted", body, pgtype.UUID{}); err != nil {
				return receipt, err
			}
			receipt.Kind, receipt.MessageID = "messaged", &messageID
		}
	} else if session.NextInputSequence > 9007199254740991 || session.NextEventSequence > 9007199254740991 {
		receipt.Code = "invalid_request"
	} else {
		turnID := uuid.NewV7()
		var source pgtype.UUID
		if request.SourceRunID != uuid.Nil() {
			source = pgvalue.UUID(request.SourceRunID)
		}
		turn, err := q.EnqueueSessionTurn(ctx, db.EnqueueSessionTurnParams{EnvironmentID: session.EnvironmentID, SessionID: session.ID, ID: pgvalue.UUID(turnID), Data: data, SourceRunID: source})
		if err != nil {
			return receipt, err
		}
		body, _ := json.Marshal(map[string]any{"input": json.RawMessage(data)})
		if _, err := appendLifecycleEvent(ctx, q, session, turn.ID, pgtype.UUID{}, "turn.enqueued", body, pgtype.UUID{}); err != nil {
			return receipt, err
		}
		if err := q.CreateSessionInputReconcileOutbox(ctx, db.CreateSessionInputReconcileOutboxParams{ID: turn.ID, SessionID: session.ID, EnvironmentID: session.EnvironmentID, TurnID: turn.ID}); err != nil {
			return receipt, err
		}
		receipt.Kind, receipt.TurnID = "enqueued", turnID
	}
	return receipt, finishOperation(ctx, tx, claim, receipt)
}

func appendLifecycleEvent(ctx context.Context, q db.Querier, session db.Session, turnID, messageID pgtype.UUID, kind string, data json.RawMessage, version pgtype.UUID) (db.SessionEvent, error) {
	return q.AppendSessionEvent(ctx, db.AppendSessionEventParams{ID: pgvalue.UUID(uuid.NewV7()), EnvironmentID: session.EnvironmentID, SessionID: session.ID, TurnID: turnID, MessageID: messageID, Kind: kind, Data: data, ComputerDiskVersionID: version})
}

// sessionComputerRef addresses the Computer the Session runs on.
func sessionComputerRef(session db.Session) computer.SessionComputerRef {
	return computer.SessionComputerRef{
		EnvironmentID: pgvalue.MustUUIDValue(session.EnvironmentID),
		ComputerID:    pgvalue.MustUUIDValue(session.ComputerID),
		SessionID:     pgvalue.MustUUIDValue(session.ID),
	}
}
