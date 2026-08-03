package workerclient

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/httpclient"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func (c *Client) CreateWorkerEnrollmentChallenge(ctx context.Context, workerGroupID string) (workerapi.EnrollmentChallengeResponse, error) {
	var response workerapi.EnrollmentChallengeResponse
	if err := c.postJSON(ctx, "/api/worker/enrollment/challenge", workerapi.EnrollmentChallengeRequest{WorkerGroupID: workerGroupID}, &response); err != nil {
		return workerapi.EnrollmentChallengeResponse{}, err
	}
	return response, nil
}

func (c *Client) EnrollWorker(ctx context.Context, request workerapi.EnrollmentRequest) (workerapi.EnrollmentResponse, error) {
	var response workerapi.EnrollmentResponse
	if err := c.postJSON(ctx, "/api/worker/enrollment", request, &response); err != nil {
		return workerapi.EnrollmentResponse{}, err
	}
	return response, nil
}

func (c *Client) DiscoverRunLeases(ctx context.Context) (workerapi.RunLeaseDiscoveryResponse, error) {
	var response workerapi.RunLeaseDiscoveryResponse
	if err := c.postWorkerJSON(
		ctx,
		"/api/worker/leases/discover",
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
		"/api/worker/leases/claim",
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
		"/api/worker/leases/start",
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
		"/api/worker/leases/resume-release",
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
		"/api/worker/leases/entrypoint",
		request,
		nil,
	)
}

func (c *Client) RejectRun(ctx context.Context, request workerapi.RejectRunRequest) error {
	return c.postWorkerJSON(ctx, "/api/worker/leases/reject", request, nil)
}

func (c *Client) NextPlatformAcquisition(ctx context.Context) (workerapi.PlatformAcquisitionResponse, error) {
	var response workerapi.PlatformAcquisitionResponse
	if err := c.postWorkerJSON(
		ctx,
		"/api/worker/platform-acquisitions/next",
		workerapi.PlatformAcquisitionRequest{},
		&response,
	); err != nil {
		return workerapi.PlatformAcquisitionResponse{}, err
	}
	return response, nil
}

func (c *Client) CompletePlatformAcquisition(
	ctx context.Context,
	request workerapi.PlatformAcquisitionCompleteRequest,
) (workerapi.PlatformAcquisitionResult, error) {
	var response workerapi.PlatformAcquisitionResult
	if err := c.postWorkerJSON(
		ctx,
		"/api/worker/platform-acquisitions/complete",
		request,
		&response,
	); err != nil {
		return workerapi.PlatformAcquisitionResult{}, err
	}
	return response, nil
}

func (c *Client) FailPlatformAcquisition(
	ctx context.Context,
	request workerapi.PlatformAcquisitionFailRequest,
) (workerapi.PlatformAcquisitionResult, error) {
	var response workerapi.PlatformAcquisitionResult
	if err := c.postWorkerJSON(
		ctx,
		"/api/worker/platform-acquisitions/fail",
		request,
		&response,
	); err != nil {
		return workerapi.PlatformAcquisitionResult{}, err
	}
	return response, nil
}

func (c *Client) LeaseDeploymentBuild(ctx context.Context) (workerapi.DeploymentBuildLeaseResponse, error) {
	var response workerapi.DeploymentBuildLeaseResponse
	if err := c.postWorkerJSON(ctx, "/api/worker/deployments/lease", workerapi.DeploymentBuildLeaseRequest{}, &response); err != nil {
		return workerapi.DeploymentBuildLeaseResponse{}, err
	}
	return response, nil
}

func (c *Client) StartDeploymentBuild(ctx context.Context, lease workerapi.DeploymentBuildLease) (workerapi.DeploymentBuildStartResponse, error) {
	var response workerapi.DeploymentBuildStartResponse
	if err := c.postWorkerJSON(ctx, "/api/worker/deployments/start", workerapi.DeploymentBuildStartRequest{Lease: lease}, &response); err != nil {
		return workerapi.DeploymentBuildStartResponse{}, err
	}
	return response, nil
}

func (c *Client) RenewDeploymentBuild(ctx context.Context, lease workerapi.DeploymentBuildLease) (workerapi.DeploymentBuildRenewResponse, error) {
	var response workerapi.DeploymentBuildRenewResponse
	if err := c.postWorkerJSON(ctx, "/api/worker/deployments/renew", workerapi.DeploymentBuildRenewRequest{Lease: lease}, &response); err != nil {
		return workerapi.DeploymentBuildRenewResponse{}, err
	}
	return response, nil
}

func (c *Client) AdmitWorkspaceImage(
	ctx context.Context,
	request workerapi.WorkspaceImageAdmissionRequest,
) (workerapi.WorkspaceImageAssignment, error) {
	var response workerapi.WorkspaceImageAssignment
	if err := c.postWorkerJSON(ctx, "/api/worker/deployments/workspace-images/admit", request, &response); err != nil {
		return workerapi.WorkspaceImageAssignment{}, err
	}
	return response, nil
}

func (c *Client) FetchWorkspaceImageCredentials(
	ctx context.Context,
	request workerapi.WorkspaceImageCredentialRequest,
) (workerapi.WorkspaceImageCredentialResponse, error) {
	var response workerapi.WorkspaceImageCredentialResponse
	if err := c.postWorkerJSON(ctx, "/api/worker/deployments/workspace-images/credentials", request, &response); err != nil {
		return workerapi.WorkspaceImageCredentialResponse{}, err
	}
	return response, nil
}

func (c *Client) CompleteWorkspaceImage(
	ctx context.Context,
	request workerapi.WorkspaceImageOperationResultRequest,
) (workerapi.WorkspaceImageOperationResultResponse, error) {
	var response workerapi.WorkspaceImageOperationResultResponse
	if err := c.postWorkerJSON(ctx, "/api/worker/deployments/workspace-images/complete", request, &response); err != nil {
		return workerapi.WorkspaceImageOperationResultResponse{}, err
	}
	return response, nil
}

func (c *Client) RejectDeploymentBuild(ctx context.Context, request workerapi.DeploymentBuildRejectRequest) error {
	return c.postWorkerJSON(ctx, "/api/worker/deployments/reject", request, nil)
}

func (c *Client) ReportDeploymentBuildDeliveryFailure(
	ctx context.Context,
	request workerapi.DeploymentBuildDeliveryFailureRequest,
) (workerapi.DeploymentBuildResponse, error) {
	var response workerapi.DeploymentBuildResponse
	if err := c.postWorkerJSON(ctx, "/api/worker/deployments/delivery-failed", request, &response); err != nil {
		return workerapi.DeploymentBuildResponse{}, err
	}
	return response, nil
}

func (c *Client) ClaimWorkspaceMount(ctx context.Context, capabilities workerapi.Capabilities) (workerapi.WorkspaceMountClaimResponse, error) {
	var response workerapi.WorkspaceMountClaimResponse
	if err := c.postWorkerJSON(ctx, "/api/worker/workspaces/mounts/claim", workerapi.WorkspaceMountClaimRequest{Capabilities: capabilities}, &response); err != nil {
		return workerapi.WorkspaceMountClaimResponse{}, err
	}
	return response, nil
}

func (c *Client) RenewWorkspaceMount(ctx context.Context, request workerapi.WorkspaceMountRenewRequest) (workerapi.WorkspaceMountResponse, error) {
	var response workerapi.WorkspaceMountResponse
	if err := c.postWorkerJSON(ctx, "/api/worker/workspaces/mounts/renew", request, &response); err != nil {
		return workerapi.WorkspaceMountResponse{}, err
	}
	return response, nil
}

func (c *Client) MarkWorkspaceMountMounted(ctx context.Context, request workerapi.WorkspaceMountMountedRequest) (workerapi.WorkspaceMountResponse, error) {
	var response workerapi.WorkspaceMountResponse
	if err := c.postWorkerJSON(ctx, "/api/worker/workspaces/mounts/mounted", request, &response); err != nil {
		return workerapi.WorkspaceMountResponse{}, err
	}
	return response, nil
}

func (c *Client) CaptureWorkspaceMount(ctx context.Context, request workerapi.WorkspaceMountCaptureRequest) (workerapi.WorkspaceMountCaptureResponse, error) {
	var response workerapi.WorkspaceMountCaptureResponse
	if err := c.postWorkerJSON(ctx, "/api/worker/workspaces/mounts/capture", request, &response); err != nil {
		return workerapi.WorkspaceMountCaptureResponse{}, err
	}
	return response, nil
}

func (c *Client) StopWorkspaceMount(ctx context.Context, request workerapi.WorkspaceMountStopRequest) (workerapi.WorkspaceMountResponse, error) {
	var response workerapi.WorkspaceMountResponse
	if err := c.postWorkerJSON(ctx, "/api/worker/workspaces/mounts/stop", request, &response); err != nil {
		return workerapi.WorkspaceMountResponse{}, err
	}
	return response, nil
}

func (c *Client) FailWorkspaceMount(ctx context.Context, request workerapi.WorkspaceMountFailRequest) (workerapi.WorkspaceMountResponse, error) {
	var response workerapi.WorkspaceMountResponse
	if err := c.postWorkerJSON(ctx, "/api/worker/workspaces/mounts/fail", request, &response); err != nil {
		return workerapi.WorkspaceMountResponse{}, err
	}
	return response, nil
}

func (c *Client) ClaimWorkspaceExec(ctx context.Context, request workerapi.WorkspaceExecClaimRequest) (workerapi.WorkspaceExecClaimResponse, error) {
	var response workerapi.WorkspaceExecClaimResponse
	if err := c.postWorkerJSON(ctx, "/api/worker/workspaces/execs/claim", request, &response); err != nil {
		return workerapi.WorkspaceExecClaimResponse{}, err
	}
	return response, nil
}

func (c *Client) CompleteWorkspaceExec(ctx context.Context, request workerapi.WorkspaceExecCompleteRequest) (workerapi.WorkspaceMountResponse, error) {
	var response workerapi.WorkspaceMountResponse
	if err := c.postWorkerJSON(ctx, "/api/worker/workspaces/execs/complete", request, &response); err != nil {
		return workerapi.WorkspaceMountResponse{}, err
	}
	return response, nil
}

func (c *Client) AppendActorOutput(ctx context.Context, request workerapi.AppendActorOutputRequest) (workerapi.AppendActorOutputResponse, error) {
	var response workerapi.AppendActorOutputResponse
	if err := c.postWorkerJSON(ctx, "/api/worker/leases/actor-outputs", request, &response); err != nil {
		return workerapi.AppendActorOutputResponse{}, err
	}
	return response, nil
}

func (c *Client) RegisterRuntimeSubstrate(ctx context.Context, request workerapi.RuntimeSubstrateRegisterRequest) (workerapi.RuntimeSubstrateRegisterResponse, error) {
	var response workerapi.RuntimeSubstrateRegisterResponse
	if err := c.postWorkerJSON(ctx, "/api/worker/runtime-substrates/register", request, &response); err != nil {
		return workerapi.RuntimeSubstrateRegisterResponse{}, err
	}
	return response, nil
}

func (c *Client) ActivateWorker(ctx context.Context, capabilities workerapi.Capabilities) (workerapi.StatusResponse, error) {
	var response workerapi.StatusResponse
	if err := c.postWorkerJSON(ctx, "/api/worker/activate", workerapi.ActivateRequest{Capabilities: capabilities}, &response); err != nil {
		return workerapi.StatusResponse{}, err
	}
	return response, nil
}

func (c *Client) ReportWorkerStartupRecovery(ctx context.Context, request workerapi.StartupRecoveryRequest) error {
	return c.postWorkerJSON(ctx, "/api/worker/startup-recovery", request, nil)
}

func (c *Client) ObserveWorker(ctx context.Context, observation workerapi.Observation) (workerapi.StatusResponse, error) {
	var response workerapi.StatusResponse
	if err := c.postWorkerJSON(ctx, "/api/worker/observe", workerapi.ObserveRequest{Observation: observation}, &response); err != nil {
		return workerapi.StatusResponse{}, err
	}
	return response, nil
}

func (c *Client) DrainWorker(ctx context.Context) (workerapi.StatusResponse, error) {
	var response workerapi.StatusResponse
	if err := c.postWorkerJSON(ctx, "/api/worker/drain", struct{}{}, &response); err != nil {
		return workerapi.StatusResponse{}, err
	}
	return response, nil
}

func (c *Client) CompleteWorkerDrain(ctx context.Context, request workerapi.DrainCompletionRequest) (workerapi.StatusResponse, error) {
	const attempts = 3
	var lastErr error
	for attempt := range attempts {
		var response workerapi.StatusResponse
		lastErr = c.postWorkerJSON(ctx, "/api/worker/drain/complete", request, &response)
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
		lastErr = c.postWorkerJSON(ctx, "/api/worker/fence", request, nil)
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
	if err := c.getWorkerJSON(ctx, "/api/worker/status", &response); err != nil {
		return workerapi.StatusResponse{}, err
	}
	return response, nil
}

func (c *Client) NextRuntimeReconcileTarget(ctx context.Context) (workerapi.RuntimeReconcileResponse, error) {
	var response workerapi.RuntimeReconcileResponse
	if err := c.postWorkerJSON(ctx, "/api/worker/runtime-instances/reconcile", workerapi.RuntimeReconcileRequest{}, &response); err != nil {
		return workerapi.RuntimeReconcileResponse{}, err
	}
	return response, nil
}

func (c *Client) MarkRuntimeInstanceReady(ctx context.Context, request workerapi.RuntimeInstanceStateRequest) (workerapi.RuntimeInstance, error) {
	var response workerapi.RuntimeInstance
	if err := c.postWorkerJSON(ctx, "/api/worker/runtime-instances/ready", request, &response); err != nil {
		return workerapi.RuntimeInstance{}, err
	}
	return response, nil
}

func (c *Client) MarkRuntimeInstanceClosed(ctx context.Context, request workerapi.RuntimeInstanceStateRequest) (workerapi.RuntimeInstance, error) {
	var response workerapi.RuntimeInstance
	if err := c.postWorkerJSON(ctx, "/api/worker/runtime-instances/closed", request, &response); err != nil {
		return workerapi.RuntimeInstance{}, err
	}
	return response, nil
}

func (c *Client) MarkRuntimeInstanceFailed(ctx context.Context, request workerapi.RuntimeInstanceStateRequest) (workerapi.RuntimeInstance, error) {
	var response workerapi.RuntimeInstance
	if err := c.postWorkerJSON(ctx, "/api/worker/runtime-instances/failed", request, &response); err != nil {
		return workerapi.RuntimeInstance{}, err
	}
	return response, nil
}

func (c *Client) StartRun(ctx context.Context, lease workerapi.RunLease) (workerapi.StartResponse, error) {
	var response workerapi.StartResponse
	if err := c.postWorkerJSON(ctx, "/api/worker/leases/start", workerapi.StartRequest{Lease: lease}, &response); err != nil {
		return workerapi.StartResponse{}, err
	}
	return response, nil
}

func (c *Client) AcknowledgeRestore(ctx context.Context, request workerapi.AcknowledgeRestoreRequest) (workerapi.AcknowledgeRestoreResponse, error) {
	var response workerapi.AcknowledgeRestoreResponse
	if err := c.postWorkerJSON(ctx, "/api/worker/leases/restores/ack", request, &response); err != nil {
		return workerapi.AcknowledgeRestoreResponse{}, err
	}
	return response, nil
}

func (c *Client) RenewRun(ctx context.Context, lease workerapi.RunLease) (workerapi.RenewResponse, error) {
	var response workerapi.RenewResponse
	if err := c.postWorkerJSON(ctx, "/api/worker/leases/renew", workerapi.RenewRequest{Lease: lease}, &response); err != nil {
		return workerapi.RenewResponse{}, err
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
		"/api/worker/leases/run-renew",
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
		"/api/worker/leases/finalization/begin",
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
	if err := c.postWorkerJSON(ctx, "/api/worker/leases/actor-turns/commit", request, &response); err != nil {
		return workerapi.CommitActorTurnResponse{}, err
	}
	return response, nil
}

func (c *Client) SendRunActorInput(
	ctx context.Context,
	request workerapi.SendActorInputRequest,
) (workerapi.SendActorInputResponse, error) {
	var response workerapi.SendActorInputResponse
	if err := c.postWorkerJSON(ctx, "/api/worker/leases/actor-inputs/send", request, &response); err != nil {
		return workerapi.SendActorInputResponse{}, err
	}
	return response, nil
}

func (c *Client) StartRunActor(
	ctx context.Context,
	request workerapi.StartActorRequest,
) (workerapi.StartActorResponse, error) {
	var response workerapi.StartActorResponse
	if err := c.postWorkerJSON(ctx, "/api/worker/leases/actors/start", request, &response); err != nil {
		return workerapi.StartActorResponse{}, err
	}
	return response, nil
}

func (c *Client) GetRunActorStatus(
	ctx context.Context,
	request workerapi.ActorReferenceRequest,
) (workerapi.ActorStatusResponse, error) {
	var response workerapi.ActorStatusResponse
	if err := c.postWorkerJSON(ctx, "/api/worker/leases/actors/status", request, &response); err != nil {
		return workerapi.ActorStatusResponse{}, err
	}
	return response, nil
}

func (c *Client) CloseRunActor(
	ctx context.Context,
	request workerapi.CloseActorRequest,
) (workerapi.CloseActorResponse, error) {
	var response workerapi.CloseActorResponse
	if err := c.postWorkerJSON(ctx, "/api/worker/leases/actors/close", request, &response); err != nil {
		return workerapi.CloseActorResponse{}, err
	}
	return response, nil
}

func (c *Client) ReadRunActorOutputPage(
	ctx context.Context,
	request workerapi.ReadActorOutputPageRequest,
) (workerapi.ReadActorOutputPageResponse, error) {
	var response workerapi.ReadActorOutputPageResponse
	if err := c.postWorkerJSON(ctx, "/api/worker/leases/actors/output-page", request, &response); err != nil {
		return workerapi.ReadActorOutputPageResponse{}, err
	}
	return response, nil
}

func (c *Client) CreateRunWorkspace(
	ctx context.Context,
	request workerapi.CreateWorkspaceRequest,
) (workerapi.CreateWorkspaceResponse, error) {
	var response workerapi.CreateWorkspaceResponse
	if err := c.postWorkerJSON(ctx, "/api/worker/leases/workspaces/create", request, &response); err != nil {
		return workerapi.CreateWorkspaceResponse{}, err
	}
	return response, nil
}

func (c *Client) RetrieveRunWorkspace(
	ctx context.Context,
	request workerapi.RetrieveWorkspaceRequest,
) (workerapi.RetrieveWorkspaceResponse, error) {
	var response workerapi.RetrieveWorkspaceResponse
	if err := c.postWorkerJSON(ctx, "/api/worker/leases/workspaces/retrieve", request, &response); err != nil {
		return workerapi.RetrieveWorkspaceResponse{}, err
	}
	return response, nil
}

func (c *Client) ReadRunWorkspaceFile(
	ctx context.Context,
	request workerapi.ReadWorkspaceFileRequest,
) (workerapi.ReadWorkspaceFileResponse, error) {
	var response workerapi.ReadWorkspaceFileResponse
	if err := c.postWorkerJSON(ctx, "/api/worker/leases/workspaces/files/read", request, &response); err != nil {
		return workerapi.ReadWorkspaceFileResponse{}, err
	}
	return response, nil
}

func (c *Client) StatRunWorkspaceFile(
	ctx context.Context,
	request workerapi.ReadWorkspaceFileRequest,
) (workerapi.StatWorkspaceFileResponse, error) {
	var response workerapi.StatWorkspaceFileResponse
	if err := c.postWorkerJSON(ctx, "/api/worker/leases/workspaces/files/stat", request, &response); err != nil {
		return workerapi.StatWorkspaceFileResponse{}, err
	}
	return response, nil
}

func (c *Client) ListRunWorkspaceFiles(
	ctx context.Context,
	request workerapi.ListWorkspaceFilesRequest,
) (workerapi.ListWorkspaceFilesResponse, error) {
	var response workerapi.ListWorkspaceFilesResponse
	if err := c.postWorkerJSON(ctx, "/api/worker/leases/workspaces/files/list", request, &response); err != nil {
		return workerapi.ListWorkspaceFilesResponse{}, err
	}
	return response, nil
}

func (c *Client) ExecuteRunWorkspace(
	ctx context.Context,
	request workerapi.ExecuteWorkspaceRequest,
) (workerapi.ExecuteWorkspaceResponse, error) {
	var response workerapi.ExecuteWorkspaceResponse
	if err := c.postWorkerJSON(ctx, "/api/worker/leases/workspaces/exec", request, &response); err != nil {
		return workerapi.ExecuteWorkspaceResponse{}, err
	}
	return response, nil
}

func (c *Client) PollRunWorkspaceExec(
	ctx context.Context,
	request workerapi.PollWorkspaceExecRequest,
) (workerapi.ExecuteWorkspaceResponse, error) {
	var response workerapi.ExecuteWorkspaceResponse
	if err := c.postWorkerJSON(ctx, "/api/worker/leases/workspaces/exec/poll", request, &response); err != nil {
		return workerapi.ExecuteWorkspaceResponse{}, err
	}
	return response, nil
}

func (c *Client) DeleteRunWorkspace(
	ctx context.Context,
	request workerapi.DeleteWorkspaceRequest,
) (workerapi.DeleteWorkspaceResponse, error) {
	var response workerapi.DeleteWorkspaceResponse
	if err := c.postWorkerJSON(ctx, "/api/worker/leases/workspaces/delete", request, &response); err != nil {
		return workerapi.DeleteWorkspaceResponse{}, err
	}
	return response, nil
}

func (c *Client) InvokeChildTask(
	ctx context.Context,
	request workerapi.InvokeChildTaskRequest,
) (workerapi.InvokeChildTaskResponse, error) {
	var response workerapi.InvokeChildTaskResponse
	if err := c.postWorkerJSON(ctx, "/api/worker/leases/task-children/invoke", request, &response); err != nil {
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
		"/api/worker/leases/tasks/complete",
		request,
		nil,
	)
}

func (c *Client) CompleteActor(
	ctx context.Context,
	request workerapi.CompleteActorRequest,
) error {
	return c.postWorkerJSON(ctx, "/api/worker/leases/actors/complete", request, nil)
}

func (c *Client) ReleaseRun(ctx context.Context, lease workerapi.RunLease, result workerapi.ReleaseResult) (workerapi.ReleaseResponse, error) {
	var response workerapi.ReleaseResponse
	if err := c.postWorkerJSON(ctx, "/api/worker/leases/release", workerapi.ReleaseRequest{Lease: lease, Result: result}, &response); err != nil {
		return workerapi.ReleaseResponse{}, err
	}
	return response, nil
}

func (c *Client) CompleteDeploymentBuild(ctx context.Context, lease workerapi.DeploymentBuildLease, result json.RawMessage) (workerapi.DeploymentBuildResponse, error) {
	var response workerapi.DeploymentBuildResponse
	if err := c.postWorkerJSON(ctx, "/api/worker/deployments/complete", workerapi.CompleteDeploymentBuildRequest{Lease: lease, Result: result}, &response); err != nil {
		return workerapi.DeploymentBuildResponse{}, err
	}
	return response, nil
}

func (c *Client) AppendRunLog(
	ctx context.Context,
	lease workerapi.RunLeaseAssignment,
	stream workerapi.LogStream,
	observedSeq uint64,
	content []byte,
) error {
	return c.postWorkerJSON(ctx, "/api/worker/leases/run-logs", workerapi.RunLogAppendRequest{
		Lease:         lease.Fence(),
		Stream:        stream,
		ObservedSeq:   observedSeq,
		ContentBase64: base64.StdEncoding.EncodeToString(content),
	}, nil)
}

func (c *Client) UpdateRunMetadata(ctx context.Context, request workerapi.UpdateRunMetadataRequest) error {
	return c.postWorkerJSON(ctx, "/api/worker/leases/run-metadata", request, nil)
}

func (c *Client) AppendStructuredRunLog(ctx context.Context, request workerapi.StructuredLogRequest) error {
	return c.postWorkerJSON(ctx, "/api/worker/leases/structured-logs", request, nil)
}

func (c *Client) CreateRuntimeToken(ctx context.Context, request workerapi.CreateTokenRequest) (api.TokenResponse, error) {
	var response api.TokenResponse
	if err := c.postWorkerJSON(ctx, "/api/worker/leases/tokens", request, &response); err != nil {
		return api.TokenResponse{}, err
	}
	return response, nil
}

func (c *Client) CreateRunWait(ctx context.Context, request workerapi.CreateRunWaitRequest) (workerapi.CreateRunWaitResponse, error) {
	var response workerapi.CreateRunWaitResponse
	if err := c.postWorkerJSON(ctx, "/api/worker/leases/run-waits", request, &response); err != nil {
		return workerapi.CreateRunWaitResponse{}, err
	}
	return response, nil
}

func (c *Client) PollRunWait(ctx context.Context, request workerapi.RunWaitPollRequest) (workerapi.RunWaitPollResponse, error) {
	var response workerapi.RunWaitPollResponse
	if err := c.postWorkerJSON(ctx, "/api/worker/leases/run-waits/poll", request, &response); err != nil {
		return workerapi.RunWaitPollResponse{}, err
	}
	return response, nil
}

func (c *Client) AcknowledgeRunWaitResume(ctx context.Context, request workerapi.RunWaitResumeAckRequest) (workerapi.RunWaitResumeAckResponse, error) {
	var response workerapi.RunWaitResumeAckResponse
	if err := c.postWorkerJSON(ctx, "/api/worker/leases/run-waits/resume-ack", request, &response); err != nil {
		return workerapi.RunWaitResumeAckResponse{}, err
	}
	return response, nil
}

func (c *Client) MarkCheckpointReady(ctx context.Context, request workerapi.CheckpointReadyRequest) (workerapi.CheckpointResponse, error) {
	var response workerapi.CheckpointResponse
	if err := c.postWorkerJSON(ctx, "/api/worker/leases/checkpoints/ready", request, &response); err != nil {
		return workerapi.CheckpointResponse{}, err
	}
	return response, nil
}

func (c *Client) MarkCheckpointFailed(ctx context.Context, request workerapi.CheckpointFailedRequest) (workerapi.CheckpointResponse, error) {
	var response workerapi.CheckpointResponse
	if err := c.postWorkerJSON(ctx, "/api/worker/leases/checkpoints/failed", request, &response); err != nil {
		return workerapi.CheckpointResponse{}, err
	}
	return response, nil
}
