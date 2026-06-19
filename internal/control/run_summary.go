package control

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
)

type runSummary struct {
	ID                   pgtype.UUID
	OrgID                pgtype.UUID
	ProjectID            pgtype.UUID
	EnvironmentID        pgtype.UUID
	DeploymentID         pgtype.UUID
	DeploymentTaskID     pgtype.UUID
	TaskSessionID        pgtype.UUID
	DeploymentVersion    string
	APIVersion           string
	SDKVersion           string
	CLIVersion           string
	TaskID               string
	Status               db.RunStatus
	ExecutionStatus      db.RunExecutionStatus
	TerminalOutcome      db.NullRunTerminalOutcome
	Metadata             []byte
	Tags                 []string
	LockedRetryPolicy    []byte
	CurrentAttemptNumber pgtype.Int4
	ExitCode             pgtype.Int4
	Output               []byte
	CreatedAt            pgtype.Timestamptz
	UpdatedAt            pgtype.Timestamptz
}

func createScopedRunSummary(run db.CreateScopedRunRow) runSummary {
	return runSummary{
		ID:                   run.ID,
		OrgID:                run.OrgID,
		ProjectID:            run.ProjectID,
		EnvironmentID:        run.EnvironmentID,
		DeploymentID:         run.DeploymentID,
		DeploymentTaskID:     run.DeploymentTaskID,
		TaskSessionID:        run.TaskSessionID,
		DeploymentVersion:    run.DeploymentVersion,
		APIVersion:           run.ApiVersion,
		SDKVersion:           run.SdkVersion,
		CLIVersion:           run.CliVersion,
		TaskID:               run.TaskID,
		Status:               run.Status,
		ExecutionStatus:      run.ExecutionStatus,
		TerminalOutcome:      run.TerminalOutcome,
		Metadata:             run.Metadata,
		Tags:                 run.Tags,
		LockedRetryPolicy:    run.LockedRetryPolicy,
		CurrentAttemptNumber: run.CurrentAttemptNumber,
		ExitCode:             run.ExitCode,
		Output:               run.Output,
		CreatedAt:            run.CreatedAt,
		UpdatedAt:            run.UpdatedAt,
	}
}

func getRunSummary(run db.GetRunSummaryRow) runSummary {
	return runSummary{
		ID:                   run.ID,
		OrgID:                run.OrgID,
		ProjectID:            run.ProjectID,
		EnvironmentID:        run.EnvironmentID,
		DeploymentID:         run.DeploymentID,
		DeploymentTaskID:     run.DeploymentTaskID,
		TaskSessionID:        run.TaskSessionID,
		DeploymentVersion:    run.DeploymentVersion,
		APIVersion:           run.ApiVersion,
		SDKVersion:           run.SdkVersion,
		CLIVersion:           run.CliVersion,
		TaskID:               run.TaskID,
		Status:               run.Status,
		ExecutionStatus:      run.ExecutionStatus,
		TerminalOutcome:      run.TerminalOutcome,
		Metadata:             run.Metadata,
		Tags:                 run.Tags,
		LockedRetryPolicy:    run.LockedRetryPolicy,
		CurrentAttemptNumber: run.CurrentAttemptNumber,
		ExitCode:             run.ExitCode,
		Output:               run.Output,
		CreatedAt:            run.CreatedAt,
		UpdatedAt:            run.UpdatedAt,
	}
}

func listScopedRunSummary(run db.ListScopedRunSummariesRow) runSummary {
	return runSummary{
		ID:                   run.ID,
		OrgID:                run.OrgID,
		ProjectID:            run.ProjectID,
		EnvironmentID:        run.EnvironmentID,
		DeploymentID:         run.DeploymentID,
		DeploymentTaskID:     run.DeploymentTaskID,
		TaskSessionID:        run.TaskSessionID,
		DeploymentVersion:    run.DeploymentVersion,
		APIVersion:           run.ApiVersion,
		SDKVersion:           run.SdkVersion,
		CLIVersion:           run.CliVersion,
		TaskID:               run.TaskID,
		Status:               run.Status,
		ExecutionStatus:      run.ExecutionStatus,
		TerminalOutcome:      run.TerminalOutcome,
		Metadata:             run.Metadata,
		Tags:                 run.Tags,
		LockedRetryPolicy:    run.LockedRetryPolicy,
		CurrentAttemptNumber: run.CurrentAttemptNumber,
		ExitCode:             run.ExitCode,
		Output:               run.Output,
		CreatedAt:            run.CreatedAt,
		UpdatedAt:            run.UpdatedAt,
	}
}

func cancelRunSummary(run db.CancelRunRow) runSummary {
	return runSummary{
		ID:                   run.ID,
		OrgID:                run.OrgID,
		ProjectID:            run.ProjectID,
		EnvironmentID:        run.EnvironmentID,
		DeploymentID:         run.DeploymentID,
		DeploymentTaskID:     run.DeploymentTaskID,
		TaskSessionID:        run.TaskSessionID,
		DeploymentVersion:    run.DeploymentVersion,
		APIVersion:           run.ApiVersion,
		SDKVersion:           run.SdkVersion,
		CLIVersion:           run.CliVersion,
		TaskID:               run.TaskID,
		Status:               run.Status,
		ExecutionStatus:      run.ExecutionStatus,
		TerminalOutcome:      run.TerminalOutcome,
		Metadata:             run.Metadata,
		Tags:                 run.Tags,
		LockedRetryPolicy:    run.LockedRetryPolicy,
		CurrentAttemptNumber: run.CurrentAttemptNumber,
		ExitCode:             run.ExitCode,
		Output:               run.Output,
		CreatedAt:            run.CreatedAt,
		UpdatedAt:            run.UpdatedAt,
	}
}

func scopedRunCountsResponse(counts db.CountScopedRunsByStatusRow) api.RunCountsResponse {
	return api.RunCountsResponse{
		Queued:    counts.Queued,
		Running:   counts.Running,
		Waiting:   counts.Waiting,
		Succeeded: counts.Succeeded,
		Failed:    counts.Failed,
		Cancelled: counts.Cancelled,
		Expired:   counts.Expired,
	}
}

func runResponse(run runSummary) api.RunResponse {
	runID := pgvalue.MustUUIDValue(run.ID)
	var exitCode *int32
	if run.ExitCode.Valid {
		exitCode = &run.ExitCode.Int32
	}
	var attemptNumber *int32
	if run.CurrentAttemptNumber.Valid {
		attemptNumber = &run.CurrentAttemptNumber.Int32
	}
	var output json.RawMessage
	if len(run.Output) > 0 {
		output = append(json.RawMessage(nil), run.Output...)
	}
	metadata := json.RawMessage(run.Metadata)
	if len(metadata) == 0 {
		metadata = json.RawMessage(`{}`)
	}
	return api.RunResponse{
		ID:                runID.String(),
		ProjectID:         pgvalue.MustUUIDValue(run.ProjectID).String(),
		EnvironmentID:     pgvalue.MustUUIDValue(run.EnvironmentID).String(),
		DeploymentID:      pgvalue.MustUUIDValue(run.DeploymentID).String(),
		DeploymentTaskID:  pgvalue.MustUUIDValue(run.DeploymentTaskID).String(),
		TaskSessionID:     pgvalue.MustUUIDValue(run.TaskSessionID).String(),
		Version:           run.DeploymentVersion,
		DeploymentVersion: run.DeploymentVersion,
		APIVersion:        run.APIVersion,
		SDKVersion:        run.SDKVersion,
		CLIVersion:        run.CLIVersion,
		TaskID:            run.TaskID,
		Status:            publicRunStatus(run.Status),
		Metadata:          metadata,
		AttemptNumber:     attemptNumber,
		ExitCode:          exitCode,
		Output:            output,
		CreatedAt:         pgvalue.Time(run.CreatedAt),
		UpdatedAt:         pgvalue.Time(run.UpdatedAt),
	}
}

func (s *Server) runResponses(ctx context.Context, orgID pgtype.UUID, runs []runSummary) ([]api.RunResponse, error) {
	responses := make([]api.RunResponse, 0, len(runs))
	waitingRunIDs := make([]pgtype.UUID, 0, len(runs))
	responseIndexesByRunID := make(map[pgtype.UUID][]int, len(runs))
	for _, run := range runs {
		responseIndexesByRunID[run.ID] = append(responseIndexesByRunID[run.ID], len(responses))
		responses = append(responses, runResponse(run))
		if run.Status == db.RunStatusWaiting {
			waitingRunIDs = append(waitingRunIDs, run.ID)
		}
	}
	if len(waitingRunIDs) == 0 {
		return responses, nil
	}
	waitpoints, err := s.db.ListPendingWaitpointsForRuns(ctx, db.ListPendingWaitpointsForRunsParams{
		OrgID:  orgID,
		RunIds: waitingRunIDs,
	})
	if err != nil {
		return nil, fmt.Errorf("list pending waitpoints for runs: %w", err)
	}
	for _, waitpoint := range waitpoints {
		indexes := responseIndexesByRunID[waitpoint.RunID]
		if len(indexes) == 0 {
			continue
		}
		pending := pendingWaitpointResponse(pendingWaitpointViewFromList(waitpoint))
		for _, index := range indexes {
			pendingCopy := pending
			responses[index].PendingWaitpoint = &pendingCopy
		}
	}
	return responses, nil
}

func (s *Server) runResponse(ctx context.Context, run runSummary) (api.RunResponse, error) {
	response := runResponse(run)
	if run.Status != db.RunStatusWaiting {
		return response, nil
	}
	waitpoint, err := s.db.GetPendingWaitpointForRun(ctx, db.GetPendingWaitpointForRunParams{
		OrgID: run.OrgID,
		RunID: run.ID,
	})
	if isNoRows(err) {
		return response, nil
	}
	if err != nil {
		return api.RunResponse{}, err
	}
	pending := pendingWaitpointResponse(pendingWaitpointView(waitpoint))
	response.PendingWaitpoint = &pending
	return response, nil
}

func pendingWaitpointResponse(waitpoint waitpointView) api.PendingWaitpoint {
	response := api.PendingWaitpoint{
		ID:        pgvalue.MustUUIDValue(waitpoint.ID).String(),
		Kind:      string(waitpoint.Kind),
		Params:    waitpoint.Params,
		Metadata:  waitpoint.Metadata,
		Tags:      waitpoint.Tags,
		Status:    waitpoint.WaitpointStatus,
		CreatedAt: pgvalue.Time(waitpoint.CreatedAt),
	}
	if waitpoint.TimeoutSeconds.Valid {
		response.Timeout = &waitpoint.TimeoutSeconds.Int32
	}
	return response
}

func pendingWaitpointView(waitpoint db.GetPendingWaitpointForRunRow) waitpointView {
	return waitpointView{
		ID:              waitpoint.ID,
		RunSuspensionID: waitpoint.RunSuspensionID,
		OrgID:           waitpoint.OrgID,
		RunID:           waitpoint.RunID,
		RunLeaseID:      waitpoint.RunLeaseID,
		CheckpointID:    waitpoint.CheckpointID,
		CorrelationID:   waitpoint.CorrelationID,
		Kind:            waitpoint.Kind,
		WaitpointStatus: string(waitpoint.WaitpointStatus),
		Params:          waitpoint.Params,
		Metadata:        waitpoint.Metadata,
		Tags:            waitpoint.Tags,
		TimeoutSeconds:  waitpoint.TimeoutSeconds,
		Status:          waitpoint.Status,
		ResolutionKind:  waitpoint.ResolutionKind,
		Resolution:      waitpoint.Resolution,
		CreatedAt:       waitpoint.CreatedAt,
		WaitingAt:       waitpoint.WaitingAt,
		ResolvedAt:      waitpoint.ResolvedAt,
	}
}

func pendingWaitpointViewFromList(waitpoint db.ListPendingWaitpointsForRunsRow) waitpointView {
	return waitpointView{
		ID:              waitpoint.ID,
		RunSuspensionID: waitpoint.RunSuspensionID,
		OrgID:           waitpoint.OrgID,
		RunID:           waitpoint.RunID,
		RunLeaseID:      waitpoint.RunLeaseID,
		CheckpointID:    waitpoint.CheckpointID,
		CorrelationID:   waitpoint.CorrelationID,
		Kind:            waitpoint.Kind,
		WaitpointStatus: string(waitpoint.WaitpointStatus),
		Params:          waitpoint.Params,
		Metadata:        waitpoint.Metadata,
		Tags:            waitpoint.Tags,
		TimeoutSeconds:  waitpoint.TimeoutSeconds,
		Status:          waitpoint.Status,
		ResolutionKind:  waitpoint.ResolutionKind,
		Resolution:      waitpoint.Resolution,
		CreatedAt:       waitpoint.CreatedAt,
		WaitingAt:       waitpoint.WaitingAt,
		ResolvedAt:      waitpoint.ResolvedAt,
	}
}
