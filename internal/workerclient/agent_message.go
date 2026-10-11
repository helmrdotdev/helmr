package workerclient

import (
	"context"
	"github.com/helmrdotdev/helmr/internal/httpclient"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"net/http"
)

func (client *Client) NextAgentMessage(ctx context.Context, request workerapi.AgentMessageRequest) (*workerapi.AgentMessageDispatch, error) {
	var response *workerapi.AgentMessageDispatch
	err := client.postWorkerJSON(ctx, "/worker/v1/sessions/message", request, &response)
	if httpclient.IsStatus(err, http.StatusConflict) || httpclient.IsStatus(err, http.StatusBadRequest) {
		err = SessionAuthorityRejectedError{Err: err}
	}
	return response, err
}
func (client *Client) ObserveAgentMessage(ctx context.Context, request workerapi.AgentMessageReceipt) error {
	return client.postWorkerJSON(ctx, "/worker/v1/sessions/message/receipt", request, nil)
}
