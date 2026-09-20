package run

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
)

var ErrWaitAuthority = errors.New("durable wait authority is inconsistent")

func Complete(
	ctx context.Context,
	store db.Querier,
	wait db.RunWait,
	result json.RawMessage,
	completedTurnID pgtype.UUID,
) (db.RunWait, error) {
	{
		current, err := store.RunWaitTurnCurrent(ctx, wait.ID)
		if err != nil {
			return db.RunWait{}, err
		}
		if !current {
			return db.RunWait{}, ErrWaitAuthority
		}
	}

	var completed db.RunWait
	var err error
	switch wait.SuspensionStatus {
	case db.RunWaitStatusHot:
		completed, err = store.CompleteHotRunWait(ctx, db.CompleteHotRunWaitParams{
			ConditionResult: result, CompletedTurnID: completedTurnID,
			ID: wait.ID, RunID: wait.RunID,
			ExpectedRunRevision: wait.ExpectedRunRevision,
			CurrentRunLeaseID:   wait.CurrentRunLeaseID,
			AttemptNumber:       wait.AttemptNumber,
		})
	case db.RunWaitStatusCheckpointing:
		completed, err = store.CompleteCheckpointingRunWait(ctx, db.CompleteCheckpointingRunWaitParams{
			ConditionResult: result, CompletedTurnID: completedTurnID,
			ID: wait.ID, RunID: wait.RunID,
			ExpectedRunRevision: wait.ExpectedRunRevision,
			CurrentRunLeaseID:   wait.CurrentRunLeaseID,
		})
	case db.RunWaitStatusParked:
		completed, err = store.CompleteParkedRunWait(ctx, db.CompleteParkedRunWaitParams{
			ConditionResult: result, CompletedTurnID: completedTurnID,
			ID: wait.ID, RunID: wait.RunID,
			ExpectedRunRevision: wait.ExpectedRunRevision,
			PriorRunLeaseID:     wait.PriorRunLeaseID,
			SuspendCheckpointID: wait.SuspendCheckpointID,
			AttemptNumber:       wait.AttemptNumber,
		})
	default:
		return db.RunWait{}, ErrWaitAuthority
	}
	if err != nil {
		return db.RunWait{}, err
	}
	return completed, nil
}

func Fail(
	ctx context.Context,
	store db.Querier,
	wait db.RunWait,
	reason string,
) (db.RunWait, error) {
	{
		current, err := store.RunWaitTurnCurrent(ctx, wait.ID)
		if err != nil {
			return db.RunWait{}, err
		}
		if !current {
			return db.RunWait{}, ErrWaitAuthority
		}
	}

	errorJSON, err := json.Marshal(map[string]any{"code": reason, "retryable": false})
	if err != nil {
		return db.RunWait{}, err
	}
	reasonCode := pgvalue.Text(reason)
	var failed db.RunWait
	switch wait.SuspensionStatus {
	case db.RunWaitStatusHot:
		failed, err = store.FailHotRunWait(ctx, db.FailHotRunWaitParams{
			ReasonCode: reasonCode, ConditionError: errorJSON,
			ID: wait.ID, RunID: wait.RunID,
			ExpectedRunRevision: wait.ExpectedRunRevision,
			CurrentRunLeaseID:   wait.CurrentRunLeaseID,
			AttemptNumber:       wait.AttemptNumber,
		})
	case db.RunWaitStatusCheckpointing:
		failed, err = store.FailCheckpointingRunWait(ctx, db.FailCheckpointingRunWaitParams{
			ReasonCode: reasonCode, ConditionError: errorJSON,
			ID: wait.ID, RunID: wait.RunID,
			ExpectedRunRevision: wait.ExpectedRunRevision,
			CurrentRunLeaseID:   wait.CurrentRunLeaseID,
		})
	case db.RunWaitStatusParked:
		failed, err = store.FailParkedRunWait(ctx, db.FailParkedRunWaitParams{
			ReasonCode: reasonCode, ConditionError: errorJSON,
			ID: wait.ID, RunID: wait.RunID,
			ExpectedRunRevision: wait.ExpectedRunRevision,
			PriorRunLeaseID:     wait.PriorRunLeaseID,
			SuspendCheckpointID: wait.SuspendCheckpointID,
			AttemptNumber:       wait.AttemptNumber,
		})
	default:
		return db.RunWait{}, ErrWaitAuthority
	}
	if err != nil {
		return db.RunWait{}, err
	}
	return failed, nil
}
