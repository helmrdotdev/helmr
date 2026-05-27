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
	if err := c.postWorkerJSON(ctx, "/api/worker/executions/lease", api.WorkerRunLeaseRequest{Capabilities: capabilities}, &response); err != nil {
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
	if err := c.postWorkerJSON(ctx, "/api/worker/executions/start", api.WorkerStartRequest{Lease: lease}, &response); err != nil {
		return api.WorkerStartResponse{}, err
	}
	return response, nil
}

func (c *Client) AcknowledgeRestore(ctx context.Context, request api.WorkerAcknowledgeRestoreRequest) (api.WorkerAcknowledgeRestoreResponse, error) {
	var response api.WorkerAcknowledgeRestoreResponse
	if err := c.postWorkerJSON(ctx, "/api/worker/executions/restores/ack", request, &response); err != nil {
		return api.WorkerAcknowledgeRestoreResponse{}, err
	}
	return response, nil
}

func (c *Client) RenewRun(ctx context.Context, lease api.WorkerRunLease) (api.WorkerRenewResponse, error) {
	var response api.WorkerRenewResponse
	if err := c.postWorkerJSON(ctx, "/api/worker/executions/renew", api.WorkerRenewRequest{Lease: lease}, &response); err != nil {
		return api.WorkerRenewResponse{}, err
	}
	return response, nil
}

func (c *Client) ReleaseRun(ctx context.Context, lease api.WorkerRunLease, result api.WorkerReleaseResult) (api.WorkerReleaseResponse, error) {
	var response api.WorkerReleaseResponse
	if err := c.postWorkerJSON(ctx, "/api/worker/executions/release", api.WorkerReleaseRequest{Lease: lease, Result: result}, &response); err != nil {
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
	if err := c.postWorkerJSON(ctx, "/api/worker/executions/logs", api.WorkerAppendLogRequest{
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
	if err := c.postWorkerJSON(ctx, "/api/worker/executions/log-entries", api.WorkerRecordLogEntryRequest{
		Lease: lease,
		Entry: entry,
	}, &response); err != nil {
		return api.WorkerEventResponse{}, err
	}
	return response, nil
}

func (c *Client) EmitEvent(ctx context.Context, lease api.WorkerRunLease, eventType string, content json.RawMessage) (api.WorkerEventResponse, error) {
	var response api.WorkerEventResponse
	if len(content) == 0 {
		content = json.RawMessage(`null`)
	}
	if err := c.postWorkerJSON(ctx, "/api/worker/executions/events", api.WorkerEmitEventRequest{
		Lease:     lease,
		EventType: eventType,
		Content:   content,
	}, &response); err != nil {
		return api.WorkerEventResponse{}, err
	}
	return response, nil
}

func (c *Client) CreateWaitpoint(ctx context.Context, request api.WorkerCreateWaitpointRequest) (api.WorkerCreateWaitpointResponse, error) {
	var response api.WorkerCreateWaitpointResponse
	if err := c.postWorkerJSON(ctx, "/api/worker/executions/waitpoints", request, &response); err != nil {
		return api.WorkerCreateWaitpointResponse{}, err
	}
	return response, nil
}

func (c *Client) MarkCheckpointReady(ctx context.Context, request api.WorkerCheckpointReadyRequest) (api.WorkerCreateWaitpointResponse, error) {
	var response api.WorkerCreateWaitpointResponse
	if err := c.postWorkerJSON(ctx, "/api/worker/executions/checkpoints/ready", request, &response); err != nil {
		return api.WorkerCreateWaitpointResponse{}, err
	}
	return response, nil
}

func (c *Client) MarkCheckpointFailed(ctx context.Context, request api.WorkerCheckpointFailedRequest) (api.WorkerCreateWaitpointResponse, error) {
	var response api.WorkerCreateWaitpointResponse
	if err := c.postWorkerJSON(ctx, "/api/worker/executions/checkpoints/failed", request, &response); err != nil {
		return api.WorkerCreateWaitpointResponse{}, err
	}
	return response, nil
}
