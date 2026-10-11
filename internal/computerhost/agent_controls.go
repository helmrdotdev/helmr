package computerhost

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/helmrdotdev/helmr/internal/httpclient"
	agentv1 "github.com/helmrdotdev/helmr/internal/proto/agent/v1"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

// AgentControlClient owns durable desired controls and physical stop observation.
// Business operation forwarding remains on the same authenticated worker client.
type AgentControlClient interface {
	AgentRuntimeClient
	AppendSessionLog(context.Context, workerapi.SessionLogRequest) (workerapi.DiagnosticLogReceipt, error)
	PrepareAgentControl(context.Context, workerapi.AgentControlRequest) (workerapi.AgentControlResponse, error)
	AcknowledgeAgentControl(context.Context, workerapi.AgentControlReceipt) error
	ObserveAgentStopped(context.Context, workerapi.AgentControlRequest) error
	ObserveAgentReady(context.Context, workerapi.AgentControlRequest) error
	ObserveAgentFailure(context.Context, workerapi.AgentControlRequest) error
}

// SessionControlFailedError requires lifecycle reconciliation by the process
// owner. Reattaching is not recovery from a failed runtime control.
type SessionControlFailedError struct {
	Session            workerapi.RuntimeSession
	ReceiptError       error
	FailureError       error
	AttachmentSequence int64
	Kind               string
	Message            string
}

func (e SessionControlFailedError) Error() string {
	return fmt.Sprintf("Session %s failed: %s", e.Kind, e.Message)
}

type agentSessionControls struct {
	connection  *AgentSessionConnection
	client      AgentControlClient
	environment string
	mu          sync.Mutex
	ready       bool
	terminal    bool
	latest      workerapi.AgentControlResponse
	sent        int64
	failure     *SessionControlFailedError
}

func controlDeliveryID(control workerapi.AgentControlResponse) string {
	return fmt.Sprintf("control:%d:%d:%s", control.Sequence, control.AuthorityGeneration, control.Kind)
}

func (owner *agentSessionControls) request() workerapi.AgentControlRequest {
	grant := owner.connection.CurrentGrant()
	return workerapi.AgentControlRequest{Session: owner.connection.runtimeSession(owner.environment, grant.GetComputerLeaseEpoch()), AttachmentSequence: int64(owner.connection.attachment)}
}

func (owner *agentSessionControls) apply(ctx context.Context) error {
	// A terminal attachment only drains retained events, including the physical
	// stop receipt. Commands are no longer admitted by this process.
	owner.mu.Lock()
	terminal := owner.terminal
	owner.mu.Unlock()
	if terminal {
		return nil
	}
	requestCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	control, err := owner.client.PrepareAgentControl(requestCtx, owner.request())
	cancel()
	owner.mu.Lock()
	terminal = owner.terminal
	owner.mu.Unlock()
	if terminal {
		return nil
	}
	if err != nil {
		var rejected interface{ SessionAuthorityRejected() bool }
		var hostRejected interface{ WorkerAuthorityRejected() bool }
		if (errors.As(err, &rejected) && rejected.SessionAuthorityRejected()) || (errors.As(err, &hostRejected) && hostRejected.WorkerAuthorityRejected()) {
			return err
		}
		// The same attachment and delivery identity reconcile an uncertain prepare.
		return nil
	}
	if control.Sequence <= 0 || control.AuthorityGeneration <= 0 || !control.ExpiresAt.After(time.Now()) {
		return errors.New("invalid Session control authority")
	}
	command := &agentv1.GuestCommand{Identity: owner.connection.identity, DeliveryId: controlDeliveryID(control), ControlSequence: control.Sequence}
	switch control.Kind {
	case "suspend":
		command.Command = &agentv1.GuestCommand_Suspend{Suspend: &agentv1.SessionSuspend{Reason: "Session is held"}}
	case "resume":
		command.Command = &agentv1.GuestCommand_Resume{Resume: &agentv1.SessionResume{}}
	case "shutdown":
		command.Command = &agentv1.GuestCommand_Shutdown{Shutdown: &agentv1.SessionShutdown{Reason: "Session process must stop"}}
	default:
		return errors.New("unknown Session control")
	}
	owner.mu.Lock()
	if control.Sequence < owner.latest.Sequence {
		owner.mu.Unlock()
		return errors.New("session control sequence regressed")
	}
	owner.latest = control
	send := !control.Acknowledged && owner.sent != control.Sequence && (control.Kind != "resume" || owner.ready)
	owner.mu.Unlock()
	if control.Error != "" {
		return owner.fail(ctx, control.Kind, control.Error, nil)
	}
	if !send {
		return nil
	}
	// Renewal races are serialized with MaintainAuthority. Keep an already newer
	// expiry; a stale state read never rolls authority back or sends its command.
	owner.connection.renewMu.Lock()
	defer owner.connection.renewMu.Unlock()
	grant := owner.connection.CurrentGrant()
	if grant.AuthorityGeneration > control.AuthorityGeneration {
		return nil
	}
	grant.AuthorityGeneration = control.AuthorityGeneration
	grant.ExpiresAtUnixNano = max(grant.ExpiresAtUnixNano, control.ExpiresAt.UnixNano())
	if err := owner.connection.renewLocked(ctx, grant); err != nil {
		return err
	}
	if err := owner.connection.SendCommand(ctx, command); err != nil {
		return err
	}
	owner.mu.Lock()
	owner.sent = control.Sequence
	owner.mu.Unlock()
	return nil
}

func (owner *agentSessionControls) observe(ctx context.Context, message *agentv1.GuestSessionMessage) (bool, error) {
	if message.GetStopped() != nil {
		owner.mu.Lock()
		owner.terminal = true
		owner.mu.Unlock()
		return true, owner.client.ObserveAgentStopped(ctx, owner.request())
	}
	if message.GetEvent().GetReady() != nil {
		if err := owner.client.ObserveAgentReady(ctx, owner.request()); err != nil {
			return true, err
		}
		owner.mu.Lock()
		owner.ready = true
		owner.mu.Unlock()
		return true, nil
	}
	result := message.GetEvent().GetDeliveryResult()
	if result == nil || !strings.HasPrefix(result.GetDeliveryId(), "control:") {
		return false, nil
	}
	owner.mu.Lock()
	current := owner.latest
	owner.mu.Unlock()
	failure := ""
	if result.GetError() != nil {
		failure = result.GetError().GetMessage()
	}
	if result.GetError() != nil && failure == "" {
		failure = "Session control failed"
	}
	if len(failure) > 4096 {
		failure = "Session control failed with an oversized error"
	}
	if result.GetDeliveryId() != controlDeliveryID(current) {
		// A retained rejection is still a fact about this process even when a
		// newer control or attachment superseded its ordinary receipt.
		if failure != "" {
			return true, owner.fail(ctx, "control", failure, nil)
		}
		return true, nil
	}
	if !bytes.Equal(bytes.TrimSpace(result.GetValueJson()), []byte("null")) && failure == "" {
		failure = "Session control was not applied"
	}
	r := owner.request()
	err := owner.client.AcknowledgeAgentControl(ctx, workerapi.AgentControlReceipt{Session: r.Session, AttachmentSequence: r.AttachmentSequence, Sequence: current.Sequence, AuthorityGeneration: current.AuthorityGeneration, Kind: current.Kind, Error: failure})
	if failure != "" {
		// The guest has definitively rejected this control even if storing its
		// receipt is uncertain. Reattachment must not erase or replay that failure.
		return true, owner.fail(ctx, current.Kind, failure, err)
	}
	return true, err
}

// ServeControlledAgentSession connects durable controls to the retained Session
// transport. An uncertain API/pipe outcome ends this attachment; its owner must
// reacquire an attachment and reconcile, never restart setup to retry transport.
func ServeControlledAgentSession(ctx context.Context, connection *AgentSessionConnection, attached *agentv1.SessionAttached, environment string, client AgentControlClient, observe func(context.Context, *agentv1.GuestSessionMessage) error) error {
	return serveControlledAgentSession(ctx, connection, attached, environment, client, observe, nil)
}

func serveControlledAgentSession(ctx context.Context, connection *AgentSessionConnection, attached *agentv1.SessionAttached, environment string, client AgentControlClient, observe func(context.Context, *agentv1.GuestSessionMessage) error, activated <-chan struct{}) error {
	if connection == nil || attached == nil || client == nil || observe == nil || environment == "" {
		return errors.New("controlled Session requires its attachment, client and observer")
	}
	owner := &agentSessionControls{connection: connection, client: client, environment: environment, ready: attached.GetReady(), terminal: attached.GetTerminal()}
	ctx, cancel := context.WithCancelCause(ctx)
	var jobs sync.WaitGroup
	stop := func() { cancel(nil); _ = connection.Close(); jobs.Wait() }
	defer stop()
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
			if err := owner.apply(ctx); err != nil {
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
	err := serveAgentSession(ctx, connection, environment, client, func(ctx context.Context, message *agentv1.GuestSessionMessage) error {
		if log := message.GetLog(); log != nil {
			return connection.appendSessionLog(ctx, environment, client, log)
		}
		handled, err := owner.observe(ctx, message)
		var failed SessionControlFailedError
		if errors.As(err, &failed) && failed.FailureError == nil {
			// The failure is durable before its retained guest event is released.
			// Keep the typed outcome so the owner reconciles shutdown on its next
			// attachment. An uncertain report must leave this event replayable.
			return errors.Join(err, connection.Acknowledge(ctx, message.GetEventSequence()))
		}
		if err != nil || handled {
			return err
		}
		return observe(ctx, message)
	}, activated)
	// A retained failure report can finish after the reader exits. Join before
	// selecting the outcome so cancellation cannot turn a known failure into retry.
	cause := context.Cause(ctx)
	stop()
	owner.mu.Lock()
	failedControl := owner.failure
	owner.mu.Unlock()
	if failedControl != nil {
		return *failedControl
	}
	if err == nil {
		return nil
	}
	var failed SessionControlFailedError
	if errors.As(err, &failed) {
		return err
	}
	if cause != nil {
		return cause
	}
	return err
}

func (owner *agentSessionControls) fail(ctx context.Context, kind, message string, receiptError error) error {
	// Attachment cancellation must not erase a known failure. This bounded report
	// is joined by the transport owner, and its uncertain outcome remains typed.
	reportCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	request := owner.request()
	err := owner.client.ObserveAgentFailure(reportCtx, request)
	failure := SessionControlFailedError{Session: request.Session, AttachmentSequence: request.AttachmentSequence, Kind: kind, Message: message, ReceiptError: receiptError, FailureError: err}
	owner.mu.Lock()
	if owner.failure == nil || receiptError != nil {
		owner.failure = &failure
	}
	owner.mu.Unlock()
	return failure
}

// This is the production observer for diagnostics: only a committed CP receipt
// allows the service to release the retained guest record.
func (connection *AgentSessionConnection) appendSessionLog(ctx context.Context, environment string, client AgentControlClient, log *agentv1.SessionLog) error {
	stream := "stdout"
	if log.GetStream() == agentv1.SessionLog_STREAM_STDERR {
		stream = "stderr"
	}
	kind := map[agentv1.SessionLog_Kind]string{agentv1.SessionLog_KIND_DATA: "data", agentv1.SessionLog_KIND_GAP: "gap", agentv1.SessionLog_KIND_END: "end"}[log.GetKind()]
	grant := connection.CurrentGrant()
	request := workerapi.SessionLogRequest{Session: connection.runtimeSession(environment, grant.GetComputerLeaseEpoch()), AttachmentSequence: int64(connection.attachment), Stream: stream, Kind: kind, Sequence: log.GetSequence(), ThroughSequence: log.GetThroughSequence(), ObservedAtUnixNano: log.GetObservedAtUnixNano(), Data: log.GetData(), DroppedBytes: log.GetDroppedBytes(), Complete: log.GetComplete()}
	receipt, err := client.AppendSessionLog(ctx, request)
	if err != nil {
		var response *httpclient.Error
		if errors.As(err, &response) && response.StatusCode >= 400 && response.StatusCode < 500 && response.StatusCode != 408 && response.StatusCode != 429 {
			return errDiagnosticRejected
		}
		return err
	}
	if receipt.ThroughSequence != request.ThroughSequence {
		return errDiagnosticRejected
	}
	if receipt.Expired {
		if !receipt.AcceptedAt.IsZero() || !receipt.ExpiresAt.IsZero() {
			return errDiagnosticRejected
		}
		return errDiagnosticExpired
	}
	if receipt.AcceptedAt.IsZero() || receipt.ExpiresAt.Sub(receipt.AcceptedAt) != 90*24*time.Hour {
		return errDiagnosticRejected
	}
	return nil
}
