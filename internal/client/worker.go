package client

import (
	"context"
	"encoding/base64"
	"encoding/json"

	"github.com/helmrdotdev/helmr/internal/api"
)

func (c *Client) RegisterWorker(ctx context.Context, registrationToken string, resourceName string) (api.WorkerRegisterResponse, error) {
	var response api.WorkerRegisterResponse
	if err := c.postJSON(ctx, "/api/worker/register", api.WorkerRegisterRequest{
		RegistrationToken: registrationToken,
		ResourceName:      resourceName,
	}, &response); err != nil {
		return api.WorkerRegisterResponse{}, err
	}
	return response, nil
}

func (c *Client) ClaimRun(ctx context.Context, capabilities api.WorkerCapabilities) (api.WorkerClaimResponse, error) {
	var response api.WorkerClaimResponse
	if err := c.postWorkerJSON(ctx, "/api/worker/executions/claim", api.WorkerClaimRequest{Capabilities: capabilities}, &response); err != nil {
		return api.WorkerClaimResponse{}, err
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

func (c *Client) StartRun(ctx context.Context, claim api.WorkerClaim) (api.WorkerStartResponse, error) {
	var response api.WorkerStartResponse
	if err := c.postWorkerJSON(ctx, "/api/worker/executions/start", api.WorkerStartRequest{Claim: claim}, &response); err != nil {
		return api.WorkerStartResponse{}, err
	}
	return response, nil
}

func (c *Client) RenewRun(ctx context.Context, claim api.WorkerClaim) (api.WorkerRenewResponse, error) {
	var response api.WorkerRenewResponse
	if err := c.postWorkerJSON(ctx, "/api/worker/executions/renew", api.WorkerRenewRequest{Claim: claim}, &response); err != nil {
		return api.WorkerRenewResponse{}, err
	}
	return response, nil
}

func (c *Client) ReleaseRun(ctx context.Context, claim api.WorkerClaim, result api.WorkerReleaseResult) (api.WorkerReleaseResponse, error) {
	var response api.WorkerReleaseResponse
	if err := c.postWorkerJSON(ctx, "/api/worker/executions/release", api.WorkerReleaseRequest{Claim: claim, Result: result}, &response); err != nil {
		return api.WorkerReleaseResponse{}, err
	}
	return response, nil
}

func (c *Client) AppendLog(ctx context.Context, claim api.WorkerClaim, stream api.WorkerLogStream, observedSeq uint64, content []byte) (api.WorkerEventResponse, error) {
	var response api.WorkerEventResponse
	if err := c.postWorkerJSON(ctx, "/api/worker/executions/logs", api.WorkerAppendLogRequest{
		Claim:         claim,
		Stream:        stream,
		ObservedSeq:   observedSeq,
		ContentBase64: base64.StdEncoding.EncodeToString(content),
	}, &response); err != nil {
		return api.WorkerEventResponse{}, err
	}
	return response, nil
}

func (c *Client) RecordLogEntry(ctx context.Context, claim api.WorkerClaim, entry string) (api.WorkerEventResponse, error) {
	var response api.WorkerEventResponse
	if err := c.postWorkerJSON(ctx, "/api/worker/executions/log-entries", api.WorkerRecordLogEntryRequest{
		Claim: claim,
		Entry: entry,
	}, &response); err != nil {
		return api.WorkerEventResponse{}, err
	}
	return response, nil
}

func (c *Client) EmitEvent(ctx context.Context, claim api.WorkerClaim, eventType string, content json.RawMessage) (api.WorkerEventResponse, error) {
	var response api.WorkerEventResponse
	if len(content) == 0 {
		content = json.RawMessage(`null`)
	}
	if err := c.postWorkerJSON(ctx, "/api/worker/executions/events", api.WorkerEmitEventRequest{
		Claim:     claim,
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

func (c *Client) GetWaitpointDecision(ctx context.Context, request api.WorkerWaitpointDecisionRequest) (api.WorkerWaitpointDecisionResponse, error) {
	var response api.WorkerWaitpointDecisionResponse
	if err := c.postWorkerJSON(ctx, "/api/worker/executions/waitpoints/decision", request, &response); err != nil {
		return api.WorkerWaitpointDecisionResponse{}, err
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
