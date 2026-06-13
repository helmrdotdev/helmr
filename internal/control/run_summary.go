package control

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

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
	ReplayedFromRunID    pgtype.UUID
	CurrentAttemptNumber pgtype.Int4
	ExitCode             pgtype.Int4
	Output               []byte
	CreatedAt            pgtype.Timestamptz
	UpdatedAt            pgtype.Timestamptz
}

func idempotentRunSummary(run db.GetScopedRunByIdempotencyKeyRow) runSummary {
	return runSummary{
		ID:                   run.ID,
		OrgID:                run.OrgID,
		ProjectID:            run.ProjectID,
		EnvironmentID:        run.EnvironmentID,
		DeploymentID:         run.DeploymentID,
		DeploymentTaskID:     run.DeploymentTaskID,
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
		ReplayedFromRunID:    run.ReplayedFromRunID,
		CurrentAttemptNumber: run.CurrentAttemptNumber,
		ExitCode:             run.ExitCode,
		Output:               run.Output,
		CreatedAt:            run.CreatedAt,
		UpdatedAt:            run.UpdatedAt,
	}
}

func createScopedRunSummary(run db.CreateScopedRunRow) runSummary {
	return runSummary{
		ID:                   run.ID,
		OrgID:                run.OrgID,
		ProjectID:            run.ProjectID,
		EnvironmentID:        run.EnvironmentID,
		DeploymentID:         run.DeploymentID,
		DeploymentTaskID:     run.DeploymentTaskID,
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
		ReplayedFromRunID:    run.ReplayedFromRunID,
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
		ReplayedFromRunID:    run.ReplayedFromRunID,
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
		ReplayedFromRunID:    run.ReplayedFromRunID,
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
		ReplayedFromRunID:    run.ReplayedFromRunID,
		CurrentAttemptNumber: run.CurrentAttemptNumber,
		ExitCode:             run.ExitCode,
		Output:               run.Output,
		CreatedAt:            run.CreatedAt,
		UpdatedAt:            run.UpdatedAt,
	}
}

func runRecordSummary(run db.Run) runSummary {
	return runSummary{
		ID:                   run.ID,
		OrgID:                run.OrgID,
		ProjectID:            run.ProjectID,
		EnvironmentID:        run.EnvironmentID,
		DeploymentID:         run.DeploymentID,
		DeploymentTaskID:     run.DeploymentTaskID,
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
		ReplayedFromRunID:    run.ReplayedFromRunID,
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
	return api.RunResponse{
		ID:                runID.String(),
		ProjectID:         pgvalue.MustUUIDValue(run.ProjectID).String(),
		EnvironmentID:     pgvalue.MustUUIDValue(run.EnvironmentID).String(),
		DeploymentID:      pgvalue.MustUUIDValue(run.DeploymentID).String(),
		DeploymentTaskID:  pgvalue.MustUUIDValue(run.DeploymentTaskID).String(),
		Version:           run.DeploymentVersion,
		DeploymentVersion: run.DeploymentVersion,
		APIVersion:        run.APIVersion,
		SDKVersion:        run.SDKVersion,
		CLIVersion:        run.CLIVersion,
		TaskID:            run.TaskID,
		Status:            publicRunStatus(run.Status),
		AttemptNumber:     attemptNumber,
		ExitCode:          exitCode,
		Output:            output,
		CreatedAt:         pgvalue.Time(run.CreatedAt),
		UpdatedAt:         pgvalue.Time(run.UpdatedAt),
	}
}

type waitpointDeliveryKey struct {
	runID       pgtype.UUID
	runWaitID   pgtype.UUID
	waitpointID pgtype.UUID
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
	deliveryKeys := make(map[waitpointDeliveryKey]struct{}, len(waitpoints))
	runWaitIDs := make([]pgtype.UUID, 0, len(waitpoints))
	for _, waitpoint := range waitpoints {
		indexes := responseIndexesByRunID[waitpoint.RunID]
		if len(indexes) == 0 {
			continue
		}
		pending, err := pendingWaitpointResponse(pendingWaitpointViewFromList(waitpoint))
		if err != nil {
			return nil, fmt.Errorf("build pending waitpoint response for run %s: %w", pgvalue.MustUUIDValue(waitpoint.RunID).String(), err)
		}
		for _, index := range indexes {
			pendingCopy := pending
			responses[index].PendingWaitpoint = &pendingCopy
		}
		deliveryKeys[waitpointDeliveryKey{
			runID:       waitpoint.RunID,
			runWaitID:   waitpoint.RunWaitID,
			waitpointID: waitpoint.ID,
		}] = struct{}{}
		runWaitIDs = append(runWaitIDs, waitpoint.RunWaitID)
	}
	if len(deliveryKeys) == 0 {
		return responses, nil
	}
	deliveries, err := s.db.ListWaitpointDeliveriesForRunWaits(ctx, db.ListWaitpointDeliveriesForRunWaitsParams{
		OrgID:      orgID,
		RunWaitIds: runWaitIDs,
	})
	if err != nil {
		return nil, fmt.Errorf("list waitpoint deliveries for run waits: %w", err)
	}
	for _, delivery := range deliveries {
		key := waitpointDeliveryKey{
			runID:       delivery.RunID,
			runWaitID:   delivery.RunWaitID,
			waitpointID: delivery.WaitpointID,
		}
		if _, ok := deliveryKeys[key]; !ok {
			continue
		}
		indexes := responseIndexesByRunID[delivery.RunID]
		if len(indexes) == 0 {
			continue
		}
		for _, index := range indexes {
			if responses[index].PendingWaitpoint == nil {
				continue
			}
			responses[index].PendingWaitpoint.Deliveries = append(responses[index].PendingWaitpoint.Deliveries, waitpointDeliveryResponse(delivery))
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
	pending, err := pendingWaitpointResponse(pendingWaitpointView(waitpoint))
	if err != nil {
		return api.RunResponse{}, err
	}
	deliveries, err := s.db.ListWaitpointDeliveries(ctx, db.ListWaitpointDeliveriesParams{
		OrgID:       waitpoint.OrgID,
		RunID:       waitpoint.RunID,
		RunWaitID:   waitpoint.RunWaitID,
		WaitpointID: waitpoint.ID,
	})
	if err != nil {
		return api.RunResponse{}, err
	}
	pending.Deliveries = make([]api.WaitpointDeliveryResponse, 0, len(deliveries))
	for _, delivery := range deliveries {
		pending.Deliveries = append(pending.Deliveries, waitpointDeliveryResponse(delivery))
	}
	response.PendingWaitpoint = &pending
	return response, nil
}

func pendingWaitpointResponse(waitpoint waitpointView) (api.PendingWaitpoint, error) {
	response := api.PendingWaitpoint{
		Kind:        string(waitpoint.Kind),
		WaitpointID: pgvalue.MustUUIDValue(waitpoint.ID).String(),
		Request:     waitpoint.Request,
		DisplayText: waitpoint.DisplayText,
		RequestedAt: pgvalue.Time(waitpoint.RequestedAt),
	}
	if waitpoint.TimeoutSeconds.Valid {
		response.Timeout = &waitpoint.TimeoutSeconds.Int32
	}
	if waitpoint.PolicyName.Valid {
		policy := waitpoint.PolicyName.String
		response.Policy = &policy
	}
	switch waitpoint.Kind {
	case db.WaitpointKindHuman, db.WaitpointKindDelay:
	default:
		return api.PendingWaitpoint{}, fmt.Errorf("unsupported waitpoint kind %q", waitpoint.Kind)
	}
	return response, nil
}

func pendingWaitpointView(waitpoint db.GetPendingWaitpointForRunRow) waitpointView {
	return waitpointView{
		ID:             waitpoint.ID,
		RunWaitID:      waitpoint.RunWaitID,
		OrgID:          waitpoint.OrgID,
		RunID:          waitpoint.RunID,
		SessionID:      waitpoint.SessionID,
		CheckpointID:   waitpoint.CheckpointID,
		CorrelationID:  waitpoint.CorrelationID,
		Kind:           waitpoint.Kind,
		Request:        waitpoint.Request,
		DisplayText:    waitpoint.DisplayText,
		TimeoutSeconds: waitpoint.TimeoutSeconds,
		PolicyName:     waitpoint.PolicyName,
		PolicySnapshot: waitpoint.PolicySnapshot,
		Status:         waitpoint.Status,
		ResolutionKind: waitpoint.ResolutionKind,
		Resolution:     waitpoint.Resolution,
		CreatedAt:      waitpoint.CreatedAt,
		RequestedAt:    waitpoint.RequestedAt,
		ResolvedAt:     waitpoint.ResolvedAt,
	}
}

func pendingWaitpointViewFromList(waitpoint db.ListPendingWaitpointsForRunsRow) waitpointView {
	return waitpointView{
		ID:             waitpoint.ID,
		RunWaitID:      waitpoint.RunWaitID,
		OrgID:          waitpoint.OrgID,
		RunID:          waitpoint.RunID,
		SessionID:      waitpoint.SessionID,
		CheckpointID:   waitpoint.CheckpointID,
		CorrelationID:  waitpoint.CorrelationID,
		Kind:           waitpoint.Kind,
		Request:        waitpoint.Request,
		DisplayText:    waitpoint.DisplayText,
		TimeoutSeconds: waitpoint.TimeoutSeconds,
		PolicyName:     waitpoint.PolicyName,
		PolicySnapshot: waitpoint.PolicySnapshot,
		Status:         waitpoint.Status,
		ResolutionKind: waitpoint.ResolutionKind,
		Resolution:     waitpoint.Resolution,
		CreatedAt:      waitpoint.CreatedAt,
		RequestedAt:    waitpoint.RequestedAt,
		ResolvedAt:     waitpoint.ResolvedAt,
	}
}

func waitpointDeliveryResponse(delivery db.WaitpointDelivery) api.WaitpointDeliveryResponse {
	var lastError *string
	if delivery.LastError.Valid {
		lastError = &delivery.LastError.String
	}
	var sentAt *time.Time
	if delivery.SentAt.Valid {
		value := pgvalue.Time(delivery.SentAt)
		sentAt = &value
	}
	return api.WaitpointDeliveryResponse{
		ID:            pgvalue.MustUUIDValue(delivery.ID).String(),
		Channel:       delivery.Channel,
		RecipientKind: delivery.RecipientKind,
		Recipient:     delivery.Recipient,
		Status:        string(delivery.Status),
		LastError:     lastError,
		SentAt:        sentAt,
		CreatedAt:     pgvalue.Time(delivery.CreatedAt),
		UpdatedAt:     pgvalue.Time(delivery.UpdatedAt),
	}
}
