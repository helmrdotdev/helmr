package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
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
		(actor.State == "open" || actor.State == "closing") &&
		!actor.ManualRunCancelled
}

func CompleteWait(ctx context.Context, store db.Querier, wait db.RunWait, record db.SessionRecord) (db.RunWait, error) {
	result, err := RecordResolution(record)
	if err != nil {
		return db.RunWait{}, err
	}
	completed, err := run.Complete(ctx, store, wait, result, record.ID)
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

func RecordResolution(record db.SessionRecord) (json.RawMessage, error) {
	var value any
	if err := json.Unmarshal(record.Data, &value); err != nil {
		return nil, fmt.Errorf("actor input record data is invalid: %w", err)
	}
	source := map[string]any{"type": record.SourceKind.String}
	if record.SourceKind.String == "run" {
		source["run_id"] = pgvalue.UUIDString(record.SourceRunID)
	}
	return json.Marshal(map[string]any{
		"value": value,
		"record": map[string]any{
			"id": pgvalue.UUIDString(record.ID), "sequence": record.Sequence,
			"created_at": record.CreatedAt.Time.UTC().Format(time.RFC3339Nano), "source": source,
		},
	})
}

func CreateContinuation(
	ctx context.Context,
	store db.Querier,
	actor db.Session,
	workspace db.Workspace,
	bindings []db.LockWorkspaceSecretsForAdmissionRow,
) (pgtype.UUID, error) {
	runID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
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
			binding.SecretState != "active" || !binding.CurrentVersionID.Valid {
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
	if _, err := store.CreateRunAdmissionOutbox(ctx, db.CreateRunAdmissionOutboxParams{
		ID: pgvalue.UUID(uuid.Must(uuid.NewV7())), WorkspaceID: workspace.ID,
		EnvironmentID: actor.EnvironmentID, RunID: run.ID,
	}); err != nil {
		return pgtype.UUID{}, err
	}
	return run.ID, nil
}
