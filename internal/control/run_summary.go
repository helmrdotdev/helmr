package control

import (
	"encoding/json"

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

func runResponses(runs []runSummary) []api.RunResponse {
	responses := make([]api.RunResponse, 0, len(runs))
	for _, run := range runs {
		responses = append(responses, runResponse(run))
	}
	return responses
}
