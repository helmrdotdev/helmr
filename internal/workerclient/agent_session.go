package workerclient

import (
	"context"
	"github.com/helmrdotdev/helmr/internal/httpclient"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"net/http"
)

func (client *Client) RenewAgentAuthority(ctx context.Context, session workerapi.RuntimeSession) (workerapi.AgentAuthorityResponse, error) {
	var response workerapi.AgentAuthorityResponse
	err := client.postWorkerJSON(ctx, "/worker/v1/sessions/authority", session, &response)
	if httpclient.IsStatus(err, http.StatusConflict) {
		err = SessionAuthorityRejectedError{Err: err}
	}
	return response, err
}
func (client *Client) AcquireAgentAttachment(ctx context.Context, session workerapi.RuntimeSession) (workerapi.AgentAttachmentResponse, error) {
	var response workerapi.AgentAttachmentResponse
	err := client.postWorkerJSON(ctx, "/worker/v1/sessions/attachment", session, &response)
	if httpclient.IsStatus(err, http.StatusConflict) {
		err = SessionAuthorityRejectedError{Err: err}
	}
	return response, err
}

func (client *Client) AgentOperation(ctx context.Context, request workerapi.AgentOperationRequest) (workerapi.AgentOperationResponse, error) {
	var response workerapi.AgentOperationResponse
	err := client.postWorkerJSON(ctx, "/worker/v1/sessions/operations", request, &response)
	return response, err
}

func (client *Client) PrepareAgentControl(ctx context.Context, request workerapi.AgentControlRequest) (workerapi.AgentControlResponse, error) {
	var response workerapi.AgentControlResponse
	err := client.postWorkerJSON(ctx, "/worker/v1/sessions/control", request, &response)
	if httpclient.IsStatus(err, http.StatusConflict) || httpclient.IsStatus(err, http.StatusBadRequest) {
		err = SessionAuthorityRejectedError{Err: err}
	}
	return response, err
}
func (client *Client) AcknowledgeAgentControl(ctx context.Context, request workerapi.AgentControlReceipt) error {
	return client.postWorkerJSON(ctx, "/worker/v1/sessions/control/receipt", request, nil)
}

func (client *Client) ObserveAgentStopped(ctx context.Context, request workerapi.AgentControlRequest) error {
	return client.postWorkerJSON(ctx, "/worker/v1/sessions/stopped", request, nil)
}

func (client *Client) ObserveAgentReady(ctx context.Context, request workerapi.AgentControlRequest) error {
	return client.postWorkerJSON(ctx, "/worker/v1/sessions/ready", request, nil)
}

// SessionAuthorityRejectedError signals that this physical execution no longer
// receives control grants. The host owner must reconcile rather than retry forever.
type SessionAuthorityRejectedError struct{ Err error }

func (e SessionAuthorityRejectedError) Error() string                  { return e.Err.Error() }
func (e SessionAuthorityRejectedError) Unwrap() error                  { return e.Err }
func (e SessionAuthorityRejectedError) SessionAuthorityRejected() bool { return true }

func (client *Client) ObserveAgentFailure(ctx context.Context, request workerapi.AgentControlRequest) error {
	return client.postWorkerJSON(ctx, "/worker/v1/sessions/failed", request, nil)
}

func (client *Client) AuthorizeAgentStart(ctx context.Context, request workerapi.AgentControlRequest) (workerapi.AgentStartResponse, error) {
	var response workerapi.AgentStartResponse
	err := client.postWorkerJSON(ctx, "/worker/v1/sessions/start", request, &response)
	if httpclient.IsStatus(err, http.StatusConflict) || httpclient.IsStatus(err, http.StatusBadRequest) {
		err = SessionAuthorityRejectedError{Err: err}
	}
	return response, err
}

func (client *Client) ReleaseAgentStart(ctx context.Context, request workerapi.AgentStartReleaseRequest) (workerapi.AgentAuthorityResponse, error) {
	var response workerapi.AgentAuthorityResponse
	err := client.postWorkerJSON(ctx, "/worker/v1/sessions/start/release", request, &response)
	if httpclient.IsStatus(err, http.StatusConflict) || httpclient.IsStatus(err, http.StatusBadRequest) {
		err = SessionAuthorityRejectedError{Err: err}
	}
	return response, err
}
