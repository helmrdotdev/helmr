package runwait

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
)

var ErrAuthority = errors.New("durable Wait authority is inconsistent")

func Complete(
	ctx context.Context,
	store db.Querier,
	wait db.RunWait,
	result json.RawMessage,
	completedActorRecordID pgtype.UUID,
) (db.RunWait, error) {
	var completed db.RunWait
	var err error
	switch wait.SuspensionState {
	case db.RunWaitStateHot:
		completed, err = store.CompleteHotRunWait(ctx, db.CompleteHotRunWaitParams{
			ConditionResult: result, CompletedActorRecordID: completedActorRecordID,
			ID: wait.ID, RunID: wait.RunID,
			ExpectedRunStateVersion: wait.ExpectedRunStateVersion,
			CurrentRunLeaseID:       wait.CurrentRunLeaseID,
			AttemptNumber:           wait.AttemptNumber,
		})
	case db.RunWaitStateCheckpointing:
		completed, err = store.CompleteCheckpointingRunWait(ctx, db.CompleteCheckpointingRunWaitParams{
			ConditionResult: result, CompletedActorRecordID: completedActorRecordID,
			ID: wait.ID, RunID: wait.RunID,
			ExpectedRunStateVersion: wait.ExpectedRunStateVersion,
			CurrentRunLeaseID:       wait.CurrentRunLeaseID,
		})
	case db.RunWaitStateParked:
		completed, err = store.CompleteParkedRunWait(ctx, db.CompleteParkedRunWaitParams{
			ConditionResult: result, CompletedActorRecordID: completedActorRecordID,
			ID: wait.ID, RunID: wait.RunID,
			ExpectedRunStateVersion: wait.ExpectedRunStateVersion,
			PriorRunLeaseID:         wait.PriorRunLeaseID,
			SuspendCheckpointID:     wait.SuspendCheckpointID,
			AttemptNumber:           wait.AttemptNumber,
		})
	default:
		return db.RunWait{}, ErrAuthority
	}
	if err != nil {
		return db.RunWait{}, err
	}
	return publishResumeIfParked(ctx, store, wait, completed)
}

func Fail(
	ctx context.Context,
	store db.Querier,
	wait db.RunWait,
	reason string,
) (db.RunWait, error) {
	errorJSON, err := json.Marshal(map[string]any{"code": reason, "retryable": false})
	if err != nil {
		return db.RunWait{}, err
	}
	reasonCode := pgvalue.Text(reason)
	var failed db.RunWait
	switch wait.SuspensionState {
	case db.RunWaitStateHot:
		failed, err = store.FailHotRunWait(ctx, db.FailHotRunWaitParams{
			ReasonCode: reasonCode, ConditionError: errorJSON,
			ID: wait.ID, RunID: wait.RunID,
			ExpectedRunStateVersion: wait.ExpectedRunStateVersion,
			CurrentRunLeaseID:       wait.CurrentRunLeaseID,
			AttemptNumber:           wait.AttemptNumber,
		})
	case db.RunWaitStateCheckpointing:
		failed, err = store.FailCheckpointingRunWait(ctx, db.FailCheckpointingRunWaitParams{
			ReasonCode: reasonCode, ConditionError: errorJSON,
			ID: wait.ID, RunID: wait.RunID,
			ExpectedRunStateVersion: wait.ExpectedRunStateVersion,
			CurrentRunLeaseID:       wait.CurrentRunLeaseID,
		})
	case db.RunWaitStateParked:
		failed, err = store.FailParkedRunWait(ctx, db.FailParkedRunWaitParams{
			ReasonCode: reasonCode, ConditionError: errorJSON,
			ID: wait.ID, RunID: wait.RunID,
			ExpectedRunStateVersion: wait.ExpectedRunStateVersion,
			PriorRunLeaseID:         wait.PriorRunLeaseID,
			SuspendCheckpointID:     wait.SuspendCheckpointID,
			AttemptNumber:           wait.AttemptNumber,
		})
	default:
		return db.RunWait{}, ErrAuthority
	}
	if err != nil {
		return db.RunWait{}, err
	}
	return publishResumeIfParked(ctx, store, wait, failed)
}

func publishResumeIfParked(
	ctx context.Context,
	store db.Querier,
	wait db.RunWait,
	terminal db.RunWait,
) (db.RunWait, error) {
	if terminal.SuspensionState != db.RunWaitStateResumePending {
		return terminal, nil
	}
	now, err := store.GetRunLeaseRenewalTime(ctx)
	if err != nil || !now.Valid {
		return db.RunWait{}, fmt.Errorf("load durable Wait resume time: %w", err)
	}
	payload, err := json.Marshal(map[string]any{
		"environmentId":        pgvalue.UUIDString(wait.EnvironmentID),
		"runId":                pgvalue.UUIDString(wait.RunID),
		"runWaitId":            pgvalue.UUIDString(wait.ID),
		"resumeRequestVersion": terminal.ResumeRequestVersion,
	})
	if err != nil {
		return db.RunWait{}, err
	}
	_, err = store.CreateOutboxMessage(ctx, db.CreateOutboxMessageParams{
		ID:   pgvalue.UUID(uuid.Must(uuid.NewV7())),
		Lane: "control", Topic: "run.resume",
		PartitionKey: pgvalue.UUIDString(wait.WorkspaceID),
		Payload:      payload, AvailableAt: now,
	})
	return terminal, err
}
