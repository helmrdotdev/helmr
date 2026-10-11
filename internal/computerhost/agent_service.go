package computerhost

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/helmrdotdev/helmr/internal/httpclient"
	agentv1 "github.com/helmrdotdev/helmr/internal/proto/agent/v1"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"google.golang.org/protobuf/proto"
)

var (
	errDiagnosticExpired  = errors.New("retained diagnostic head expired")
	errDiagnosticRejected = errors.New("diagnostic stream rejected")
)

// AgentRuntimeClient is the authenticated worker-to-Control Plane API. The
// workerclient implementation renews its own HTTP credential independently.
type AgentRuntimeClient interface {
	RenewAgentAuthority(context.Context, workerapi.RuntimeSession) (workerapi.AgentAuthorityResponse, error)
	AgentOperation(context.Context, workerapi.AgentOperationRequest) (workerapi.AgentOperationResponse, error)
}

func (connection *AgentSessionConnection) runtimeSession(environmentID string, leaseEpoch int64) workerapi.RuntimeSession {
	return workerapi.RuntimeSession{EnvironmentID: environmentID, SessionID: connection.identity.SessionId, ProcessEpoch: connection.identity.ProcessEpoch, ComputerLeaseEpoch: leaseEpoch}
}
func (connection *AgentSessionConnection) forwardOperation(ctx context.Context, environmentID string, client AgentRuntimeClient, event *agentv1.GuestSessionMessage) error {
	operation := event.GetEvent().GetOperation()
	request := workerapi.AgentOperationRequest{Session: connection.runtimeSession(environmentID, event.GetOperationLeaseEpoch()), RequestID: operation.GetRequestId(), AuthorityGeneration: event.GetOperationAuthorityGeneration(), TurnID: operation.GetTurnId(), Method: int32(operation.GetMethod()), Payload: append(json.RawMessage(nil), operation.GetPayloadJson()...), DrainEvidence: event.GetDrainEvidence()}
	var response workerapi.AgentOperationResponse
	if len(request.RequestID) > 512 || !json.Valid(request.Payload) {
		// Invalid authored bytes cannot enter HTTP JSON encoding. Settle this exact
		// retained request instead of treating deterministic rejection as uncertainty.
		response = workerapi.AgentOperationResponse{RequestID: request.RequestID, Error: &workerapi.AgentOperationError{Code: "invalid_arguments", Message: "Operation ID or JSON payload is invalid"}}
	} else {
		var err error
		response, err = client.AgentOperation(ctx, request)
		if err != nil {
			var response *httpclient.Error
			status := 0
			if errors.As(err, &response) {
				status = response.StatusCode
			}
			slog.Info("Session operation awaiting retry", "session_id", request.Session.SessionID, "turn_id", request.TurnID,
				"request_id", request.RequestID, "method", operation.GetMethod().String(), "http_status", status, "error", err)
			return err
		}
	}
	if response.RequestID != request.RequestID || (response.Error == nil) == (len(response.Value) == 0) || (response.Error == nil && !json.Valid(response.Value)) {
		return errors.New("invalid Control Plane Session operation receipt")
	}
	result := &agentv1.OperationResult{RequestId: request.RequestID}
	if response.Error != nil {
		result.Outcome = &agentv1.OperationResult_Error{Error: &agentv1.OperationError{Code: response.Error.Code, Message: response.Error.Message}}
	} else {
		result.Outcome = &agentv1.OperationResult_ValueJson{ValueJson: response.Value}
	}
	if err := connection.SendCommand(ctx, &agentv1.GuestCommand{Identity: connection.identity, Command: &agentv1.GuestCommand_OperationResult{OperationResult: result}}); err != nil {
		return err
	}
	return connection.Acknowledge(ctx, event.GetEventSequence())
}

// ServeAgentSession runs one replaceable attachment. Upstream uncertainty closes
// only this transport; the caller reacquires a CP attachment sequence and attaches
// to the same process. A replay keeps original payload, generation and retry key.
// observe must durably handle non-operation events before returning successfully.
// It may run concurrently for each diagnostic stream and lifecycle events. A log
// temporary error retries without blocking control. Expiry releases the exact
// retained head; a definite rejection stops that stream without claiming EOF.
func ServeAgentSession(ctx context.Context, connection *AgentSessionConnection, environmentID string, client AgentRuntimeClient, observe func(context.Context, *agentv1.GuestSessionMessage) error) error {
	return serveAgentSession(ctx, connection, environmentID, client, observe, nil)
}

func serveAgentSession(ctx context.Context, connection *AgentSessionConnection, environmentID string, client AgentRuntimeClient, observe func(context.Context, *agentv1.GuestSessionMessage) error, activated <-chan struct{}) error {
	if connection == nil || environmentID == "" || client == nil || observe == nil {
		return errors.New("session service requires its owner, Control Plane and event observer")
	}
	ctx, cancel := context.WithCancelCause(ctx)
	var jobs sync.WaitGroup
	// Cancellation must close a blocked reader/writer before joining API work.
	defer func() { cancel(nil); _ = connection.Close(); jobs.Wait() }()
	var mu sync.Mutex
	pending := make(map[string]*agentv1.GuestSessionMessage)
	capacity := make(chan struct{}, 64)
	var logPending [2]bool
	// Local drain completion includes a definitively rejected stream. It never
	// creates a durable EOF or a claim of complete output.
	var logSettled [2]bool
	logsDone := make(chan struct{})
	var logsDoneOnce sync.Once
	stopped := false
	jobs.Add(1)
	go func() {
		defer jobs.Done()
		err := connection.MaintainAuthority(ctx, func(ctx context.Context, current *agentv1.SessionGrant) (*agentv1.SessionGrant, error) {
			result, err := client.RenewAgentAuthority(ctx, connection.runtimeSession(environmentID, current.GetComputerLeaseEpoch()))
			if err != nil {
				return nil, err
			}
			next := proto.Clone(current).(*agentv1.SessionGrant)
			next.AuthorityGeneration = result.AuthorityGeneration
			next.ExpiresAtUnixNano = result.ExpiresAt.UnixNano()
			return next, nil
		})
		if err != nil {
			cancel(err)
		}
	}()
	// A restored guest retains its outbox while frozen. Keep authority fresh,
	// but do not consume events or send operation results before activation.
	if activated != nil {
		select {
		case <-activated:
		case <-ctx.Done():
			return context.Cause(ctx)
		}
	}
	for {
		event, err := connection.Receive(ctx)
		if err != nil {
			if stopped {
				return nil
			} // Physical outcome already committed; missing EOF is an unknown tail.
			if context.Cause(ctx) != nil {
				return context.Cause(ctx)
			}
			return err
		}
		if log := event.GetLog(); log != nil {
			index := int(log.GetStream()) - 1 // Receive validated the stream and envelope.
			mu.Lock()
			busy := logPending[index]
			logPending[index] = true
			mu.Unlock()
			if busy {
				return errors.New("session sent another log before durable acceptance")
			}
			jobs.Add(1)
			go func() {
				defer jobs.Done()
				for {
					err := observe(ctx, event)
					if errors.Is(err, errDiagnosticRejected) {
						slog.Warn("Session diagnostic stream rejected", "session_id", connection.identity.GetSessionId(), "stream", log.GetStream().String())
						mu.Lock()
						logSettled[index] = true
						if logSettled[0] && logSettled[1] {
							logsDoneOnce.Do(func() { close(logsDone) })
						}
						mu.Unlock()
						return
					}
					if err == nil || errors.Is(err, errDiagnosticExpired) {
						// Release before sending: the next frame can arrive immediately
						// after the guest reads this receipt.
						mu.Lock()
						logPending[index] = false
						mu.Unlock()
						if err := connection.acknowledgeLog(ctx, log.GetStream(), log.GetThroughSequence(), errors.Is(err, errDiagnosticExpired)); err != nil {
							cancel(err)
						}
						if log.GetKind() == agentv1.SessionLog_KIND_END {
							mu.Lock()
							logSettled[index] = true
							if logSettled[0] && logSettled[1] {
								logsDoneOnce.Do(func() { close(logsDone) })
							}
							mu.Unlock()
						}
						return
					}
					timer := time.NewTimer(time.Second)
					select {
					case <-ctx.Done():
						timer.Stop()
						return
					case <-timer.C:
					}
				}
			}()
			continue
		}
		if operation := event.GetEvent().GetOperation(); operation != nil {
			mu.Lock()
			original := pending[operation.GetRequestId()]
			if original == nil {
				pending[operation.GetRequestId()] = event
			}
			mu.Unlock()
			if original != nil {
				if !proto.Equal(original.GetEvent(), event.GetEvent()) || original.GetOperationAuthorityGeneration() != event.GetOperationAuthorityGeneration() || original.GetOperationLeaseEpoch() != event.GetOperationLeaseEpoch() {
					return errors.New("pending Session request identity changed")
				}
				if err := connection.Acknowledge(ctx, event.GetEventSequence()); err != nil {
					return err
				}
				continue
			}
			select {
			case capacity <- struct{}{}:
			case <-ctx.Done():
				return context.Cause(ctx)
			}
			jobs.Add(1)
			go func() {
				defer jobs.Done()
				defer func() { <-capacity }()
				err := connection.forwardOperation(ctx, environmentID, client, event)
				mu.Lock()
				delete(pending, operation.GetRequestId())
				mu.Unlock()
				if err != nil {
					cancel(err)
				}
			}()
			continue
		}
		if event.GetEvent().GetCancelOperation() != nil {
			// The original mutation still resolves and returns its receipt. Cancellation
			// of bounded observations is handled by the CP operation's own time limit.
			if err := connection.Acknowledge(ctx, event.GetEventSequence()); err != nil {
				return err
			}
			continue
		}
		if err := observe(ctx, event); err != nil {
			return err
		}
		if err := connection.Acknowledge(ctx, event.GetEventSequence()); err != nil {
			return err
		}
		if event.GetStopped() != nil {
			stopped = true
			// Stop is durable and acknowledged before tail delivery. Reuse the
			// current grant's finite lifetime; a log outage cannot extend it.
			deadline := time.Unix(0, connection.CurrentGrant().GetExpiresAtUnixNano())
			jobs.Add(1)
			go func() {
				defer jobs.Done()
				timer := time.NewTimer(max(0, time.Until(deadline)))
				defer timer.Stop()
				select {
				case <-ctx.Done():
					return
				case <-logsDone:
				case <-timer.C:
				}
				cancel(context.Canceled)
			}()
		}
	}
}
