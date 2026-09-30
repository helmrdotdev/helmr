package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/idempotency"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

var errChildTaskInvokeStale = errors.New("child task invocation authority is stale")

type childCallRegistration struct {
	RunWaitID, ResumeAttachID uuid.UUID
	TurnID                    pgtype.UUID
	RunGeneration             pgtype.Int8
	TaskDeclaredID            string
}

func registerChildCall(
	ctx context.Context,
	store db.Querier,
	input childCallRegistration,
	authority run.Execution,
	claim db.IdempotencyClaim,
	fingerprint idempotency.TaskChildInvokeFingerprint,
	childRunID uuid.UUID,
	childComputerID uuid.UUID,
) (workerapi.CreateRunWaitResponse, error) {
	existing, waitErr := store.GetChildCallAttemptWait(ctx, db.GetChildCallAttemptWaitParams{EnvironmentID: authority.Run().EnvironmentID, RunID: authority.Run().ID, AttemptNumber: authority.Attempt().Number, ChildClaimID: claim.ID})
	if waitErr != nil && !errors.Is(waitErr, pgx.ErrNoRows) {
		return workerapi.CreateRunWaitResponse{}, waitErr
	}
	if waitErr == nil && (existing.ID != pgvalue.UUID(input.RunWaitID)) {
		return workerapi.CreateRunWaitResponse{}, errChildTaskInvokeStale
	}
	waitID := input.RunWaitID
	resumeAttachID := input.ResumeAttachID
	requestFingerprint, err := terminalRequestFingerprint("worker.child-call.wait", struct {
		Claim      string
		WaitID     string
		AttachID   string
		TurnID     string
		Generation int64
	}{fmt.Sprintf("%x", claim.RequestFingerprint), input.RunWaitID.String(), input.ResumeAttachID.String(), pgvalue.UUIDString(input.TurnID), input.RunGeneration.Int64})
	if err != nil {
		return workerapi.CreateRunWaitResponse{}, err
	}
	if waitErr == nil && existing.RegistrationRequestFingerprint.String != requestFingerprint {
		return workerapi.CreateRunWaitResponse{}, errChildTaskInvokeStale
	}
	childRequest, err := idempotency.EncodeTaskChildInvokeFingerprint(fingerprint)
	if err != nil {
		return workerapi.CreateRunWaitResponse{}, fmt.Errorf("encode child task call request: %w", err)
	}
	response := workerapi.CreateRunWaitResponse{
		RunID: pgvalue.UUIDString(authority.Run().ID), RunWaitID: waitID.String(),
		ResumeAttachID:     resumeAttachID.String(),
		ComputerInstanceID: pgvalue.UUIDString(authority.Instance().ID),
		RuntimeEpoch:       authority.Instance().WorkerEpoch,
	}
	replayed, err := store.GetChildCallRunWaitReplay(ctx, db.GetChildCallRunWaitReplayParams{
		EnvironmentID: authority.Run().EnvironmentID, RunID: authority.Run().ID,
		AttemptNumber: authority.Attempt().Number, ID: pgvalue.UUID(waitID),
		ChildRunID: pgvalue.UUID(childRunID), ChildClaimID: claim.ID,
		RegistrationRequestFingerprint: pgvalue.Text(requestFingerprint),
	})
	if err == nil {
		if err := validateChildWaitScope(childWaitScopeOf(authority), replayed); err != nil {
			return workerapi.CreateRunWaitResponse{}, err
		}
		if replayed.SuspensionStatus == db.RunWaitStatusReleased {
			if replayed.ConditionStatus != db.WaitStatusCompleted || replayed.ConditionResult == nil {
				return workerapi.CreateRunWaitResponse{}, errChildTaskInvokeStale
			}
			response.ResolutionKind = "completed"
			response.Resolution = append(json.RawMessage(nil), replayed.ConditionResult...)
		}
		return response, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return workerapi.CreateRunWaitResponse{}, fmt.Errorf("load child task call replay: %w", err)
	}
	childRun, err := store.GetRun(ctx, db.GetRunParams{
		EnvironmentID: authority.Run().EnvironmentID, ID: pgvalue.UUID(childRunID),
	})
	if err != nil || childRun.ParentRunID != authority.Run().ID ||
		!childRun.ParentOwnsLifecycle.Valid || !childRun.ParentOwnsLifecycle.Bool ||
		childRun.ComputerID != pgvalue.UUID(childComputerID) ||
		childRun.ClaimID != claim.ID {
		return workerapi.CreateRunWaitResponse{}, staleChildTaskInvoke(err)
	}
	params := db.RegisterChildCallParams{
		RunID: authority.Run().ID, EnvironmentID: authority.Run().EnvironmentID,
		ExpectedRunningRevision: authority.Run().Revision,
		AttemptNumber:           authority.Attempt().Number,
		CurrentRunLeaseID:       authority.Lease().ID,
		ChildComputerID:         pgvalue.UUID(childComputerID), ID: pgvalue.UUID(waitID),
		ChildRunID: pgvalue.UUID(childRunID), ChildTargetDeclaredID: pgvalue.Text(input.TaskDeclaredID),
		ChildClaimID: claim.ID, ChildRequest: childRequest,
		RegistrationRequestFingerprint: pgvalue.Text(requestFingerprint),
	}
	if childRun.Status == db.RunStatusSucceeded || childRun.Status == db.RunStatusFailed ||
		childRun.Status == db.RunStatusCancelled || childRun.Status == db.RunStatusExpired ||
		childRun.Status == db.RunStatusSystemFailed {
		resolution, err := childTaskResult(childRun)
		if err != nil {
			return workerapi.CreateRunWaitResponse{}, err
		}
		_, err = store.RegisterResolvedChildCall(
			ctx,
			db.RegisterResolvedChildCallParams{
				ID: params.ID, EnvironmentID: params.EnvironmentID, RunID: params.RunID,
				ChildRunID: params.ChildRunID, ChildTargetDeclaredID: params.ChildTargetDeclaredID,
				ChildClaimID: params.ChildClaimID, ChildRequest: params.ChildRequest,
				ConditionResult:                resolution,
				RegistrationRequestFingerprint: params.RegistrationRequestFingerprint,
				ExpectedRunningRevision:        params.ExpectedRunningRevision,
				AttemptNumber:                  params.AttemptNumber,
				CurrentRunLeaseID:              params.CurrentRunLeaseID,
			},
		)
		if err != nil {
			return workerapi.CreateRunWaitResponse{}, staleChildTaskInvoke(err)
		}
		if err := bindOrCheckChildWaitTurn(ctx, store, authority, input); err != nil {
			return workerapi.CreateRunWaitResponse{}, err
		}
		response.ResolutionKind = "completed"
		response.Resolution = resolution
		return response, nil
	}
	if childRun.Status != db.RunStatusQueued && childRun.Status != db.RunStatusRunning &&
		childRun.Status != db.RunStatusWaiting && childRun.Status != db.RunStatusRetryDelayed &&
		childRun.Status != db.RunStatusCancelRequested {
		return workerapi.CreateRunWaitResponse{}, errChildTaskInvokeStale
	}
	_, err = store.RegisterChildCall(ctx, params)
	if err != nil {
		return workerapi.CreateRunWaitResponse{}, staleChildTaskInvoke(err)
	}
	if err := bindOrCheckChildWaitTurn(ctx, store, authority, input); err != nil {
		return workerapi.CreateRunWaitResponse{}, err
	}
	return response, nil
}

func childTaskResult(run db.Run) (json.RawMessage, error) {
	runID := pgvalue.UUIDString(run.ID)
	if err := ids.Validate(runID); err != nil {
		return nil, err
	}
	if run.Status == db.RunStatusSucceeded {
		if run.Output == nil || !json.Valid(run.Output) {
			return nil, errors.New("succeeded child task has invalid output")
		}
		return json.Marshal(struct {
			OK     bool            `json:"ok"`
			Output json.RawMessage `json:"output"`
			Run    struct {
				ID string `json:"id"`
			} `json:"run"`
		}{OK: true, Output: run.Output, Run: struct {
			ID string `json:"id"`
		}{ID: runID}})
	}
	if len(run.Failure) == 0 {
		return nil, errors.New("failed child task has no failure")
	}
	failure, err := projectRunFailure(run.Failure)
	if err != nil {
		return nil, err
	}
	return json.Marshal(struct {
		OK      bool                   `json:"ok"`
		Failure api.RunFailureResponse `json:"failure"`
		Run     struct {
			ID string `json:"id"`
		} `json:"run"`
	}{OK: false, Failure: failure, Run: struct {
		ID string `json:"id"`
	}{ID: runID}})
}

func staleChildTaskInvoke(err error) error {
	if err == nil || errors.Is(err, pgx.ErrNoRows) {
		return errChildTaskInvokeStale
	}
	return err
}

func bindOrCheckChildWaitTurn(ctx context.Context, q db.Querier, a run.Execution, input childCallRegistration) error {
	wait, err := q.GetRunWait(ctx, db.GetRunWaitParams{AttemptNumber: a.Attempt().Number, RunID: a.Run().ID, ID: pgvalue.UUID(input.RunWaitID)})
	if err != nil {
		return err
	}
	if wait.TurnID.Valid {
		return validateChildWaitScope(childWaitScopeOf(a), wait)
	}
	wait.TurnID = input.TurnID
	wait.TurnRunGeneration = input.RunGeneration
	if input.TurnID.Valid {
		wait.TurnSessionID = a.Session().ID
	}
	if err := validateChildWaitScope(childWaitScopeOf(a), wait); err != nil {
		return err
	}
	return bindWorkerWaitTurn(ctx, q, a, wait.ID, input.TurnID, input.RunGeneration)
}
