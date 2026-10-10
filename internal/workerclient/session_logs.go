package workerclient

import (
	"context"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func (client *Client) AppendSessionLog(ctx context.Context, request workerapi.SessionLogRequest) (workerapi.DiagnosticLogReceipt, error) {
	var receipt workerapi.DiagnosticLogReceipt
	err := client.postWorkerJSONStatus(ctx, "/worker/v1/sessions/logs", request, &receipt, true)
	return receipt, err
}
