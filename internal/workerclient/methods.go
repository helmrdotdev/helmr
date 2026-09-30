package workerclient

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/httpclient"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func (c *Client) EnrollWorker(ctx context.Context, token string, request workerapi.EnrollmentRequest) (workerapi.EnrollmentResponse, error) {
	request.APIVersion = workerapi.APIVersion
	var response workerapi.EnrollmentResponse
	if err := c.postJSON(ctx, "/worker/v1/enrollment", token, request, &response); err != nil {
		return workerapi.EnrollmentResponse{}, err
	}
	return response, nil
}

func (c *Client) DiscoverRunLeases(ctx context.Context) (workerapi.RunLeaseDiscoveryResponse, error) {
	var response workerapi.RunLeaseDiscoveryResponse
	if err := c.postWorkerJSON(
		ctx,
		"/worker/v1/run/leases/discover",
		workerapi.RunLeaseDiscoveryRequest{},
		&response,
	); err != nil {
		return workerapi.RunLeaseDiscoveryResponse{}, err
	}
	return response, nil
}

func (c *Client) ClaimRunLease(
	ctx context.Context,
	work workerapi.RunLeaseWork,
) (workerapi.RunLeaseClaimResponse, error) {
	var response workerapi.RunLeaseClaimResponse
	if err := c.postWorkerJSON(
		ctx,
		"/worker/v1/run/leases/claim",
		workerapi.RunLeaseClaimRequest(work),
		&response,
	); err != nil {
		return workerapi.RunLeaseClaimResponse{}, err
	}
	return response, nil
}

func (c *Client) AcknowledgeRunStart(
	ctx context.Context,
	request workerapi.RunStartRequest,
) (workerapi.RunStartResponse, error) {
	var response workerapi.RunStartResponse
	if err := c.postWorkerJSON(
		ctx,
		"/worker/v1/run/leases/start",
		request,
		&response,
	); err != nil {
		return workerapi.RunStartResponse{}, err
	}
	return response, nil
}

func (c *Client) AcknowledgeRunEntrypoint(
	ctx context.Context,
	request workerapi.RunEntrypointRequest,
) error {
	return c.postWorkerJSON(
		ctx,
		"/worker/v1/run/leases/entrypoint",
		request,
		nil,
	)
}

func (c *Client) ClaimComputerInstance(ctx context.Context) (workerapi.ComputerInstanceClaimResponse, error) {
	var response workerapi.ComputerInstanceClaimResponse
	if err := c.postWorkerJSON(ctx, "/worker/v1/run/computer-instances/claim", struct{}{}, &response); err != nil {
		return workerapi.ComputerInstanceClaimResponse{}, err
	}
	return response, nil
}

func (c *Client) RenewComputerInstance(ctx context.Context, request workerapi.ComputerInstanceRenewRequest) (workerapi.ComputerInstanceRenewResponse, error) {
	var response workerapi.ComputerInstanceRenewResponse
	if err := c.postWorkerJSON(ctx, "/worker/v1/run/computer-instances/renew", request, &response); err != nil {
		return workerapi.ComputerInstanceRenewResponse{}, err
	}
	return response, nil
}

func (c *Client) ClaimComputerCommand(ctx context.Context, request workerapi.ComputerCommandClaimRequest) (workerapi.ComputerCommandClaimResponse, error) {
	var response workerapi.ComputerCommandClaimResponse
	if err := c.postWorkerJSON(ctx, "/worker/v1/run/computer-commands/claim", request, &response); err != nil {
		return workerapi.ComputerCommandClaimResponse{}, err
	}
	return response, nil
}

func (c *Client) CompleteComputerCommand(ctx context.Context, request workerapi.ComputerCommandCompleteRequest) error {
	return c.postWorkerJSON(ctx, "/worker/v1/run/computer-commands/complete", request, nil)
}

func (c *Client) AppendCommandLog(ctx context.Context, request workerapi.CommandLogAppendRequest) error {
	return c.postWorkerJSON(ctx, "/worker/v1/run/computer-commands/logs/append", request, nil)
}

func (c *Client) WriteTurnOutput(ctx context.Context, request workerapi.WriteTurnOutputRequest) (workerapi.WriteOutputResponse, error) {
	var response workerapi.WriteOutputResponse
	if err := c.postWorkerJSON(ctx, "/worker/v1/run/turns/output/write", request, &response); err != nil {
		return workerapi.WriteOutputResponse{}, err
	}
	return response, nil
}

func (c *Client) ActivateWorker(ctx context.Context, capabilities workerapi.Capabilities) (workerapi.StatusResponse, error) {
	var response workerapi.StatusResponse
	if err := c.postWorkerJSON(ctx, "/worker/v1/instance/activate", workerapi.ActivateRequest{APIVersion: workerapi.APIVersion, Capabilities: capabilities}, &response); err != nil {
		return workerapi.StatusResponse{}, err
	}
	return response, nil
}

func (c *Client) ReportWorkerStartupRecovery(ctx context.Context, request workerapi.StartupRecoveryRequest) error {
	return c.postWorkerJSON(ctx, "/worker/v1/instance/recover", request, nil)
}

func (c *Client) ObserveWorker(ctx context.Context, observation workerapi.Observation) (workerapi.StatusResponse, error) {
	var response workerapi.StatusResponse
	if err := c.postWorkerJSON(ctx, "/worker/v1/instance/observations", workerapi.ObserveRequest{Observation: observation}, &response); err != nil {
		return workerapi.StatusResponse{}, err
	}
	return response, nil
}

func (c *Client) DrainWorker(ctx context.Context) (workerapi.StatusResponse, error) {
	var response workerapi.StatusResponse
	if err := c.postWorkerJSON(ctx, "/worker/v1/instance/drain", struct{}{}, &response); err != nil {
		return workerapi.StatusResponse{}, err
	}
	return response, nil
}

func (c *Client) CompleteWorkerDrain(ctx context.Context, request workerapi.DrainCompletionRequest) (workerapi.StatusResponse, error) {
	const attempts = 3
	var lastErr error
	for attempt := range attempts {
		var response workerapi.StatusResponse
		lastErr = c.postWorkerJSON(ctx, "/worker/v1/instance/drain/complete", request, &response)
		if lastErr == nil {
			return response, nil
		}
		if !ambiguousWorkerTerminalMutation(lastErr) || attempt == attempts-1 {
			break
		}
		delay := time.Duration(attempt+1) * 100 * time.Millisecond
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return workerapi.StatusResponse{}, ctx.Err()
		case <-timer.C:
		}
	}
	return workerapi.StatusResponse{}, fmt.Errorf("worker drain completion was not confirmed after %d identical attempts: %w", attempts, lastErr)
}

func ambiguousWorkerTerminalMutation(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var httpErr *httpclient.Error
	if !errors.As(err, &httpErr) {
		return true
	}
	switch httpErr.StatusCode {
	case http.StatusRequestTimeout, http.StatusTooManyRequests, http.StatusInternalServerError,
		http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

func (c *Client) FenceWorker(ctx context.Context, reasonCode string) error {
	const attempts = 3
	var lastErr error
	request := workerapi.FenceRequest{ReasonCode: reasonCode}
	for attempt := range attempts {
		lastErr = c.postWorkerJSON(ctx, "/worker/v1/instance/fence", request, nil)
		if lastErr == nil {
			return nil
		}
		if !ambiguousWorkerTerminalMutation(lastErr) || attempt == attempts-1 {
			break
		}
		delay := time.Duration(attempt+1) * 100 * time.Millisecond
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return fmt.Errorf("worker fence was not confirmed after %d identical attempts: %w", attempts, lastErr)
}

func (c *Client) GetWorkerStatus(ctx context.Context) (workerapi.StatusResponse, error) {
	var response workerapi.StatusResponse
	if err := c.getWorkerJSON(ctx, "/worker/v1/instance", &response); err != nil {
		return workerapi.StatusResponse{}, err
	}
	return response, nil
}

func (c *Client) ListRuntimeReconcileTargets(ctx context.Context) (workerapi.RuntimeReconcileResponse, error) {
	var response workerapi.RuntimeReconcileResponse
	if err := c.postWorkerJSON(ctx, "/worker/v1/run/computer-instances/reconcile", workerapi.RuntimeReconcileRequest{}, &response); err != nil {
		return workerapi.RuntimeReconcileResponse{}, err
	}
	return response, nil
}

func (c *Client) MarkComputerInstanceReady(ctx context.Context, request workerapi.ComputerInstanceStateRequest) (workerapi.ComputerInstance, error) {
	var response workerapi.ComputerInstance
	if err := c.postWorkerJSON(ctx, "/worker/v1/run/computer-instances/ready", request, &response); err != nil {
		return workerapi.ComputerInstance{}, err
	}
	return response, nil
}

func (c *Client) MarkComputerInstanceClosed(ctx context.Context, request workerapi.ComputerInstanceStateRequest) (workerapi.ComputerInstance, error) {
	var response workerapi.ComputerInstance
	if err := c.postWorkerJSON(ctx, "/worker/v1/run/computer-instances/closed", request, &response); err != nil {
		return workerapi.ComputerInstance{}, err
	}
	return response, nil
}

func (c *Client) MarkComputerInstanceFailed(ctx context.Context, request workerapi.ComputerInstanceStateRequest) (workerapi.ComputerInstance, error) {
	var response workerapi.ComputerInstance
	if err := c.postWorkerJSON(ctx, "/worker/v1/run/computer-instances/failed", request, &response); err != nil {
		return workerapi.ComputerInstance{}, err
	}
	return response, nil
}

func (c *Client) RenewRunLease(
	ctx context.Context,
	lease workerapi.RunLeaseAssignment,
) (workerapi.RunLeaseRenewResponse, error) {
	var response workerapi.RunLeaseRenewResponse
	if err := c.postWorkerJSON(
		ctx,
		"/worker/v1/run/leases/renew",
		workerapi.RunLeaseRenewRequest{
			Lease:             lease.Fence(),
			ExpectedExpiresAt: lease.ExpiresAt,
		},
		&response,
	); err != nil {
		return workerapi.RunLeaseRenewResponse{}, err
	}
	return response, nil
}

func (c *Client) BeginRunFinalization(
	ctx context.Context,
	request workerapi.BeginRunFinalizationRequest,
) (workerapi.BeginRunFinalizationResponse, error) {
	var response workerapi.BeginRunFinalizationResponse
	if err := c.postWorkerJSON(
		ctx,
		"/worker/v1/run/finalization/begin",
		request,
		&response,
	); err != nil {
		return workerapi.BeginRunFinalizationResponse{}, err
	}
	return response, nil
}

func (c *Client) CommitActorTurn(
	ctx context.Context,
	request workerapi.CommitActorTurnRequest,
) (workerapi.CommitActorTurnResponse, error) {
	var response workerapi.CommitActorTurnResponse
	if err := c.postWorkerJSON(ctx, "/worker/v1/run/sessions/turns/commit", request, &response); err != nil {
		return workerapi.CommitActorTurnResponse{}, err
	}
	return response, nil
}

func (c *Client) SendRunSession(
	ctx context.Context,
	request workerapi.SubmitSessionDataRequest,
) (workerapi.SubmitSessionDataResponse, error) {
	var response workerapi.SubmitSessionDataResponse
	if err := c.postWorkerJSON(ctx, "/worker/v1/run/sessions/send", request, &response); err != nil {
		return workerapi.SubmitSessionDataResponse{}, err
	}
	return response, nil
}

func (c *Client) StartRunActor(
	ctx context.Context,
	request workerapi.StartActorRequest,
) (workerapi.StartActorResponse, error) {
	var response workerapi.StartActorResponse
	if err := c.postWorkerJSON(ctx, "/worker/v1/run/actors/start", request, &response); err != nil {
		return workerapi.StartActorResponse{}, err
	}
	return response, nil
}

func (c *Client) GetRunSessionStatus(
	ctx context.Context,
	request workerapi.SessionReferenceRequest,
) (workerapi.SessionStatusResponse, error) {
	var response workerapi.SessionStatusResponse
	if err := c.postWorkerJSON(ctx, "/worker/v1/run/sessions/retrieve", request, &response); err != nil {
		return workerapi.SessionStatusResponse{}, err
	}
	return response, nil
}

func (c *Client) CloseRunSession(
	ctx context.Context,
	request workerapi.CloseSessionRequest,
) (workerapi.CloseSessionResponse, error) {
	var response workerapi.CloseSessionResponse
	if err := c.postWorkerJSON(ctx, "/worker/v1/run/sessions/close", request, &response); err != nil {
		return workerapi.CloseSessionResponse{}, err
	}
	return response, nil
}

func (c *Client) CancelRunSession(
	ctx context.Context,
	request workerapi.CancelSessionRequest,
) (workerapi.CancelSessionResponse, error) {
	var response workerapi.CancelSessionResponse
	if err := c.postWorkerJSON(ctx, "/worker/v1/run/sessions/cancel", request, &response); err != nil {
		return workerapi.CancelSessionResponse{}, err
	}
	return response, nil
}

func (c *Client) ReadRunSessionEvents(
	ctx context.Context,
	request workerapi.ReadSessionEventsRequest,
) (workerapi.ReadSessionEventsResponse, error) {
	var response workerapi.ReadSessionEventsResponse
	if err := c.postWorkerJSON(ctx, "/worker/v1/run/sessions/events/read-page", request, &response); err != nil {
		return workerapi.ReadSessionEventsResponse{}, err
	}
	return response, nil
}

func (c *Client) CreateRunComputer(
	ctx context.Context,
	request workerapi.CreateComputerRequest,
) (workerapi.CreateComputerResponse, error) {
	var response workerapi.CreateComputerResponse
	if err := c.postWorkerJSON(ctx, "/worker/v1/run/computers/create", request, &response); err != nil {
		return workerapi.CreateComputerResponse{}, err
	}
	return response, nil
}

func (c *Client) RetrieveRunComputer(
	ctx context.Context,
	request workerapi.RetrieveComputerRequest,
) (workerapi.RetrieveComputerResponse, error) {
	var response workerapi.RetrieveComputerResponse
	if err := c.postWorkerJSON(ctx, "/worker/v1/run/computers/retrieve", request, &response); err != nil {
		return workerapi.RetrieveComputerResponse{}, err
	}
	return response, nil
}

func (c *Client) ListRunComputerMembers(
	ctx context.Context,
	request workerapi.ComputerMembersRequest,
) (workerapi.ComputerMembersResponse, error) {
	var response workerapi.ComputerMembersResponse
	if err := c.postWorkerJSON(ctx, "/worker/v1/run/computers/members", request, &response); err != nil {
		return workerapi.ComputerMembersResponse{}, err
	}
	return response, nil
}

func (c *Client) DeleteRunComputer(
	ctx context.Context,
	request workerapi.DeleteComputerRequest,
) (workerapi.DeleteComputerResponse, error) {
	var response workerapi.DeleteComputerResponse
	if err := c.postWorkerJSON(ctx, "/worker/v1/run/computers/delete", request, &response); err != nil {
		return workerapi.DeleteComputerResponse{}, err
	}
	return response, nil
}

func (c *Client) InvokeChildTask(
	ctx context.Context,
	request workerapi.InvokeChildTaskRequest,
) (workerapi.InvokeChildTaskResponse, error) {
	var response workerapi.InvokeChildTaskResponse
	if err := c.postWorkerJSON(ctx, "/worker/v1/run/tasks/invoke", request, &response); err != nil {
		return workerapi.InvokeChildTaskResponse{}, err
	}
	return response, nil
}

func (c *Client) CompleteTask(
	ctx context.Context,
	request workerapi.CompleteTaskRequest,
) error {
	return c.postWorkerJSON(
		ctx,
		"/worker/v1/run/tasks/complete",
		request,
		nil,
	)
}

func (c *Client) CompleteActor(
	ctx context.Context,
	request workerapi.CompleteActorRequest,
) error {
	return c.postWorkerJSON(ctx, "/worker/v1/run/sessions/complete", request, nil)
}

func (c *Client) AppendRunLog(
	ctx context.Context,
	lease workerapi.RunLeaseAssignment,
	stream workerapi.LogStream,
	observedSeq uint64,
	content []byte,
) error {
	return c.postWorkerJSON(ctx, "/worker/v1/run/logs/append", workerapi.RunLogAppendRequest{
		Lease:         lease.Fence(),
		Stream:        stream,
		ObservedSeq:   observedSeq,
		ContentBase64: base64.StdEncoding.EncodeToString(content),
	}, nil)
}

func (c *Client) UpdateRunMetadata(ctx context.Context, request workerapi.UpdateRunMetadataRequest) error {
	return c.postWorkerJSON(ctx, "/worker/v1/run/metadata/update", request, nil)
}

func (c *Client) AppendStructuredRunLog(ctx context.Context, request workerapi.StructuredLogRequest) error {
	return c.postWorkerJSON(ctx, "/worker/v1/run/structured-logs/append", request, nil)
}

func (c *Client) CreateRuntimeToken(ctx context.Context, request workerapi.CreateTokenRequest) (api.TokenResponse, error) {
	var response api.TokenResponse
	if err := c.postWorkerJSON(ctx, "/worker/v1/run/tokens/create", request, &response); err != nil {
		return api.TokenResponse{}, err
	}
	return response, nil
}

func (c *Client) CreateRunWait(ctx context.Context, request workerapi.CreateRunWaitRequest) (workerapi.CreateRunWaitResponse, error) {
	var response workerapi.CreateRunWaitResponse
	if err := c.postWorkerJSON(ctx, "/worker/v1/run/waits/create", request, &response); err != nil {
		return workerapi.CreateRunWaitResponse{}, err
	}
	return response, nil
}

func (c *Client) PollRunWait(ctx context.Context, request workerapi.RunWaitPollRequest) (workerapi.RunWaitPollResponse, error) {
	var response workerapi.RunWaitPollResponse
	if err := c.postWorkerJSON(ctx, "/worker/v1/run/waits/poll", request, &response); err != nil {
		return workerapi.RunWaitPollResponse{}, err
	}
	return response, nil
}

func (c *Client) AcknowledgeRunWaitResume(ctx context.Context, request workerapi.RunWaitResumeAckRequest) (workerapi.RunWaitResumeAckResponse, error) {
	var response workerapi.RunWaitResumeAckResponse
	if err := c.postWorkerJSON(ctx, "/worker/v1/run/waits/resume-ack", request, &response); err != nil {
		return workerapi.RunWaitResumeAckResponse{}, err
	}
	return response, nil
}

func (c *Client) RegisterCheckpoint(ctx context.Context, request workerapi.RegisterCheckpointRequest) (workerapi.ComputerCheckpointResponse, error) {
	var response workerapi.ComputerCheckpointResponse
	if err := c.postWorkerJSON(ctx, "/worker/v1/computer/checkpoints/register", request, &response); err != nil {
		return workerapi.ComputerCheckpointResponse{}, err
	}
	return response, nil
}

func (c *Client) MarkCheckpointReady(ctx context.Context, request workerapi.CheckpointReadyRequest) (workerapi.ComputerCheckpointResponse, error) {
	var response workerapi.ComputerCheckpointResponse
	if err := c.postWorkerJSON(ctx, "/worker/v1/computer/checkpoints/ready", request, &response); err != nil {
		return workerapi.ComputerCheckpointResponse{}, err
	}
	return response, nil
}

func (c *Client) MarkCheckpointFailed(ctx context.Context, request workerapi.CheckpointFailedRequest) (workerapi.ComputerCheckpointResponse, error) {
	var response workerapi.ComputerCheckpointResponse
	if err := c.postWorkerJSON(ctx, "/worker/v1/computer/checkpoints/failed", request, &response); err != nil {
		return workerapi.ComputerCheckpointResponse{}, err
	}
	return response, nil
}

func (c *Client) EnqueueRunSession(ctx context.Context, request workerapi.SubmitSessionDataRequest) (workerapi.SubmitSessionDataResponse, error) {
	var response workerapi.SubmitSessionDataResponse
	if err := c.postWorkerJSON(ctx, "/worker/v1/run/sessions/enqueue", request, &response); err != nil {
		return workerapi.SubmitSessionDataResponse{}, err
	}
	return response, nil
}

func (c *Client) SendRunTurnMessage(ctx context.Context, request workerapi.SubmitSessionDataRequest) (workerapi.SubmitSessionDataResponse, error) {
	var response workerapi.SubmitSessionDataResponse
	if err := c.postWorkerJSON(ctx, "/worker/v1/run/turns/messages/send", request, &response); err != nil {
		return workerapi.SubmitSessionDataResponse{}, err
	}
	return response, nil
}

func (c *Client) WriteSessionOutput(ctx context.Context, request workerapi.WriteSessionOutputRequest) (workerapi.WriteOutputResponse, error) {
	var response workerapi.WriteOutputResponse
	if err := c.postWorkerJSON(ctx, "/worker/v1/run/sessions/output/write", request, &response); err != nil {
		return workerapi.WriteOutputResponse{}, err
	}
	return response, nil
}

func (c *Client) TurnMessagesReady(ctx context.Context, request workerapi.TurnExecutionRequest) (workerapi.TurnCommandResponse, error) {
	var response workerapi.TurnCommandResponse
	if err := c.postWorkerJSON(ctx, "/worker/v1/run/turns/messages/ready", request, &response); err != nil {
		return workerapi.TurnCommandResponse{}, err
	}
	return response, nil
}

func (c *Client) ClaimTurnMessage(ctx context.Context, request workerapi.ClaimTurnMessageRequest) (workerapi.ClaimTurnMessageResponse, error) {
	var response workerapi.ClaimTurnMessageResponse
	if err := c.postWorkerJSON(ctx, "/worker/v1/run/turns/messages/claim", request, &response); err != nil {
		return workerapi.ClaimTurnMessageResponse{}, err
	}
	return response, nil
}

func (c *Client) CompleteTurnMessage(ctx context.Context, request workerapi.CompleteTurnMessageRequest) (workerapi.TurnCommandResponse, error) {
	var response workerapi.TurnCommandResponse
	if err := c.postWorkerJSON(ctx, "/worker/v1/run/turns/messages/complete", request, &response); err != nil {
		return workerapi.TurnCommandResponse{}, err
	}
	return response, nil
}

func (c *Client) BeginTurnSettlement(ctx context.Context, request workerapi.TurnExecutionRequest) (workerapi.TurnCommandResponse, error) {
	var response workerapi.TurnCommandResponse
	if err := c.postWorkerJSON(ctx, "/worker/v1/run/turns/settlement/begin", request, &response); err != nil {
		return workerapi.TurnCommandResponse{}, err
	}
	return response, nil
}

func (c *Client) ReadSessionControl(ctx context.Context, request workerapi.SessionControlRequest) (workerapi.SessionControlResponse, error) {
	var response workerapi.SessionControlResponse
	if err := c.postWorkerJSON(ctx, "/worker/v1/run/sessions/control", request, &response); err != nil {
		return workerapi.SessionControlResponse{}, err
	}
	return response, nil
}

func (c *Client) GetRunSessionTurn(ctx context.Context, request workerapi.TurnReferenceRequest) (workerapi.SessionTurnResponse, error) {
	var response workerapi.SessionTurnResponse
	if err := c.postWorkerJSON(ctx, "/worker/v1/run/turns/retrieve", request, &response); err != nil {
		return workerapi.SessionTurnResponse{}, err
	}
	return response, nil
}

func (c *Client) InterruptRunSessionTurn(ctx context.Context, request workerapi.InterruptSessionTurnRequest) (workerapi.InterruptSessionTurnResponse, error) {
	var response workerapi.InterruptSessionTurnResponse
	if err := c.postWorkerJSON(ctx, "/worker/v1/run/turns/interrupt", request, &response); err != nil {
		return workerapi.InterruptSessionTurnResponse{}, err
	}
	return response, nil
}

func (c *Client) ResumeRunSession(ctx context.Context, request workerapi.ResumeSessionRequest) (workerapi.ResumeSessionResponse, error) {
	var response workerapi.ResumeSessionResponse
	if err := c.postWorkerJSON(ctx, "/worker/v1/run/sessions/resume", request, &response); err != nil {
		return workerapi.ResumeSessionResponse{}, err
	}
	return response, nil
}

func (c *Client) RegisterInitialComputerObject(ctx context.Context, request workerapi.InitialComputerObjectRequest) error {
	return c.postWorkerJSON(ctx, "/worker/v1/run/computer-instances/initialization/objects/register", request, &struct{}{})
}
func (c *Client) CertifyInitialComputerObject(ctx context.Context, request workerapi.InitialComputerObjectRequest) error {
	return c.postWorkerJSON(ctx, "/worker/v1/run/computer-instances/initialization/objects/certify", request, &struct{}{})
}

func (c *Client) RegisterCheckpointComputerObject(ctx context.Context, request workerapi.CheckpointComputerObjectRequest) error {
	return c.postWorkerJSON(ctx, "/worker/v1/computer/checkpoints/objects/register", request, nil)
}
func (c *Client) CertifyCheckpointComputerObject(ctx context.Context, request workerapi.CheckpointComputerObjectRequest) error {
	return c.postWorkerJSON(ctx, "/worker/v1/computer/checkpoints/objects/certify", request, nil)
}
func (c *Client) ReuseCheckpointComputerObject(ctx context.Context, request workerapi.CheckpointComputerObjectRequest) error {
	return c.postWorkerJSON(ctx, "/worker/v1/computer/checkpoints/objects/reuse", request, nil)
}

func (c *Client) AcknowledgeComputerRestore(ctx context.Context, request workerapi.ComputerRestoreAckRequest) (workerapi.ComputerRestoreAckResponse, error) {
	var response workerapi.ComputerRestoreAckResponse
	if err := c.postWorkerJSON(ctx, "/worker/v1/computer/restores/ack", request, &response); err != nil {
		return workerapi.ComputerRestoreAckResponse{}, err
	}
	return response, nil
}

func (c *Client) ReconcileComputerCommand(ctx context.Context, r workerapi.ComputerCommandCompleteRequest) error {
	return c.postWorkerJSON(ctx, "/worker/v1/run/computer-commands/reconcile", r, nil)
}

func (c *Client) GetComputerRestorePlan(ctx context.Context, request workerapi.ComputerRestorePlanRequest) (workerapi.ComputerRestorePlanResponse, error) {
	var response workerapi.ComputerRestorePlanResponse
	if err := c.postWorkerJSON(ctx, "/worker/v1/computer/restores/plan", request, &response); err != nil {
		return workerapi.ComputerRestorePlanResponse{}, err
	}
	return response, nil
}

func (c *Client) GetComputerRunCleanup(ctx context.Context, r workerapi.ComputerRunCleanupRequest) (workerapi.ComputerRunCleanupResponse, error) {
	var response workerapi.ComputerRunCleanupResponse
	err := c.postWorkerJSON(ctx, "/worker/v1/run/computer-instances/runs/cleanup", r, &response)
	return response, err
}
func (c *Client) ReconcileComputerRun(ctx context.Context, r workerapi.ComputerRunReconcileRequest) error {
	return c.postWorkerJSON(ctx, "/worker/v1/run/computer-instances/runs/reconcile", r, nil)
}
