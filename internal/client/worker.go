package client

import (
	"context"
	"encoding/base64"
	"encoding/json"

	"github.com/helmrdotdev/helmr/internal/api"
)

func (c *Client) RegisterWorker(ctx context.Context, bootstrapToken string, resourceID string) (api.WorkerRegisterResponse, error) {
	var response api.WorkerRegisterResponse
	if err := c.postJSON(ctx, "/api/worker/register", api.WorkerRegisterRequest{
		BootstrapToken: bootstrapToken,
		ResourceID:     resourceID,
	}, &response); err != nil {
		return api.WorkerRegisterResponse{}, err
	}
	return response, nil
}

func (c *Client) LeaseRun(ctx context.Context, capabilities api.WorkerCapabilities) (api.WorkerRunLeaseResponse, error) {
	var response api.WorkerRunLeaseResponse
	if err := c.postWorkerJSON(ctx, "/api/worker/leases/lease", api.WorkerRunLeaseRequest{Capabilities: capabilities}, &response); err != nil {
		return api.WorkerRunLeaseResponse{}, err
	}
	return response, nil
}

func (c *Client) LeaseDeploymentBuild(ctx context.Context, capabilities api.WorkerCapabilities) (api.WorkerDeploymentBuildLeaseResponse, error) {
	var response api.WorkerDeploymentBuildLeaseResponse
	if err := c.postWorkerJSON(ctx, "/api/worker/deployments/lease", api.WorkerDeploymentBuildLeaseRequest{Capabilities: capabilities}, &response); err != nil {
		return api.WorkerDeploymentBuildLeaseResponse{}, err
	}
	return response, nil
}

func (c *Client) ClaimWorkspaceMaterialization(ctx context.Context, capabilities api.WorkerCapabilities) (api.WorkerWorkspaceMaterializationClaimResponse, error) {
	var response api.WorkerWorkspaceMaterializationClaimResponse
	if err := c.postWorkerJSON(ctx, "/api/worker/workspaces/materializations/claim", api.WorkerWorkspaceMaterializationClaimRequest{Capabilities: capabilities}, &response); err != nil {
		return api.WorkerWorkspaceMaterializationClaimResponse{}, err
	}
	return response, nil
}

func (c *Client) RenewWorkspaceMaterialization(ctx context.Context, request api.WorkerWorkspaceMaterializationRenewRequest) (api.WorkspaceMaterializationResponse, error) {
	var response api.WorkspaceMaterializationResponse
	if err := c.postWorkerJSON(ctx, "/api/worker/workspaces/materializations/renew", request, &response); err != nil {
		return api.WorkspaceMaterializationResponse{}, err
	}
	return response, nil
}

func (c *Client) MarkWorkspaceMaterializationRunning(ctx context.Context, request api.WorkerWorkspaceMaterializationRunningRequest) (api.WorkspaceMaterializationResponse, error) {
	var response api.WorkspaceMaterializationResponse
	if err := c.postWorkerJSON(ctx, "/api/worker/workspaces/materializations/running", request, &response); err != nil {
		return api.WorkspaceMaterializationResponse{}, err
	}
	return response, nil
}

func (c *Client) StopWorkspaceMaterialization(ctx context.Context, request api.WorkerWorkspaceMaterializationStopRequest) (api.WorkspaceMaterializationResponse, error) {
	var response api.WorkspaceMaterializationResponse
	if err := c.postWorkerJSON(ctx, "/api/worker/workspaces/materializations/stop", request, &response); err != nil {
		return api.WorkspaceMaterializationResponse{}, err
	}
	return response, nil
}

func (c *Client) FailWorkspaceMaterialization(ctx context.Context, request api.WorkerWorkspaceMaterializationFailRequest) (api.WorkspaceMaterializationResponse, error) {
	var response api.WorkspaceMaterializationResponse
	if err := c.postWorkerJSON(ctx, "/api/worker/workspaces/materializations/fail", request, &response); err != nil {
		return api.WorkspaceMaterializationResponse{}, err
	}
	return response, nil
}

func (c *Client) ClaimWorkspaceMaterializationOperation(ctx context.Context, request api.WorkerWorkspaceOperationClaimRequest) (api.WorkerWorkspaceOperationClaimResponse, error) {
	var response api.WorkerWorkspaceOperationClaimResponse
	if err := c.postWorkerJSON(ctx, "/api/worker/workspaces/materializations/operations/claim", request, &response); err != nil {
		return api.WorkerWorkspaceOperationClaimResponse{}, err
	}
	return response, nil
}

func (c *Client) StartWorkspaceMaterializationOperation(ctx context.Context, request api.WorkerWorkspaceOperationStartRequest) (api.WorkspaceOperationResponse, error) {
	var response api.WorkspaceOperationResponse
	if err := c.postWorkerJSON(ctx, "/api/worker/workspaces/materializations/operations/start", request, &response); err != nil {
		return api.WorkspaceOperationResponse{}, err
	}
	return response, nil
}

func (c *Client) CompleteWorkspaceMaterializationOperation(ctx context.Context, request api.WorkerWorkspaceOperationCompleteRequest) (api.WorkspaceOperationResponse, error) {
	var response api.WorkspaceOperationResponse
	if err := c.postWorkerJSON(ctx, "/api/worker/workspaces/materializations/operations/complete", request, &response); err != nil {
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

func (c *Client) ActivateWorker(ctx context.Context, capabilities api.WorkerCapabilities) (api.WorkerStatusResponse, error) {
	var response api.WorkerStatusResponse
	if err := c.postWorkerJSON(ctx, "/api/worker/activate", api.WorkerActivateRequest{Capabilities: capabilities}, &response); err != nil {
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

func (c *Client) GetWorkerStatus(ctx context.Context) (api.WorkerStatusResponse, error) {
	var response api.WorkerStatusResponse
	if err := c.getWorkerJSON(ctx, "/api/worker/status", &response); err != nil {
		return api.WorkerStatusResponse{}, err
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

func (c *Client) WriteOutput(ctx context.Context, request api.WorkerWriteOutputRequest) (api.WorkerEventResponse, error) {
	var response api.WorkerEventResponse
	if len(request.Payload) == 0 {
		request.Payload = json.RawMessage(`null`)
	}
	if err := c.postWorkerJSON(ctx, "/api/worker/leases/channels", request, &response); err != nil {
		return api.WorkerEventResponse{}, err
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

func (c *Client) CreateRuntimeWaitpointToken(ctx context.Context, request api.WorkerCreateWaitpointTokenRequest) (api.WaitpointTokenResponse, error) {
	var response api.WaitpointTokenResponse
	if err := c.postWorkerJSON(ctx, "/api/worker/leases/waitpoint-tokens", request, &response); err != nil {
		return api.WaitpointTokenResponse{}, err
	}
	return response, nil
}

func (c *Client) CreateWaitpoint(ctx context.Context, request api.WorkerCreateWaitpointRequest) (api.WorkerCreateWaitpointResponse, error) {
	var response api.WorkerCreateWaitpointResponse
	if err := c.postWorkerJSON(ctx, "/api/worker/leases/waitpoints", request, &response); err != nil {
		return api.WorkerCreateWaitpointResponse{}, err
	}
	return response, nil
}

func (c *Client) MarkCheckpointReady(ctx context.Context, request api.WorkerCheckpointReadyRequest) (api.WorkerCreateWaitpointResponse, error) {
	var response api.WorkerCreateWaitpointResponse
	if err := c.postWorkerJSON(ctx, "/api/worker/leases/checkpoints/ready", request, &response); err != nil {
		return api.WorkerCreateWaitpointResponse{}, err
	}
	return response, nil
}

func (c *Client) MarkCheckpointFailed(ctx context.Context, request api.WorkerCheckpointFailedRequest) (api.WorkerCreateWaitpointResponse, error) {
	var response api.WorkerCreateWaitpointResponse
	if err := c.postWorkerJSON(ctx, "/api/worker/leases/checkpoints/failed", request, &response); err != nil {
		return api.WorkerCreateWaitpointResponse{}, err
	}
	return response, nil
}
