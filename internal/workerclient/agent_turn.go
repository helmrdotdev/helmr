package workerclient

import (
	"context"
	"net/http"

	"github.com/helmrdotdev/helmr/internal/httpclient"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func (client *Client) NextAgentTurn(ctx context.Context, request workerapi.AgentTurnRequest) (*workerapi.AgentTurnDispatch, error) {
	var response *workerapi.AgentTurnDispatch
	err := client.postWorkerJSON(ctx, "/worker/v1/sessions/turn", request, &response)
	if httpclient.IsStatus(err, http.StatusConflict) || httpclient.IsStatus(err, http.StatusBadRequest) {
		err = SessionAuthorityRejectedError{Err: err}
	}
	return response, err
}

func (client *Client) ObserveAgentTurn(ctx context.Context, request workerapi.AgentTurnReceipt) (workerapi.AgentTurnAcknowledgment, error) {
	var response workerapi.AgentTurnAcknowledgment
	err := client.postWorkerJSON(ctx, "/worker/v1/sessions/turn/receipt", request, &response)
	return response, err
}
