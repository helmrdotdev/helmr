package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/helmrdotdev/helmr/internal/tracing"
	"github.com/jackc/pgx/v5/pgtype"
)

var ErrAuthority = errors.New("actor input durable authority is inconsistent")

// CanStartContinuation is the common precondition for an idle Actor to consume
// durable input. Closing Actors must continue until their close sequence is
// committed; the SQL CAS remains the final authority for backlog and expiry.
func CanStartContinuation(actor db.Session) bool {
	return !actor.CurrentRunID.Valid &&
		(actor.Status == "open" || actor.Status == "closing") &&
		!actor.DispatchHoldID.Valid && !actor.ActiveTurnID.Valid && actor.CommittedInputSequence < actor.NextInputSequence-1
}

func CompleteWait(ctx context.Context, store db.Querier, wait db.RunWait, turn db.SessionTurn) (db.RunWait, error) {
	var err error
	turn, err = ActivateTurn(ctx, store, TurnScope{EnvironmentID: pgvalue.MustUUIDValue(turn.EnvironmentID), SessionID: pgvalue.MustUUIDValue(turn.SessionID), TurnID: pgvalue.MustUUIDValue(turn.ID), RunID: pgvalue.MustUUIDValue(wait.RunID), AttemptNumber: wait.AttemptNumber})
	if err != nil {
		return db.RunWait{}, err
	}
	// The receive wait starts outside a Turn; once it admits the queued input,
	// bind its delivery/restore acknowledgment to that exact execution as well.
	if _, err = store.BindRunWaitTurn(ctx, db.BindRunWaitTurnParams{SessionID: turn.SessionID, TurnID: turn.ID, RunGeneration: turn.RunGeneration, WaitID: wait.ID}); err != nil {
		return db.RunWait{}, err
	}
	result, err := TurnResolution(turn)
	if err != nil {
		return db.RunWait{}, err
	}
	completed, err := run.Complete(ctx, store, wait, result, turn.ID)
	if errors.Is(err, run.ErrWaitAuthority) {
		return db.RunWait{}, ErrAuthority
	}
	return completed, err
}

func FailWait(ctx context.Context, store db.Querier, wait db.RunWait, reason string) (db.RunWait, error) {
	failed, err := run.Fail(ctx, store, wait, reason)
	if errors.Is(err, run.ErrWaitAuthority) {
		return db.RunWait{}, ErrAuthority
	}
	return failed, err
}

func TurnResolution(turn db.SessionTurn) (json.RawMessage, error) {
	var value any
	if err := json.Unmarshal(turn.Data, &value); err != nil {
		return nil, fmt.Errorf("turn input data is invalid: %w", err)
	}
	source := map[string]any{"type": "external"}
	if turn.SourceRunID.Valid {
		source["type"] = "run"
		source["run_id"] = pgvalue.UUIDString(turn.SourceRunID)
	}
	return json.Marshal(map[string]any{
		"value":          value,
		"run_generation": turn.RunGeneration.Int64,
		"turn": map[string]any{
			"id": pgvalue.UUIDString(turn.ID), "sequence": turn.Sequence,
			"created_at": turn.CreatedAt.Time.UTC().Format(time.RFC3339Nano), "source": source,
		},
	})
}

func CreateContinuation(
	ctx context.Context,
	store db.Querier,
	actor db.Session,
	workspace db.LockActorInputWorkspaceRow,
	bindings []db.LockWorkspaceSecretsForAdmissionRow,
) (pgtype.UUID, error) {
	if actor.CancelRequestedAt.Valid {
		return pgtype.UUID{}, ErrAuthority
	}
	runID := pgvalue.UUID(uuid.NewV7())
	traceID, err := tracing.NewTraceID()
	if err != nil {
		return pgtype.UUID{}, err
	}
	rootSpanID, err := tracing.NewSpanID()
	if err != nil {
		return pgtype.UUID{}, err
	}
	now, err := store.GetRunLeaseRenewalTime(ctx)
	if err != nil || !now.Valid {
		return pgtype.UUID{}, fmt.Errorf("load actor input continuation time: %w", err)
	}
	run, err := store.CreateActorContinuationRun(ctx, db.CreateActorContinuationRunParams{
		RunID: runID, QueueOriginAt: now,
		TraceID: pgvalue.Text(traceID), RootSpanID: rootSpanID,
		EnvironmentID: actor.EnvironmentID, SessionID: actor.ID, WorkspaceID: workspace.ID,
		ExpectedRunGeneration: actor.RunGeneration,
	})
	if err != nil {
		return pgtype.UUID{}, err
	}
	resolutions := make([]secret.Resolution, len(bindings))
	for index, binding := range bindings {
		if binding.WorkspaceID != workspace.ID || binding.EnvironmentID != actor.EnvironmentID ||
			binding.SecretStatus != "active" || !binding.CurrentVersionID.Valid {
			return pgtype.UUID{}, ErrAuthority
		}
		resolutions[index] = secret.Resolution{
			PlacementKind: binding.PlacementKind, PlacementTarget: binding.PlacementTarget,
			SecretID: binding.SecretID, SecretVersionID: binding.CurrentVersionID,
			RevocationGeneration: binding.RevocationGeneration,
		}
	}
	if err := secret.CreateAttemptResolutions(ctx, store, workspace.ID, run.ID, 1, resolutions); err != nil {
		return pgtype.UUID{}, err
	}
	return run.ID, nil
}
