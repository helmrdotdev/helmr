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

func (c *Client) AcknowledgeRunResumeRelease(
	ctx context.Context,
	request workerapi.RunResumeReleaseRequest,
) (workerapi.RunResumeReleaseResponse, error) {
	var response workerapi.RunResumeReleaseResponse
	if err := c.postWorkerJSON(
		ctx,
		"/worker/v1/run/leases/resume-release",
		request,
		&response,
	); err != nil {
		return workerapi.RunResumeReleaseResponse{}, err
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

func (c *Client) ClaimWorkspaceMount(ctx context.Context, capabilities workerapi.Capabilities) (workerapi.WorkspaceMountClaimResponse, error) {
	var response workerapi.WorkspaceMountClaimResponse
	if err := c.postWorkerJSON(ctx, "/worker/v1/run/workspace-mounts/claim", workerapi.WorkspaceMountClaimRequest{Capabilities: capabilities}, &response); err != nil {
		return workerapi.WorkspaceMountClaimResponse{}, err
	}
	return response, nil
}

func (c *Client) RenewWorkspaceMount(ctx context.Context, request workerapi.WorkspaceMountRenewRequest) (workerapi.WorkspaceMountResponse, error) {
	var response workerapi.WorkspaceMountResponse
	if err := c.postWorkerJSON(ctx, "/worker/v1/run/workspace-mounts/renew", request, &response); err != nil {
		return workerapi.WorkspaceMountResponse{}, err
	}
	return response, nil
}

func (c *Client) MarkWorkspaceMountMounted(ctx context.Context, request workerapi.WorkspaceMountMountedRequest) (workerapi.WorkspaceMountResponse, error) {
	var response workerapi.WorkspaceMountResponse
	if err := c.postWorkerJSON(ctx, "/worker/v1/run/workspace-mounts/mounted", request, &response); err != nil {
		return workerapi.WorkspaceMountResponse{}, err
	}
	return response, nil
}

func (c *Client) CaptureWorkspaceMount(ctx context.Context, request workerapi.WorkspaceMountCaptureRequest) (workerapi.WorkspaceMountCaptureResponse, error) {
	var response workerapi.WorkspaceMountCaptureResponse
	if err := c.postWorkerJSON(ctx, "/worker/v1/run/workspace-mounts/capture", request, &response); err != nil {
		return workerapi.WorkspaceMountCaptureResponse{}, err
	}
	return response, nil
}

func (c *Client) StopWorkspaceMount(ctx context.Context, request workerapi.WorkspaceMountStopRequest) (workerapi.WorkspaceMountResponse, error) {
	var response workerapi.WorkspaceMountResponse
	if err := c.postWorkerJSON(ctx, "/worker/v1/run/workspace-mounts/stop", request, &response); err != nil {
		return workerapi.WorkspaceMountResponse{}, err
	}
	return response, nil
}

func (c *Client) FailWorkspaceMount(ctx context.Context, request workerapi.WorkspaceMountFailRequest) (workerapi.WorkspaceMountResponse, error) {
	var response workerapi.WorkspaceMountResponse
	if err := c.postWorkerJSON(ctx, "/worker/v1/run/workspace-mounts/fail", request, &response); err != nil {
		return workerapi.WorkspaceMountResponse{}, err
	}
	return response, nil
}

func (c *Client) ClaimWorkspaceExec(ctx context.Context, request workerapi.WorkspaceExecClaimRequest) (workerapi.WorkspaceExecClaimResponse, error) {
	var response workerapi.WorkspaceExecClaimResponse
	if err := c.postWorkerJSON(ctx, "/worker/v1/run/workspace-execs/claim", request, &response); err != nil {
		return workerapi.WorkspaceExecClaimResponse{}, err
	}
	return response, nil
}

func (c *Client) CompleteWorkspaceExec(ctx context.Context, request workerapi.WorkspaceExecCompleteRequest) (workerapi.WorkspaceMountResponse, error) {
	var response workerapi.WorkspaceMountResponse
	if err := c.postWorkerJSON(ctx, "/worker/v1/run/workspace-execs/complete", request, &response); err != nil {
		return workerapi.WorkspaceMountResponse{}, err
	}
	return response, nil
}

func (c *Client) AppendActorOutput(ctx context.Context, request workerapi.AppendActorOutputRequest) (workerapi.AppendActorOutputResponse, error) {
	var response workerapi.AppendActorOutputResponse
	if err := c.postWorkerJSON(ctx, "/worker/v1/run/sessions/outputs/append", request, &response); err != nil {
		return workerapi.AppendActorOutputResponse{}, err
	}
	return response, nil
}

func (c *Client) RegisterRuntimeSubstrate(ctx context.Context, request workerapi.RuntimeSubstrateRegisterRequest) (workerapi.RuntimeSubstrateRegisterResponse, error) {
	var response workerapi.RuntimeSubstrateRegisterResponse
	if err := c.postWorkerJSON(ctx, "/worker/v1/run/runtime-substrates/register", request, &response); err != nil {
		return workerapi.RuntimeSubstrateRegisterResponse{}, err
	}
	return response, nil
}

func (c *Client) ActivateWorker(ctx context.Context, capabilities workerapi.Capabilities) (workerapi.StatusResponse, error) {
	var response workerapi.StatusResponse
	if err := c.postWorkerJSON(ctx, "/worker/v1/instance/activate", workerapi.ActivateRequest{Capabilities: capabilities}, &response); err != nil {
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
	if err := c.postWorkerJSON(ctx, "/worker/v1/run/runtime-instances/reconcile", workerapi.RuntimeReconcileRequest{}, &response); err != nil {
		return workerapi.RuntimeReconcileResponse{}, err
	}
	return response, nil
}

func (c *Client) MarkRuntimeInstanceReady(ctx context.Context, request workerapi.RuntimeInstanceStateRequest) (workerapi.RuntimeInstance, error) {
	var response workerapi.RuntimeInstance
	if err := c.postWorkerJSON(ctx, "/worker/v1/run/runtime-instances/ready", request, &response); err != nil {
		return workerapi.RuntimeInstance{}, err
	}
	return response, nil
}

func (c *Client) MarkRuntimeInstanceClosed(ctx context.Context, request workerapi.RuntimeInstanceStateRequest) (workerapi.RuntimeInstance, error) {
	var response workerapi.RuntimeInstance
	if err := c.postWorkerJSON(ctx, "/worker/v1/run/runtime-instances/closed", request, &response); err != nil {
		return workerapi.RuntimeInstance{}, err
	}
	return response, nil
}

func (c *Client) MarkRuntimeInstanceFailed(ctx context.Context, request workerapi.RuntimeInstanceStateRequest) (workerapi.RuntimeInstance, error) {
	var response workerapi.RuntimeInstance
	if err := c.postWorkerJSON(ctx, "/worker/v1/run/runtime-instances/failed", request, &response); err != nil {
		return workerapi.RuntimeInstance{}, err
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

func (c *Client) SendRunActorInput(
	ctx context.Context,
	request workerapi.SendActorInputRequest,
) (workerapi.SendActorInputResponse, error) {
	var response workerapi.SendActorInputResponse
	if err := c.postWorkerJSON(ctx, "/worker/v1/run/sessions/inputs/send", request, &response); err != nil {
		return workerapi.SendActorInputResponse{}, err
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

func (c *Client) ReadRunSessionOutputPage(
	ctx context.Context,
	request workerapi.ReadSessionOutputPageRequest,
) (workerapi.ReadSessionOutputPageResponse, error) {
	var response workerapi.ReadSessionOutputPageResponse
	if err := c.postWorkerJSON(ctx, "/worker/v1/run/sessions/outputs/read-page", request, &response); err != nil {
		return workerapi.ReadSessionOutputPageResponse{}, err
	}
	return response, nil
}

func (c *Client) CreateRunWorkspace(
	ctx context.Context,
	request workerapi.CreateWorkspaceRequest,
) (workerapi.CreateWorkspaceResponse, error) {
	var response workerapi.CreateWorkspaceResponse
	if err := c.postWorkerJSON(ctx, "/worker/v1/run/workspaces/create", request, &response); err != nil {
		return workerapi.CreateWorkspaceResponse{}, err
	}
	return response, nil
}

func (c *Client) RetrieveRunWorkspace(
	ctx context.Context,
	request workerapi.RetrieveWorkspaceRequest,
) (workerapi.RetrieveWorkspaceResponse, error) {
	var response workerapi.RetrieveWorkspaceResponse
	if err := c.postWorkerJSON(ctx, "/worker/v1/run/workspaces/retrieve", request, &response); err != nil {
		return workerapi.RetrieveWorkspaceResponse{}, err
	}
	return response, nil
}

func (c *Client) ExecuteRunWorkspace(
	ctx context.Context,
	request workerapi.ExecuteWorkspaceRequest,
) (workerapi.ExecuteWorkspaceResponse, error) {
	var response workerapi.ExecuteWorkspaceResponse
	if err := c.postWorkerJSON(ctx, "/worker/v1/run/workspaces/exec", request, &response); err != nil {
		return workerapi.ExecuteWorkspaceResponse{}, err
	}
	return response, nil
}

func (c *Client) PollRunWorkspaceExec(
	ctx context.Context,
	request workerapi.PollWorkspaceExecRequest,
) (workerapi.ExecuteWorkspaceResponse, error) {
	var response workerapi.ExecuteWorkspaceResponse
	if err := c.postWorkerJSON(ctx, "/worker/v1/run/workspaces/exec/poll", request, &response); err != nil {
		return workerapi.ExecuteWorkspaceResponse{}, err
	}
	return response, nil
}

func (c *Client) DeleteRunWorkspace(
	ctx context.Context,
	request workerapi.DeleteWorkspaceRequest,
) (workerapi.DeleteWorkspaceResponse, error) {
	var response workerapi.DeleteWorkspaceResponse
	if err := c.postWorkerJSON(ctx, "/worker/v1/run/workspaces/delete", request, &response); err != nil {
		return workerapi.DeleteWorkspaceResponse{}, err
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

func (c *Client) MarkCheckpointReady(ctx context.Context, request workerapi.CheckpointReadyRequest) (workerapi.CheckpointResponse, error) {
	var response workerapi.CheckpointResponse
	if err := c.postWorkerJSON(ctx, "/worker/v1/run/checkpoints/ready", request, &response); err != nil {
		return workerapi.CheckpointResponse{}, err
	}
	return response, nil
}

func (c *Client) MarkCheckpointFailed(ctx context.Context, request workerapi.CheckpointFailedRequest) (workerapi.CheckpointResponse, error) {
	var response workerapi.CheckpointResponse
	if err := c.postWorkerJSON(ctx, "/worker/v1/run/checkpoints/failed", request, &response); err != nil {
		return workerapi.CheckpointResponse{}, err
	}
	return response, nil
}
