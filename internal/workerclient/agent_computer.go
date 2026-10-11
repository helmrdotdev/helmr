package workerclient

import (
	"context"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func (client *Client) BeginAgentComputerCapture(ctx context.Context, request workerapi.AgentComputerCaptureRequest) (workerapi.AgentComputerCaptureResponse, error) {
	var response workerapi.AgentComputerCaptureResponse
	err := client.postWorkerJSON(ctx, "/worker/v1/agent-computers/capture/begin", request, &response)
	return response, err
}

func (client *Client) SealAgentComputerCapture(ctx context.Context, request workerapi.AgentComputerReceiptRequest) error {
	return client.postWorkerJSON(ctx, "/worker/v1/agent-computers/capture/seal", request, nil)
}

func (client *Client) CancelAgentComputerCapture(ctx context.Context, request workerapi.AgentComputerUnsealedRequest) error {
	return client.postWorkerJSON(ctx, "/worker/v1/agent-computers/capture/cancel", request, nil)
}

func (client *Client) AgentComputerSaveAbsence(ctx context.Context, request workerapi.AgentComputerSaveAbsenceRequest) error {
	return client.postWorkerJSON(ctx, "/worker/v1/agent-computers/capture/save-absence", request, nil)
}

func (client *Client) PrepareAgentComputerRestore(ctx context.Context, request workerapi.AgentComputerRestoreRequest) (workerapi.AgentComputerInstallationResponse, error) {
	var response workerapi.AgentComputerInstallationResponse
	err := client.postWorkerJSON(ctx, "/worker/v1/agent-computers/restore/prepare", request, &response)
	return response, err
}

func (client *Client) ValidateAgentComputerSourceAbort(ctx context.Context, request workerapi.AgentComputerInstallationRequest) error {
	return client.postWorkerJSON(ctx, "/worker/v1/agent-computers/source-abort/validate", request, nil)
}

func (client *Client) CommitAgentComputerSourceAbort(ctx context.Context, request workerapi.AgentComputerInstallationRequest) error {
	return client.postWorkerJSON(ctx, "/worker/v1/agent-computers/source-abort/commit", request, nil)
}

func (client *Client) CompleteAgentComputerSourceAbort(ctx context.Context, request workerapi.AgentComputerInstallationRequest) error {
	return client.postWorkerJSON(ctx, "/worker/v1/agent-computers/source-abort/complete", request, nil)
}

func (client *Client) ValidateAgentComputerRestore(ctx context.Context, request workerapi.AgentComputerInstallationRequest) error {
	return client.postWorkerJSON(ctx, "/worker/v1/agent-computers/restore/validate", request, nil)
}

func (client *Client) CommitAgentComputerRestore(ctx context.Context, request workerapi.AgentComputerInstallationRequest) error {
	return client.postWorkerJSON(ctx, "/worker/v1/agent-computers/restore/commit", request, nil)
}

func (client *Client) CompleteAgentComputerRestore(ctx context.Context, request workerapi.AgentComputerInstallationRequest) error {
	return client.postWorkerJSON(ctx, "/worker/v1/agent-computers/restore/complete", request, nil)
}

func (client *Client) ReadAgentComputerControls(ctx context.Context, request workerapi.AgentComputerInstallationRequest) (workerapi.AgentComputerControlsResponse, error) {
	var response workerapi.AgentComputerControlsResponse
	err := client.postWorkerJSON(ctx, "/worker/v1/agent-computers/controls", request, &response)
	return response, err
}

func (client *Client) PrepareAgentComputerSourceAbort(ctx context.Context, request workerapi.AgentComputerSourceAbortRequest) (workerapi.AgentComputerInstallationResponse, error) {
	var response workerapi.AgentComputerInstallationResponse
	err := client.postWorkerJSON(ctx, "/worker/v1/agent-computers/source-abort/prepare", request, &response)
	return response, err
}

func (client *Client) RenewAgentComputerLease(ctx context.Context, request workerapi.AgentComputerLeaseRequest) (workerapi.AgentComputerLeaseResponse, error) {
	var response workerapi.AgentComputerLeaseResponse
	err := client.postWorkerJSON(ctx, "/worker/v1/agent-computers/lease/renew", request, &response)
	return response, err
}
func (client *Client) ObserveAgentComputerStopped(ctx context.Context, request workerapi.AgentComputerStoppedRequest) error {
	return client.postWorkerJSON(ctx, "/worker/v1/agent-computers/stopped", request, nil)
}
