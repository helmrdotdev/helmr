package workerclient

import (
	"context"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func (client *Client) AppendPreparationLog(ctx context.Context, request workerapi.PreparationLogRequest) (workerapi.DiagnosticLogReceipt, error) {
	var receipt workerapi.DiagnosticLogReceipt
	err := client.postWorkerJSONStatus(ctx, "/worker/v1/allocations/preparation/logs", request, &receipt, true)
	return receipt, err
}
