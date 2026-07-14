package client

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
)

func (c *Client) CreateWorkerEnrollmentChallenge(ctx context.Context, workerGroupID string) (api.WorkerEnrollmentChallengeResponse, error) {
	var response api.WorkerEnrollmentChallengeResponse
	if err := c.postJSON(ctx, "/api/worker/enrollment/challenge", api.WorkerEnrollmentChallengeRequest{WorkerGroupID: workerGroupID}, &response); err != nil {
		return api.WorkerEnrollmentChallengeResponse{}, err
	}
	return response, nil
}

func (c *Client) EnrollWorker(ctx context.Context, request api.WorkerEnrollmentRequest) (api.WorkerEnrollmentResponse, error) {
	var response api.WorkerEnrollmentResponse
	if err := c.postJSON(ctx, "/api/worker/enrollment", request, &response); err != nil {
		return api.WorkerEnrollmentResponse{}, err
	}
	return response, nil
}

func (c *Client) LeaseRun(ctx context.Context) (api.WorkerRunLeaseResponse, error) {
	var response api.WorkerRunLeaseResponse
	if err := c.postWorkerJSON(ctx, "/api/worker/leases/lease", api.WorkerRunLeaseRequest{}, &response); err != nil {
		return api.WorkerRunLeaseResponse{}, err
	}
	return response, nil
}

func (c *Client) RejectRun(ctx context.Context, request api.WorkerRejectRunRequest) error {
	return c.postWorkerJSON(ctx, "/api/worker/leases/reject", request, nil)
}

func (c *Client) LeaseDeploymentBuild(ctx context.Context) (api.WorkerDeploymentBuildLeaseResponse, error) {
	var response api.WorkerDeploymentBuildLeaseResponse
	if err := c.postWorkerJSON(ctx, "/api/worker/deployments/lease", api.WorkerDeploymentBuildLeaseRequest{}, &response); err != nil {
		return api.WorkerDeploymentBuildLeaseResponse{}, err
	}
	return response, nil
}

func (c *Client) StartDeploymentBuild(ctx context.Context, lease api.WorkerDeploymentBuildLease) (api.WorkerDeploymentBuildStartResponse, error) {
	var response api.WorkerDeploymentBuildStartResponse
	if err := c.postWorkerJSON(ctx, "/api/worker/deployments/start", api.WorkerDeploymentBuildStartRequest{Lease: lease}, &response); err != nil {
		return api.WorkerDeploymentBuildStartResponse{}, err
	}
	return response, nil
}

func (c *Client) RenewDeploymentBuild(ctx context.Context, lease api.WorkerDeploymentBuildLease) (api.WorkerDeploymentBuildRenewResponse, error) {
	var response api.WorkerDeploymentBuildRenewResponse
	if err := c.postWorkerJSON(ctx, "/api/worker/deployments/renew", api.WorkerDeploymentBuildRenewRequest{Lease: lease}, &response); err != nil {
		return api.WorkerDeploymentBuildRenewResponse{}, err
	}
	return response, nil
}

func (c *Client) RejectDeploymentBuild(ctx context.Context, request api.WorkerDeploymentBuildRejectRequest) error {
	return c.postWorkerJSON(ctx, "/api/worker/deployments/reject", request, nil)
}

func (c *Client) ClaimWorkspaceMount(ctx context.Context, capabilities api.WorkerCapabilities) (api.WorkerWorkspaceMountClaimResponse, error) {
	var response api.WorkerWorkspaceMountClaimResponse
	if err := c.postWorkerJSON(ctx, "/api/worker/workspaces/mounts/claim", api.WorkerWorkspaceMountClaimRequest{Capabilities: capabilities}, &response); err != nil {
		return api.WorkerWorkspaceMountClaimResponse{}, err
	}
	return response, nil
}

func (c *Client) RenewWorkspaceMount(ctx context.Context, request api.WorkerWorkspaceMountRenewRequest) (api.WorkspaceMountResponse, error) {
	var response api.WorkspaceMountResponse
	if err := c.postWorkerJSON(ctx, "/api/worker/workspaces/mounts/renew", request, &response); err != nil {
		return api.WorkspaceMountResponse{}, err
	}
	return response, nil
}

func (c *Client) MarkWorkspaceMountMounted(ctx context.Context, request api.WorkerWorkspaceMountMountedRequest) (api.WorkspaceMountResponse, error) {
	var response api.WorkspaceMountResponse
	if err := c.postWorkerJSON(ctx, "/api/worker/workspaces/mounts/mounted", request, &response); err != nil {
		return api.WorkspaceMountResponse{}, err
	}
	return response, nil
}

func (c *Client) CaptureWorkspaceMount(ctx context.Context, request api.WorkerWorkspaceMountCaptureRequest) (api.WorkerWorkspaceMountCaptureResponse, error) {
	var response api.WorkerWorkspaceMountCaptureResponse
	if err := c.postWorkerJSON(ctx, "/api/worker/workspaces/mounts/capture", request, &response); err != nil {
		return api.WorkerWorkspaceMountCaptureResponse{}, err
	}
	return response, nil
}

func (c *Client) StopWorkspaceMount(ctx context.Context, request api.WorkerWorkspaceMountStopRequest) (api.WorkspaceMountResponse, error) {
	var response api.WorkspaceMountResponse
	if err := c.postWorkerJSON(ctx, "/api/worker/workspaces/mounts/stop", request, &response); err != nil {
		return api.WorkspaceMountResponse{}, err
	}
	return response, nil
}

func (c *Client) FailWorkspaceMount(ctx context.Context, request api.WorkerWorkspaceMountFailRequest) (api.WorkspaceMountResponse, error) {
	var response api.WorkspaceMountResponse
	if err := c.postWorkerJSON(ctx, "/api/worker/workspaces/mounts/fail", request, &response); err != nil {
		return api.WorkspaceMountResponse{}, err
	}
	return response, nil
}

func (c *Client) ClaimWorkspaceOperation(ctx context.Context, request api.WorkerWorkspaceOperationClaimRequest) (api.WorkerWorkspaceOperationClaimResponse, error) {
	var response api.WorkerWorkspaceOperationClaimResponse
	if err := c.postWorkerJSON(ctx, "/api/worker/workspaces/mounts/operations/claim", request, &response); err != nil {
		return api.WorkerWorkspaceOperationClaimResponse{}, err
	}
	return response, nil
}

func (c *Client) StartWorkspaceOperation(ctx context.Context, request api.WorkerWorkspaceOperationStartRequest) (api.WorkspaceOperationResponse, error) {
	var response api.WorkspaceOperationResponse
	if err := c.postWorkerJSON(ctx, "/api/worker/workspaces/mounts/operations/start", request, &response); err != nil {
		return api.WorkspaceOperationResponse{}, err
	}
	return response, nil
}

func (c *Client) CompleteWorkspaceOperation(ctx context.Context, request api.WorkerWorkspaceOperationCompleteRequest) (api.WorkspaceOperationResponse, error) {
	var response api.WorkspaceOperationResponse
	if err := c.postWorkerJSON(ctx, "/api/worker/workspaces/mounts/operations/complete", request, &response); err != nil {
		return api.WorkspaceOperationResponse{}, err
	}
	return response, nil
}

func (c *Client) MarkWorkspaceExecStarted(ctx context.Context, request api.WorkerWorkspaceExecStartedRequest) (api.WorkspaceExecEnvelope, error) {
	var response api.WorkspaceExecEnvelope
	if err := c.postWorkerJSON(ctx, "/api/worker/workspaces/execs/started", request, &response); err != nil {
		return api.WorkspaceExecEnvelope{}, err
	}
	return response, nil
}

func (c *Client) AppendWorkspaceExecOutput(ctx context.Context, request api.WorkerWorkspaceExecOutputRequest) (api.ListWorkspaceExecStreamChunksResponse, error) {
	var response api.ListWorkspaceExecStreamChunksResponse
	if err := c.postWorkerJSON(ctx, "/api/worker/workspaces/execs/output", request, &response); err != nil {
		return api.ListWorkspaceExecStreamChunksResponse{}, err
	}
	return response, nil
}

func (c *Client) ListWorkspaceExecInput(ctx context.Context, request api.WorkerWorkspaceExecInputRequest) (api.WorkerWorkspaceExecInputResponse, error) {
	var response api.WorkerWorkspaceExecInputResponse
	if err := c.postWorkerJSON(ctx, "/api/worker/workspaces/execs/input", request, &response); err != nil {
		return api.WorkerWorkspaceExecInputResponse{}, err
	}
	return response, nil
}

func (c *Client) AdvanceWorkspaceExecInputDelivered(ctx context.Context, request api.WorkerWorkspaceExecInputDeliveredRequest) (api.WorkspaceExecEnvelope, error) {
	var response api.WorkspaceExecEnvelope
	if err := c.postWorkerJSON(ctx, "/api/worker/workspaces/execs/input-delivered", request, &response); err != nil {
		return api.WorkspaceExecEnvelope{}, err
	}
	return response, nil
}

func (c *Client) MarkWorkspaceExecExited(ctx context.Context, request api.WorkerWorkspaceExecExitedRequest) (api.WorkspaceExecEnvelope, error) {
	var response api.WorkspaceExecEnvelope
	if err := c.postWorkerJSON(ctx, "/api/worker/workspaces/execs/exited", request, &response); err != nil {
		return api.WorkspaceExecEnvelope{}, err
	}
	return response, nil
}

func (c *Client) MarkWorkspacePtyOpened(ctx context.Context, request api.WorkerWorkspacePtyOpenedRequest) (api.WorkspacePtyEnvelope, error) {
	var response api.WorkspacePtyEnvelope
	if err := c.postWorkerJSON(ctx, "/api/worker/workspaces/ptys/opened", request, &response); err != nil {
		return api.WorkspacePtyEnvelope{}, err
	}
	return response, nil
}

func (c *Client) AppendWorkspacePtyOutput(ctx context.Context, request api.WorkerWorkspacePtyOutputRequest) (api.ListWorkspacePtyStreamChunksResponse, error) {
	var response api.ListWorkspacePtyStreamChunksResponse
	if err := c.postWorkerJSON(ctx, "/api/worker/workspaces/ptys/output", request, &response); err != nil {
		return api.ListWorkspacePtyStreamChunksResponse{}, err
	}
	return response, nil
}

func (c *Client) ListWorkspacePtyInput(ctx context.Context, request api.WorkerWorkspacePtyInputRequest) (api.WorkerWorkspacePtyInputResponse, error) {
	var response api.WorkerWorkspacePtyInputResponse
	if err := c.postWorkerJSON(ctx, "/api/worker/workspaces/ptys/input", request, &response); err != nil {
		return api.WorkerWorkspacePtyInputResponse{}, err
	}
	return response, nil
}

func (c *Client) AdvanceWorkspacePtyInputDelivered(ctx context.Context, request api.WorkerWorkspacePtyInputDeliveredRequest) (api.WorkspacePtyEnvelope, error) {
	var response api.WorkspacePtyEnvelope
	if err := c.postWorkerJSON(ctx, "/api/worker/workspaces/ptys/input-delivered", request, &response); err != nil {
		return api.WorkspacePtyEnvelope{}, err
	}
	return response, nil
}

func (c *Client) MarkWorkspacePtyResizeApplied(ctx context.Context, request api.WorkerWorkspacePtyResizeAppliedRequest) (api.WorkspacePtyEnvelope, error) {
	var response api.WorkspacePtyEnvelope
	if err := c.postWorkerJSON(ctx, "/api/worker/workspaces/ptys/resize-applied", request, &response); err != nil {
		return api.WorkspacePtyEnvelope{}, err
	}
	return response, nil
}

func (c *Client) MarkWorkspacePtyClosed(ctx context.Context, request api.WorkerWorkspacePtyClosedRequest) (api.WorkspacePtyEnvelope, error) {
	var response api.WorkspacePtyEnvelope
	if err := c.postWorkerJSON(ctx, "/api/worker/workspaces/ptys/closed", request, &response); err != nil {
		return api.WorkspacePtyEnvelope{}, err
	}
	return response, nil
}

func (c *Client) RegisterRuntimeSubstrate(ctx context.Context, request api.WorkerRuntimeSubstrateRegisterRequest) (api.WorkerRuntimeSubstrateRegisterResponse, error) {
	var response api.WorkerRuntimeSubstrateRegisterResponse
	if err := c.postWorkerJSON(ctx, "/api/worker/runtime-substrates/register", request, &response); err != nil {
		return api.WorkerRuntimeSubstrateRegisterResponse{}, err
	}
	return response, nil
}

func (c *Client) LookupRuntimeSubstrate(ctx context.Context, request api.WorkerRuntimeSubstrateLookupRequest) (api.WorkerRuntimeSubstrateLookupResponse, error) {
	var response api.WorkerRuntimeSubstrateLookupResponse
	if err := c.postWorkerJSON(ctx, "/api/worker/runtime-substrates/lookup", request, &response); err != nil {
		return api.WorkerRuntimeSubstrateLookupResponse{}, err
	}
	return response, nil
}

func (c *Client) ActivateWorker(ctx context.Context, capabilities api.WorkerCapabilities) (api.WorkerStatusResponse, error) {
	var response api.WorkerStatusResponse
	if err := c.postWorkerJSON(ctx, "/api/worker/activate", api.WorkerActivateRequest{Capabilities: capabilities}, &response); err != nil {
		return api.WorkerStatusResponse{}, err
	}
	return response, nil
}

func (c *Client) ReportWorkerStartupRecovery(ctx context.Context, request api.WorkerStartupRecoveryRequest) error {
	return c.postWorkerJSON(ctx, "/api/worker/startup-recovery", request, nil)
}

func (c *Client) ObserveWorker(ctx context.Context, observation api.WorkerObservation) (api.WorkerStatusResponse, error) {
	var response api.WorkerStatusResponse
	if err := c.postWorkerJSON(ctx, "/api/worker/observe", api.WorkerObserveRequest{Observation: observation}, &response); err != nil {
		return api.WorkerStatusResponse{}, err
	}
	return response, nil
}

func (c *Client) RenewWorkerCertification(ctx context.Context, capabilities api.WorkerCapabilities) (api.WorkerStatusResponse, error) {
	var response api.WorkerStatusResponse
	if err := c.postWorkerJSON(ctx, "/api/worker/certification/renew", api.WorkerCertificationRenewRequest{Capabilities: capabilities}, &response); err != nil {
		return api.WorkerStatusResponse{}, err
	}
	return response, nil
}

func (c *Client) DrainWorker(ctx context.Context) (api.WorkerStatusResponse, error) {
	var response api.WorkerStatusResponse
	if err := c.postWorkerJSON(ctx, "/api/worker/drain", struct{}{}, &response); err != nil {
		return api.WorkerStatusResponse{}, err
	}
	return response, nil
}

func (c *Client) CompleteWorkerDrain(ctx context.Context, request api.WorkerDrainCompletionRequest) (api.WorkerStatusResponse, error) {
	const attempts = 3
	var lastErr error
	for attempt := range attempts {
		var response api.WorkerStatusResponse
		lastErr = c.postWorkerJSON(ctx, "/api/worker/drain/complete", request, &response)
		if lastErr == nil {
			return response, nil
		}
		if !ambiguousWorkerDrainCompletion(lastErr) || attempt == attempts-1 {
			break
		}
		delay := time.Duration(attempt+1) * 100 * time.Millisecond
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return api.WorkerStatusResponse{}, ctx.Err()
		case <-timer.C:
		}
	}
	return api.WorkerStatusResponse{}, fmt.Errorf("worker drain completion was not confirmed after %d identical attempts: %w", attempts, lastErr)
}

func ambiguousWorkerDrainCompletion(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var httpErr *HTTPError
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
	return c.postWorkerJSON(ctx, "/api/worker/fence", api.WorkerFenceRequest{ReasonCode: reasonCode}, nil)
}

func (c *Client) GetWorkerStatus(ctx context.Context) (api.WorkerStatusResponse, error) {
	var response api.WorkerStatusResponse
	if err := c.getWorkerJSON(ctx, "/api/worker/status", &response); err != nil {
		return api.WorkerStatusResponse{}, err
	}
	return response, nil
}

func (c *Client) NextRuntimeReconcileTarget(ctx context.Context) (api.WorkerRuntimeReconcileResponse, error) {
	var response api.WorkerRuntimeReconcileResponse
	if err := c.postWorkerJSON(ctx, "/api/worker/runtime-instances/reconcile", api.WorkerRuntimeReconcileRequest{}, &response); err != nil {
		return api.WorkerRuntimeReconcileResponse{}, err
	}
	return response, nil
}

func (c *Client) MarkRuntimeInstanceReady(ctx context.Context, request api.WorkerRuntimeInstanceStateRequest) (api.WorkerRuntimeInstance, error) {
	var response api.WorkerRuntimeInstance
	if err := c.postWorkerJSON(ctx, "/api/worker/runtime-instances/ready", request, &response); err != nil {
		return api.WorkerRuntimeInstance{}, err
	}
	return response, nil
}

func (c *Client) MarkRuntimeInstanceClosed(ctx context.Context, request api.WorkerRuntimeInstanceStateRequest) (api.WorkerRuntimeInstance, error) {
	var response api.WorkerRuntimeInstance
	if err := c.postWorkerJSON(ctx, "/api/worker/runtime-instances/closed", request, &response); err != nil {
		return api.WorkerRuntimeInstance{}, err
	}
	return response, nil
}

func (c *Client) MarkRuntimeInstanceFailed(ctx context.Context, request api.WorkerRuntimeInstanceStateRequest) (api.WorkerRuntimeInstance, error) {
	var response api.WorkerRuntimeInstance
	if err := c.postWorkerJSON(ctx, "/api/worker/runtime-instances/failed", request, &response); err != nil {
		return api.WorkerRuntimeInstance{}, err
	}
	return response, nil
}

func (c *Client) StartRun(ctx context.Context, lease api.WorkerRunLease) (api.WorkerStartResponse, error) {
	var response api.WorkerStartResponse
	if err := c.postWorkerJSON(ctx, "/api/worker/leases/start", api.WorkerStartRequest{Lease: lease}, &response); err != nil {
		return api.WorkerStartResponse{}, err
	}
	return response, nil
}

func (c *Client) AcknowledgeRestore(ctx context.Context, request api.WorkerAcknowledgeRestoreRequest) (api.WorkerAcknowledgeRestoreResponse, error) {
	var response api.WorkerAcknowledgeRestoreResponse
	if err := c.postWorkerJSON(ctx, "/api/worker/leases/restores/ack", request, &response); err != nil {
		return api.WorkerAcknowledgeRestoreResponse{}, err
	}
	return response, nil
}

func (c *Client) RenewRun(ctx context.Context, lease api.WorkerRunLease) (api.WorkerRenewResponse, error) {
	var response api.WorkerRenewResponse
	if err := c.postWorkerJSON(ctx, "/api/worker/leases/renew", api.WorkerRenewRequest{Lease: lease}, &response); err != nil {
		return api.WorkerRenewResponse{}, err
	}
	return response, nil
}

func (c *Client) ReleaseRun(ctx context.Context, lease api.WorkerRunLease, result api.WorkerReleaseResult) (api.WorkerReleaseResponse, error) {
	var response api.WorkerReleaseResponse
	if err := c.postWorkerJSON(ctx, "/api/worker/leases/release", api.WorkerReleaseRequest{Lease: lease, Result: result}, &response); err != nil {
		return api.WorkerReleaseResponse{}, err
	}
	return response, nil
}

func (c *Client) CompleteDeploymentBuild(ctx context.Context, lease api.WorkerDeploymentBuildLease, result api.WorkerDeploymentBuildResult) (api.WorkerDeploymentBuildResponse, error) {
	var response api.WorkerDeploymentBuildResponse
	if err := c.postWorkerJSON(ctx, "/api/worker/deployments/complete", api.WorkerCompleteDeploymentBuildRequest{Lease: lease, Result: result}, &response); err != nil {
		return api.WorkerDeploymentBuildResponse{}, err
	}
	return response, nil
}

func (c *Client) AppendLog(ctx context.Context, lease api.WorkerRunLease, stream api.WorkerLogStream, observedSeq uint64, content []byte) (api.WorkerEventResponse, error) {
	var response api.WorkerEventResponse
	if err := c.postWorkerJSON(ctx, "/api/worker/leases/logs", api.WorkerAppendLogRequest{
		Lease:         lease,
		Stream:        stream,
		ObservedSeq:   observedSeq,
		ContentBase64: base64.StdEncoding.EncodeToString(content),
	}, &response); err != nil {
		return api.WorkerEventResponse{}, err
	}
	return response, nil
}

func (c *Client) RecordLogEntry(ctx context.Context, lease api.WorkerRunLease, entry string) (api.WorkerEventResponse, error) {
	var response api.WorkerEventResponse
	if err := c.postWorkerJSON(ctx, "/api/worker/leases/log-entries", api.WorkerRecordLogEntryRequest{
		Lease: lease,
		Entry: entry,
	}, &response); err != nil {
		return api.WorkerEventResponse{}, err
	}
	return response, nil
}

func (c *Client) AppendOutputStream(ctx context.Context, request api.WorkerOutputStreamAppendRequest) (api.AppendStreamRecordResponse, error) {
	var response api.AppendStreamRecordResponse
	if len(request.Data) == 0 {
		request.Data = json.RawMessage(`null`)
	}
	if err := c.postWorkerJSON(ctx, "/api/worker/leases/streams/output", request, &response); err != nil {
		return api.AppendStreamRecordResponse{}, err
	}
	return response, nil
}

func (c *Client) ReadInputStream(ctx context.Context, request api.WorkerActiveStreamReadRequest) (api.WorkerActiveStreamReadResponse, error) {
	var response api.WorkerActiveStreamReadResponse
	if err := c.postWorkerJSON(ctx, "/api/worker/leases/streams/input/read", request, &response); err != nil {
		return api.WorkerActiveStreamReadResponse{}, err
	}
	return response, nil
}

func (c *Client) UpdateRunMetadata(ctx context.Context, request api.WorkerUpdateRunMetadataRequest) (api.WorkerEventResponse, error) {
	var response api.WorkerEventResponse
	if err := c.postWorkerJSON(ctx, "/api/worker/leases/metadata", request, &response); err != nil {
		return api.WorkerEventResponse{}, err
	}
	return response, nil
}

func (c *Client) CreateRuntimeToken(ctx context.Context, request api.WorkerCreateTokenRequest) (api.TokenResponse, error) {
	var response api.TokenResponse
	if err := c.postWorkerJSON(ctx, "/api/worker/leases/tokens", request, &response); err != nil {
		return api.TokenResponse{}, err
	}
	return response, nil
}

func (c *Client) CreateRunWait(ctx context.Context, request api.WorkerCreateRunWaitRequest) (api.WorkerCreateRunWaitResponse, error) {
	var response api.WorkerCreateRunWaitResponse
	if err := c.postWorkerJSON(ctx, "/api/worker/leases/run-waits", request, &response); err != nil {
		return api.WorkerCreateRunWaitResponse{}, err
	}
	return response, nil
}

func (c *Client) PollRunWait(ctx context.Context, request api.WorkerRunWaitPollRequest) (api.WorkerRunWaitPollResponse, error) {
	var response api.WorkerRunWaitPollResponse
	if err := c.postWorkerJSON(ctx, "/api/worker/leases/run-waits/poll", request, &response); err != nil {
		return api.WorkerRunWaitPollResponse{}, err
	}
	return response, nil
}

func (c *Client) AcknowledgeRunWaitResume(ctx context.Context, request api.WorkerRunWaitResumeAckRequest) (api.WorkerRunWaitResumeAckResponse, error) {
	var response api.WorkerRunWaitResumeAckResponse
	if err := c.postWorkerJSON(ctx, "/api/worker/leases/run-waits/resume-ack", request, &response); err != nil {
		return api.WorkerRunWaitResumeAckResponse{}, err
	}
	return response, nil
}

func (c *Client) CaptureRunWaitWorkspace(ctx context.Context, request api.WorkerRunWaitWorkspaceCaptureRequest) (api.WorkerRunWaitWorkspaceCaptureResponse, error) {
	var response api.WorkerRunWaitWorkspaceCaptureResponse
	if err := c.postWorkerJSON(ctx, "/api/worker/leases/run-waits/workspace-capture", request, &response); err != nil {
		return api.WorkerRunWaitWorkspaceCaptureResponse{}, err
	}
	return response, nil
}

func (c *Client) MarkCheckpointReady(ctx context.Context, request api.WorkerCheckpointReadyRequest) (api.WorkerCheckpointResponse, error) {
	var response api.WorkerCheckpointResponse
	if err := c.postWorkerJSON(ctx, "/api/worker/leases/checkpoints/ready", request, &response); err != nil {
		return api.WorkerCheckpointResponse{}, err
	}
	return response, nil
}

func (c *Client) MarkCheckpointFailed(ctx context.Context, request api.WorkerCheckpointFailedRequest) (api.WorkerCheckpointResponse, error) {
	var response api.WorkerCheckpointResponse
	if err := c.postWorkerJSON(ctx, "/api/worker/leases/checkpoints/failed", request, &response); err != nil {
		return api.WorkerCheckpointResponse{}, err
	}
	return response, nil
}
