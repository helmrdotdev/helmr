package workerclient

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/helmrdotdev/helmr/internal/httpclient"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func (c *Client) EnrollWorker(ctx context.Context, token string, request workerapi.EnrollmentRequest) (workerapi.EnrollmentResponse, error) {
	request.APIVersion = workerapi.APIVersion
	var response workerapi.EnrollmentResponse
	if err := c.postJSON(ctx, "/worker/v1/enrollment", token, request, &response); err != nil {
		return workerapi.EnrollmentResponse{}, err
	}
	return response, nil
}

func (c *Client) ClaimComputerCommand(ctx context.Context, request workerapi.ComputerCommandClaimRequest) (workerapi.ComputerCommandClaimResponse, error) {
	var response workerapi.ComputerCommandClaimResponse
	if err := c.postWorkerJSON(ctx, "/worker/v1/computer-commands/claim", request, &response); err != nil {
		return workerapi.ComputerCommandClaimResponse{}, err
	}
	return response, nil
}

func (c *Client) CompleteComputerCommand(ctx context.Context, request workerapi.ComputerCommandCompleteRequest) error {
	return c.postWorkerJSON(ctx, "/worker/v1/computer-commands/complete", request, nil)
}

func (c *Client) AppendCommandLog(ctx context.Context, request workerapi.CommandLogAppendRequest) (workerapi.DiagnosticLogReceipt, error) {
	var receipt workerapi.DiagnosticLogReceipt
	err := c.postWorkerJSON(ctx, "/worker/v1/computer-commands/logs/append", request, &receipt)
	return receipt, err
}

func (c *Client) ActivateWorker(ctx context.Context, capabilities workerapi.Capabilities) (workerapi.StatusResponse, error) {
	var response workerapi.StatusResponse
	if err := c.postWorkerJSON(ctx, "/worker/v1/instance/activate", workerapi.ActivateRequest{APIVersion: workerapi.APIVersion, Capabilities: capabilities}, &response); err != nil {
		return workerapi.StatusResponse{}, err
	}
	return response, nil
}

func (c *Client) ReportWorkerStartupRecovery(ctx context.Context, request workerapi.StartupRecoveryRequest) error {
	return c.postWorkerJSON(ctx, "/worker/v1/instance/recover", request, nil)
}

func (c *Client) ObserveWorker(ctx context.Context, observation workerapi.Observation) (workerapi.StatusResponse, error) {
	var response workerapi.StatusResponse
	if err := c.postWorkerJSON(ctx, "/worker/v1/instance/observations", workerapi.ObserveRequest{Observation: observation}, &response); err != nil {
		return workerapi.StatusResponse{}, err
	}
	return response, nil
}

func (c *Client) DrainWorker(ctx context.Context) (workerapi.StatusResponse, error) {
	var response workerapi.StatusResponse
	if err := c.postWorkerJSON(ctx, "/worker/v1/instance/drain", struct{}{}, &response); err != nil {
		return workerapi.StatusResponse{}, err
	}
	return response, nil
}

func (c *Client) CompleteWorkerDrain(ctx context.Context) (workerapi.StatusResponse, error) {
	const attempts = 3
	var lastErr error
	for attempt := range attempts {
		var response workerapi.StatusResponse
		lastErr = c.postWorkerJSON(ctx, "/worker/v1/instance/drain/complete", nil, &response)
		if lastErr == nil {
			return response, nil
		}
		if !ambiguousWorkerTerminalMutation(lastErr) || attempt == attempts-1 {
			break
		}
		delay := time.Duration(attempt+1) * 100 * time.Millisecond
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return workerapi.StatusResponse{}, ctx.Err()
		case <-timer.C:
		}
	}
	return workerapi.StatusResponse{}, fmt.Errorf("worker drain completion was not confirmed after %d identical attempts: %w", attempts, lastErr)
}

func ambiguousWorkerTerminalMutation(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var httpErr *httpclient.Error
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
	const attempts = 3
	var lastErr error
	request := workerapi.FenceRequest{ReasonCode: reasonCode}
	for attempt := range attempts {
		lastErr = c.postWorkerJSON(ctx, "/worker/v1/instance/fence", request, nil)
		if lastErr == nil {
			return nil
		}
		if !ambiguousWorkerTerminalMutation(lastErr) || attempt == attempts-1 {
			break
		}
		delay := time.Duration(attempt+1) * 100 * time.Millisecond
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return fmt.Errorf("worker fence was not confirmed after %d identical attempts: %w", attempts, lastErr)
}

func (c *Client) GetWorkerStatus(ctx context.Context) (workerapi.StatusResponse, error) {
	var response workerapi.StatusResponse
	if err := c.getWorkerJSON(ctx, "/worker/v1/instance", &response); err != nil {
		return workerapi.StatusResponse{}, err
	}
	return response, nil
}

func (c *Client) ReconcileComputerCommand(ctx context.Context, r workerapi.ComputerCommandCompleteRequest) error {
	return c.postWorkerJSON(ctx, "/worker/v1/computer-commands/reconcile", r, nil)
}
