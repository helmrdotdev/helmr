package run

import (
	"bytes"
	"context"
	"errors"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

var (
	// ErrTurnScope reports that the addressed Turn is not produced by the
	// addressed execution: a missing Session or Turn, or a different Run,
	// Attempt or Run generation.
	ErrTurnScope = errors.New("turn producer scope is stale")
	// ErrTurnNotActive reports that the Turn is not the Session's running
	// active Turn, or that the Session is not open or closing.
	ErrTurnNotActive = errors.New("turn is not active")
	// ErrTurnStopped reports that a stop was accepted for the Session or Turn.
	ErrTurnStopped = errors.New("turn interruption has been accepted")
	// ErrTurnUnsettled reports that the Turn began settlement and admits no
	// new work.
	ErrTurnUnsettled = errors.New("turn settlement has started")
	// ErrWaitCursor reports that a wait's input cursor does not match the
	// locked execution.
	ErrWaitCursor = errors.New("wait input cursor is stale")
)

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

// LockedTurn is a Session and one of its Turns locked by LockTurn in the
// owning transaction. It is valid only inside that transaction. Accessors
// return copies.
type LockedTurn struct {
	session db.Session
	turn    db.SessionTurn
	scope   TurnScope
}

// LockTurn locks the scope's Session and then its Turn. A missing Session or
// Turn is ErrTurnScope. It checks no producer; Validate and ValidateWork do.
func LockTurn(ctx context.Context, tx pgx.Tx, scope TurnScope) (LockedTurn, error) {
	if scope.EnvironmentID == uuid.Nil() || scope.SessionID == uuid.Nil() || scope.TurnID == uuid.Nil() {
		return LockedTurn{}, ErrTurnScope
	}
	q := db.New(tx)
	session, err := q.LockSessionTurnAuthority(ctx, db.LockSessionTurnAuthorityParams{EnvironmentID: pgvalue.UUID(scope.EnvironmentID), ID: pgvalue.UUID(scope.SessionID)})
	if err != nil {
		return LockedTurn{}, turnError(err)
	}
	turn, err := q.LockSessionTurnInput(ctx, db.LockSessionTurnInputParams{EnvironmentID: session.EnvironmentID, SessionID: session.ID, ID: pgvalue.UUID(scope.TurnID)})
	if err != nil {
		return LockedTurn{}, turnError(err)
	}
	return LockedTurn{session: session, turn: turn, scope: scope}, nil
}

// Session is the locked Session.
func (t LockedTurn) Session() db.Session { return cloneSession(t.session) }

// Turn is the locked Turn.
func (t LockedTurn) Turn() db.SessionTurn { return cloneTurn(t.turn) }

// Validate requires the locked Turn to be the running active Turn of the
// scope's execution and not stopping. It returns the Turn, also on failure:
// ErrTurnScope, ErrTurnNotActive or ErrTurnStopped.
func (t LockedTurn) Validate() (db.SessionTurn, error) {
	s, input, scope := t.session, t.turn, t.scope
	if scope.RunGeneration <= 0 || scope.RunID == uuid.Nil() || scope.AttemptNumber <= 0 || input.RunGeneration.Int64 != scope.RunGeneration || input.RunID != pgvalue.UUID(scope.RunID) || input.AttemptNumber.Int32 != scope.AttemptNumber || s.CurrentRunID != input.RunID || s.RunGeneration != scope.RunGeneration {
		return t.Turn(), ErrTurnScope
	}
	if s.ActiveTurnID != input.ID || input.Status != "running" || (s.Status != "open" && s.Status != "closing") {
		return t.Turn(), ErrTurnNotActive
	}
	if s.DispatchHoldID.Valid || input.InterruptRequestedAt.Valid {
		return t.Turn(), ErrTurnStopped
	}
	return t.Turn(), nil
}

// ValidateWork is Validate for new Turn work: a Turn that began settlement is
// ErrTurnUnsettled.
func (t LockedTurn) ValidateWork() (db.SessionTurn, error) {
	turn, err := t.Validate()
	if err == nil && turn.SettlementStartedAt.Valid {
		err = ErrTurnUnsettled
	}
	return turn, err
}

// ValidateTurn locks the scope's Turn and validates it.
func ValidateTurn(ctx context.Context, tx pgx.Tx, scope TurnScope) (db.SessionTurn, error) {
	locked, err := LockTurn(ctx, tx, scope)
	if err != nil {
		return db.SessionTurn{}, err
	}
	return locked.Validate()
}

// ValidateTurnWork locks the scope's Turn and validates it for new work.
func ValidateTurnWork(ctx context.Context, tx pgx.Tx, scope TurnScope) (db.SessionTurn, error) {
	locked, err := LockTurn(ctx, tx, scope)
	if err != nil {
		return db.SessionTurn{}, err
	}
	return locked.ValidateWork()
}

// ValidateTurnWork validates, for new work, the execution's Turn turnID at
// runGeneration.
func (e Execution) ValidateTurnWork(ctx context.Context, tx pgx.Tx, turnID uuid.UUID, runGeneration int64) error {
	_, err := ValidateTurnWork(ctx, tx, TurnScope{EnvironmentID: pgvalue.MustUUIDValue(e.run.EnvironmentID), SessionID: pgvalue.MustUUIDValue(e.session.ID), RunID: pgvalue.MustUUIDValue(e.run.ID), TurnID: turnID, AttemptNumber: e.attempt.Number, RunGeneration: runGeneration})
	return err
}

// ValidateWaitCursor checks a wait's input cursor against the locked
// execution. The cursor belongs to the request and its fingerprint. A wait
// references its admitted Turn; only a Computer checkpoint stores a suspended
// execution cursor. It returns ErrWaitCursor, ErrTurnStopped or ErrTurnScope.
func (e Execution) ValidateWaitCursor(wait db.RunWait, cursor pgtype.Int8) error {
	r, attempt, s := e.run, e.attempt, e.session
	if r.EntrypointKind == "task" {
		if r.SessionID.Valid || cursor.Valid || wait.TurnID.Valid {
			return ErrWaitCursor
		}
		return nil
	}
	if r.EntrypointKind != "actor" || !r.SessionID.Valid || r.SessionID != s.ID || s.CurrentRunID != r.ID || (s.Status != "open" && s.Status != "closing") {
		return ErrWaitCursor
	}
	if s.DispatchHoldID.Valid || s.CancelRequestedAt.Valid {
		return ErrTurnStopped
	}
	if wait.TurnID.Valid && (s.ActiveTurnID != wait.TurnID || wait.TurnSessionID != s.ID || !wait.TurnRunGeneration.Valid || wait.TurnRunGeneration.Int64 != s.RunGeneration) {
		return ErrTurnScope
	}
	if wait.Kind == db.WaitKindSessionInput && wait.CompletedTurnID.Valid {
		if wait.CompletedTurnID != s.ActiveTurnID || wait.TurnID != wait.CompletedTurnID {
			return ErrTurnScope
		}
	} else if s.ActiveTurnID.Valid != wait.TurnID.Valid {
		return ErrTurnScope
	}
	want := s.CommittedInputSequence
	if wait.TurnID.Valid && wait.Kind != db.WaitKindSessionInput {
		want++
	}
	if !cursor.Valid || cursor.Int64 != want || !attempt.SessionInputStartSequence.Valid || attempt.SessionInputStartSequence.Int64 > s.CommittedInputSequence || cursor.Int64 >= s.NextInputSequence {
		return ErrWaitCursor
	}
	return nil
}

func cloneTurn(t db.SessionTurn) db.SessionTurn {
	t.Data = bytes.Clone(t.Data)
	return t
}

func turnError(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrTurnScope
	}
	return err
}
