package computerhost

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/helmrdotdev/helmr/internal/ids"
	agentv1 "github.com/helmrdotdev/helmr/internal/proto/agent/v1"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

type AgentTurnClient interface {
	NextAgentTurn(context.Context, workerapi.AgentTurnRequest) (*workerapi.AgentTurnDispatch, error)
	ObserveAgentTurn(context.Context, workerapi.AgentTurnReceipt) (workerapi.AgentTurnAcknowledgment, error)
}

type AgentExecutionClient interface {
	AgentControlClient
	AgentTurnClient
	AgentMessageClient
}

type agentSessionTurns struct {
	mu             sync.Mutex
	connection     *AgentSessionConnection
	client         AgentExecutionClient
	environment    string
	pendingMessage string
	pending        string
	terminal       bool
}

func (owner *agentSessionTurns) dispatch(ctx context.Context) error {
	owner.mu.Lock()
	defer owner.mu.Unlock()
	if owner.pending != "" {
		return owner.dispatchMessageLocked(ctx)
	}
	grant := owner.connection.CurrentGrant()
	request := workerapi.AgentTurnRequest{Session: owner.connection.runtimeSession(owner.environment, grant.GetComputerLeaseEpoch()), AttachmentSequence: int64(owner.connection.attachment), AuthorityGeneration: grant.GetAuthorityGeneration()}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	next, err := owner.client.NextAgentTurn(ctx, request)
	var rejected interface{ SessionAuthorityRejected() bool }
	if errors.As(err, &rejected) && rejected.SessionAuthorityRejected() {
		// Controls own authority convergence and physical stop observation. A
		// generation race or stopped process must not cut off their retained events.
		return nil
	}
	if err != nil || next == nil {
		return err
	}
	if ids.Validate(next.TurnID) != nil || next.Sequence <= 0 || next.CreatedAt.IsZero() || !json.Valid(next.Input) {
		return errors.New("invalid Turn assignment")
	}
	source, err := json.Marshal(next.Source)
	if err != nil {
		return err
	}
	owner.pending = next.TurnID
	return owner.connection.SendCommand(ctx, &agentv1.GuestCommand{Identity: owner.connection.identity, DeliveryId: "turn:" + next.TurnID, Command: &agentv1.GuestCommand_Dispatch{Dispatch: &agentv1.TurnDispatch{TurnId: next.TurnID, Sequence: next.Sequence, CreatedAt: next.CreatedAt.Format(time.RFC3339Nano), InputJson: next.Input, SourceJson: source}}})
}

func (owner *agentSessionTurns) observe(ctx context.Context, message *agentv1.GuestSessionMessage) (bool, error) {
	if failure := message.GetEvent().GetFailed(); failure != nil {
		return true, owner.fail(ctx, message, "Session runtime failed")
	}
	result := message.GetEvent().GetDeliveryResult()
	if result == nil {
		return false, nil
	}
	id := result.GetDeliveryId()
	if strings.HasPrefix(id, "message:") {
		return true, owner.observeMessage(ctx, result)
	}
	if strings.HasPrefix(id, "turn-ack:") {
		turn := strings.TrimPrefix(id, "turn-ack:")
		if result.GetError() != nil {
			return true, owner.fail(ctx, message, "Turn retirement failed")
		}
		if ids.Validate(turn) != nil || !bytes.Equal(bytes.TrimSpace(result.GetValueJson()), []byte("null")) {
			return true, errors.New("invalid Turn retirement receipt")
		}
		owner.mu.Lock()
		if owner.pending == turn {
			owner.pending = ""
		}
		owner.mu.Unlock()
		return true, nil
	}
	if !strings.HasPrefix(id, "turn:") {
		return false, nil
	}
	turn := strings.TrimPrefix(id, "turn:")
	if result.GetError() != nil {
		return true, owner.fail(ctx, message, "Turn execution failed")
	}
	if ids.Validate(turn) != nil || !json.Valid(result.GetValueJson()) {
		return true, errors.New("invalid Turn terminal receipt")
	}
	grant := owner.connection.CurrentGrant()
	receipt, err := owner.client.ObserveAgentTurn(ctx, workerapi.AgentTurnReceipt{Session: owner.connection.runtimeSession(owner.environment, grant.GetComputerLeaseEpoch()), AttachmentSequence: int64(owner.connection.attachment), TurnID: turn, Outcome: result.GetValueJson()})
	if err != nil {
		return true, err
	}
	if receipt.Sequence <= 0 {
		return true, errors.New("invalid Turn acknowledgment sequence")
	}
	if owner.terminal {
		// A closed runtime cannot retire local state. Its retained result still
		// needs durable verification before draining the later physical-stop event.
		return true, nil
	}
	// Send the retirement command before ServeAgentSession acknowledges the
	// retained DeliveryResult. A lost pipe replays that result and this stable ID.
	return true, owner.connection.SendCommand(ctx, &agentv1.GuestCommand{Identity: owner.connection.identity, DeliveryId: "turn-ack:" + turn, Command: &agentv1.GuestCommand_Acknowledged{Acknowledged: &agentv1.TurnAcknowledged{TurnId: turn, Sequence: receipt.Sequence}}})
}

func (owner *agentSessionTurns) fail(ctx context.Context, message *agentv1.GuestSessionMessage, reason string) error {
	// Keep the runtime's reason in the existing internal failure observation.
	// As with control failures, an oversized message must not expand host logs.
	detail := message.GetEvent().GetFailed().GetMessage()
	if result := message.GetEvent().GetDeliveryResult(); result != nil {
		detail = result.GetError().GetMessage()
	}
	if detail != "" && len(detail) <= 4096 {
		reason += ": " + detail
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	grant := owner.connection.CurrentGrant()
	request := workerapi.AgentControlRequest{Session: owner.connection.runtimeSession(owner.environment, grant.GetComputerLeaseEpoch()), AttachmentSequence: int64(owner.connection.attachment)}
	err := owner.client.ObserveAgentFailure(ctx, request)
	failure := SessionControlFailedError{Session: request.Session, AttachmentSequence: request.AttachmentSequence, Kind: "execution", Message: reason, FailureError: err}
	if err == nil {
		return errors.Join(failure, owner.connection.Acknowledge(ctx, message.GetEventSequence()))
	}
	return failure
}

func ServeExecutingAgentSession(ctx context.Context, connection *AgentSessionConnection, attached *agentv1.SessionAttached, environment string, client AgentExecutionClient, observe func(context.Context, *agentv1.GuestSessionMessage) error) error {
	return serveExecutingAgentSession(ctx, connection, attached, environment, client, observe, nil)
}

func serveExecutingAgentSession(ctx context.Context, connection *AgentSessionConnection, attached *agentv1.SessionAttached, environment string, client AgentExecutionClient, observe func(context.Context, *agentv1.GuestSessionMessage) error, activated <-chan struct{}) error {
	if connection == nil || attached == nil || client == nil || observe == nil || environment == "" {
		return errors.New("executing Session requires attachment, client and observer")
	}
	owner := &agentSessionTurns{connection: connection, client: client, environment: environment, pending: attached.GetPendingTurnId(), terminal: attached.GetTerminal()}
	ctx, cancel := context.WithCancelCause(ctx)
	var jobs sync.WaitGroup
	defer func() { cancel(nil); _ = connection.Close(); jobs.Wait() }()
	if !attached.GetTerminal() {
		jobs.Go(func() {
			if activated != nil {
				select {
				case <-activated:
				case <-ctx.Done():
					return
				}
			}
			ticker := time.NewTicker(250 * time.Millisecond)
			defer ticker.Stop()
			for {
				if err := owner.dispatch(ctx); err != nil {
					cancel(err)
					return
				}
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
				}
			}
		})
	}
	err := serveControlledAgentSession(ctx, connection, attached, environment, client, func(ctx context.Context, message *agentv1.GuestSessionMessage) error {
		if handled, err := owner.observe(ctx, message); handled {
			return err
		}
		return observe(ctx, message)
	}, activated)
	var failure SessionControlFailedError
	if errors.As(err, &failure) {
		return err
	}
	if cause := context.Cause(ctx); cause != nil {
		return cause
	}
	return err
}
