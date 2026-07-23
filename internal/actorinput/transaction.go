package actorinput

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/publicid"
	"github.com/helmrdotdev/helmr/internal/tracing"
	"github.com/jackc/pgx/v5/pgtype"
)

var ErrAuthority = errors.New("Actor input durable authority is inconsistent")

// CanStartContinuation is the common precondition for an idle Actor to consume
// durable input. Closing Actors must continue until their close sequence is
// committed; the SQL CAS remains the final authority for backlog and expiry.
func CanStartContinuation(actor db.Actor) bool {
	return !actor.CurrentRunID.Valid &&
		(actor.State == "open" || actor.State == "closing") &&
		!actor.ManualRunCancelled
}

func CompleteWait(ctx context.Context, store db.Querier, wait db.RunWait, record db.ActorRecord) (db.RunWait, error) {
	result, err := RecordResolution(record)
	if err != nil {
		return db.RunWait{}, err
	}
	switch wait.SuspensionState {
	case db.RunWaitStateHot:
		return store.CompleteHotActorInputRunWait(ctx, db.CompleteHotActorInputRunWaitParams{
			ConditionResult: result, CompletedActorRecordID: record.ID, ID: wait.ID, RunID: wait.RunID,
			ExpectedRunStateVersion: wait.ExpectedRunStateVersion, CurrentRunLeaseID: wait.CurrentRunLeaseID,
			AttemptNumber: wait.AttemptNumber,
		})
	case db.RunWaitStateCheckpointing:
		return store.CompleteCheckpointingActorInputRunWait(ctx, db.CompleteCheckpointingActorInputRunWaitParams{
			ConditionResult: result, CompletedActorRecordID: record.ID, ID: wait.ID, RunID: wait.RunID,
			ExpectedRunStateVersion: wait.ExpectedRunStateVersion, CurrentRunLeaseID: wait.CurrentRunLeaseID,
		})
	case db.RunWaitStateParked:
		completed, err := store.CompleteParkedActorInputRunWait(ctx, db.CompleteParkedActorInputRunWaitParams{
			ConditionResult: result, CompletedActorRecordID: record.ID, ID: wait.ID, RunID: wait.RunID,
			ExpectedRunStateVersion: wait.ExpectedRunStateVersion, PriorRunLeaseID: wait.PriorRunLeaseID,
			SuspendCheckpointID: wait.SuspendCheckpointID, AttemptNumber: wait.AttemptNumber,
		})
		if err != nil {
			return db.RunWait{}, err
		}
		now, err := store.GetRunLeaseRenewalTime(ctx)
		if err != nil || !now.Valid {
			return db.RunWait{}, fmt.Errorf("load Actor input resume time: %w", err)
		}
		payload, _ := json.Marshal(map[string]any{
			"environmentId": pgvalue.UUIDString(wait.EnvironmentID), "runId": pgvalue.UUIDString(wait.RunID),
			"runWaitId": pgvalue.UUIDString(wait.ID), "resumeRequestVersion": completed.ResumeRequestVersion,
		})
		_, err = store.CreateOutboxMessage(ctx, db.CreateOutboxMessageParams{
			ID: pgvalue.UUID(uuid.Must(uuid.NewV7())), Lane: "control", Topic: "run.resume",
			PartitionKey: pgvalue.UUIDString(wait.WorkspaceID), Payload: payload, AvailableAt: now,
		})
		return completed, err
	default:
		return db.RunWait{}, ErrAuthority
	}
}

func FailWait(ctx context.Context, store db.Querier, wait db.RunWait, reason string) (db.RunWait, error) {
	errorJSON, err := json.Marshal(map[string]any{"code": reason, "retryable": false})
	if err != nil {
		return db.RunWait{}, err
	}
	reasonCode := pgvalue.Text(reason)
	var failed db.RunWait
	switch wait.SuspensionState {
	case db.RunWaitStateHot:
		failed, err = store.FailHotRunWait(ctx, db.FailHotRunWaitParams{
			ReasonCode: reasonCode, ConditionError: errorJSON, ID: wait.ID, RunID: wait.RunID,
			ExpectedRunStateVersion: wait.ExpectedRunStateVersion, CurrentRunLeaseID: wait.CurrentRunLeaseID,
			AttemptNumber: wait.AttemptNumber,
		})
	case db.RunWaitStateCheckpointing:
		failed, err = store.FailCheckpointingRunWait(ctx, db.FailCheckpointingRunWaitParams{
			ReasonCode: reasonCode, ConditionError: errorJSON, ID: wait.ID, RunID: wait.RunID,
			ExpectedRunStateVersion: wait.ExpectedRunStateVersion, CurrentRunLeaseID: wait.CurrentRunLeaseID,
		})
	case db.RunWaitStateParked:
		failed, err = store.FailParkedRunWait(ctx, db.FailParkedRunWaitParams{
			ReasonCode: reasonCode, ConditionError: errorJSON, ID: wait.ID, RunID: wait.RunID,
			ExpectedRunStateVersion: wait.ExpectedRunStateVersion, PriorRunLeaseID: wait.PriorRunLeaseID,
			SuspendCheckpointID: wait.SuspendCheckpointID, AttemptNumber: wait.AttemptNumber,
		})
	default:
		return db.RunWait{}, ErrAuthority
	}
	if err != nil || failed.SuspensionState != db.RunWaitStateResumePending {
		return failed, err
	}
	now, err := store.GetRunLeaseRenewalTime(ctx)
	if err != nil || !now.Valid {
		return db.RunWait{}, fmt.Errorf("load Actor input timeout resume time: %w", err)
	}
	payload, _ := json.Marshal(map[string]any{
		"environmentId": pgvalue.UUIDString(wait.EnvironmentID), "runId": pgvalue.UUIDString(wait.RunID),
		"runWaitId": pgvalue.UUIDString(wait.ID), "resumeRequestVersion": failed.ResumeRequestVersion,
	})
	_, err = store.CreateOutboxMessage(ctx, db.CreateOutboxMessageParams{
		ID: pgvalue.UUID(uuid.Must(uuid.NewV7())), Lane: "control", Topic: "run.resume",
		PartitionKey: pgvalue.UUIDString(wait.WorkspaceID), Payload: payload, AvailableAt: now,
	})
	return failed, err
}

func RecordResolution(record db.ActorRecord) (json.RawMessage, error) {
	var value any
	if err := json.Unmarshal(record.Data, &value); err != nil {
		return nil, fmt.Errorf("Actor input record data is invalid: %w", err)
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
	actor db.Actor,
	workspace db.Workspace,
	bindings []db.LockWorkspaceSecretsForAdmissionRow,
) (pgtype.UUID, error) {
	runID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	publicIDValue, err := publicid.New(publicid.Run)
	if err != nil {
		return pgtype.UUID{}, err
	}
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
		return pgtype.UUID{}, fmt.Errorf("load Actor input continuation time: %w", err)
	}
	run, err := store.CreateActorContinuationRun(ctx, db.CreateActorContinuationRunParams{
		RunID: runID, PublicID: publicIDValue, QueueOriginAt: now,
		TraceID: pgvalue.Text(traceID), RootSpanID: rootSpanID,
		EnvironmentID: actor.EnvironmentID, ActorID: actor.ID, WorkspaceID: workspace.ID,
		ExpectedRunGeneration: actor.RunGeneration,
	})
	if err != nil {
		return pgtype.UUID{}, err
	}
	for _, binding := range bindings {
		if binding.WorkspaceID != workspace.ID || binding.EnvironmentID != actor.EnvironmentID ||
			binding.SecretState != "active" || !binding.CurrentVersionID.Valid {
			return pgtype.UUID{}, ErrAuthority
		}
		if _, err := store.CreateSecretResolution(ctx, db.CreateSecretResolutionParams{
			ID: pgvalue.UUID(uuid.Must(uuid.NewV7())), WorkspaceID: workspace.ID, RunID: run.ID,
			AttemptNumber: pgtype.Int4{Int32: 1, Valid: true}, PlacementKind: binding.PlacementKind,
			PlacementTarget: binding.PlacementTarget, SecretID: binding.SecretID,
			SecretVersionID: binding.CurrentVersionID, RevocationGeneration: binding.RevocationGeneration,
		}); err != nil {
			return pgtype.UUID{}, err
		}
	}
	if _, err := store.CreateRunAdmissionOutbox(ctx, db.CreateRunAdmissionOutboxParams{
		ID: pgvalue.UUID(uuid.Must(uuid.NewV7())), WorkspaceID: workspace.ID,
		EnvironmentID: actor.EnvironmentID, RunID: run.ID,
	}); err != nil {
		return pgtype.UUID{}, err
	}
	return run.ID, nil
}
