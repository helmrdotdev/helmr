package computerhost

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/helmrdotdev/helmr/internal/frameio"
	agentv1 "github.com/helmrdotdev/helmr/internal/proto/agent/v1"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/wire"
	"google.golang.org/protobuf/proto"
)

// AgentSessionConnection is one replaceable host attachment to a retained guest
// process. Closing it never requests Session shutdown or re-runs customer setup.
// The owning worker obtains grants and attachment sequences from the Control Plane.
type AgentSessionConnection struct {
	stream       vm.Stream
	identity     *agentv1.SessionIdentity
	attachment   uint64
	mu           sync.Mutex
	grant        *agentv1.SessionGrant
	received     uint64
	logReceived  [2]int64
	readMu       sync.Mutex
	writeMu      sync.Mutex
	renewMu      sync.Mutex
	closeOnce    sync.Once
	closed       chan struct{}
	closeErr     error
	stopLifetime func() bool
}

// ErrAgentSessionAbsent is an explicit guest observation, never a transport failure.
var ErrAgentSessionAbsent = errors.New("session process is absent")

func OpenAgentSession(ctx context.Context, machine vm.GuestControlMachine, request *agentv1.SessionAttach) (*AgentSessionConnection, *agentv1.SessionAttached, error) {
	if request.GetStart() != nil {
		return nil, nil, errors.New("first Session startup requires Program transfer and execution authority")
	}
	return openAgentSession(ctx, machine, request, nil)
}

func openAgentSession(ctx context.Context, machine vm.GuestControlMachine, request *agentv1.SessionAttach, starter AgentSessionStarter) (*AgentSessionConnection, *agentv1.SessionAttached, error) {
	if machine == nil || request.GetGrant().GetIdentity().GetSessionId() == "" || request.GetGrant().GetIdentity().GetProcessEpoch() <= 0 || request.GetAttachmentSequence() == 0 || proto.Size(request) > 128*1024*1024+64*1024 {
		return nil, nil, errors.New("invalid Session attachment")
	}
	request = proto.Clone(request).(*agentv1.SessionAttach)
	defer func() {
		for _, secret := range request.GetSecrets() {
			if secret != nil {
				clear(secret.Value)
				secret.Value = nil
			}
		}
	}()
	var connection *AgentSessionConnection
	var attached *agentv1.SessionAttached
	timeout := 30 * time.Second
	if starter != nil {
		timeout = 10 * time.Minute
	}
	exchangeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	err := machine.WithRunningGuestControl(exchangeCtx, vm.GuestControlOrdinary, func(exchangeCtx context.Context) error {
		stream, err := machine.OpenStream(exchangeCtx)
		if err != nil {
			return err
		}
		connection = &AgentSessionConnection{stream: stream, identity: proto.Clone(request.Grant.Identity).(*agentv1.SessionIdentity), attachment: request.AttachmentSequence, grant: proto.Clone(request.Grant).(*agentv1.SessionGrant), closed: make(chan struct{})}
		return connection.exchange(exchangeCtx, func() error {
			if _, err := writeGuestControlRequest(stream, wire.StreamHeader{Type: wire.StreamTypeAgentSession}, request); err != nil {
				return err
			}
			var receipt agentv1.GuestSessionMessage
			if err := frameio.ReadProtoFrameBounded(stream, agentTransportFrameLimit, &receipt); err != nil {
				return err
			}
			if err := connection.exchangeProgram(exchangeCtx, request, starter, &receipt); err != nil {
				return err
			}
			if starter == nil && receipt.GetAbsent() != nil && proto.Equal(receipt.GetIdentity(), connection.identity) && receipt.GetAttachmentSequence() == connection.attachment && receipt.GetEventSequence() == 0 {
				return ErrAgentSessionAbsent
			}
			if !proto.Equal(receipt.GetIdentity(), connection.identity) || receipt.GetAttachmentSequence() != connection.attachment || receipt.GetAttached() == nil || receipt.GetEventSequence() != 0 {
				return errors.New("invalid Session attachment receipt")
			}
			attached = receipt.GetAttached()
			return nil
		})
	})
	if err != nil {
		if connection != nil {
			_ = connection.Close()
		}
		return nil, nil, err
	}
	// Retain the caller's lifetime, not the bounded handshake's deadline.
	connection.mu.Lock()
	connection.stopLifetime = context.AfterFunc(ctx, func() { _ = connection.Close() })
	connection.mu.Unlock()
	return connection, attached, nil
}
func (connection *AgentSessionConnection) Close() error {
	connection.closeOnce.Do(func() {
		connection.mu.Lock()
		stop := connection.stopLifetime
		connection.mu.Unlock()
		if stop != nil {
			stop()
		}
		connection.closeErr = connection.stream.Close()
		close(connection.closed)
	})
	return connection.closeErr
}

// Cancellation of a partially read/written frame makes this attachment unusable.
func (connection *AgentSessionConnection) exchange(ctx context.Context, operation func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	joined := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { defer close(joined); _ = connection.Close() })
	err := operation()
	if !stop() {
		<-joined
	}
	if err != nil {
		_ = connection.Close()
		return errors.Join(err, ctx.Err())
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return nil
}
func (connection *AgentSessionConnection) send(ctx context.Context, message *agentv1.HostSessionMessage) error {
	if proto.Size(message) > agentTransportFrameLimit {
		return errors.New("session host frame exceeds transport bound")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	// A single serializer protects framing. Cancellation closes the stream so a
	// stalled earlier writer cannot keep later authority renewal blocked forever.
	return connection.exchange(ctx, func() error {
		connection.writeMu.Lock()
		defer connection.writeMu.Unlock()
		select {
		case <-connection.closed:
			return errors.New("session attachment is closed")
		default:
		}
		return frameio.WriteProtoFrame(connection.stream, message)
	})
}
func (connection *AgentSessionConnection) SendCommand(ctx context.Context, command *agentv1.GuestCommand) error {
	if command == nil || !proto.Equal(command.GetIdentity(), connection.identity) {
		return errors.New("session command identity changed")
	}
	return connection.send(ctx, &agentv1.HostSessionMessage{AttachmentSequence: connection.attachment, Message: &agentv1.HostSessionMessage_Command{Command: proto.Clone(command).(*agentv1.GuestCommand)}})
}
func (connection *AgentSessionConnection) Acknowledge(ctx context.Context, through uint64) error {
	connection.mu.Lock()
	valid := through > 0 && through <= connection.received
	connection.mu.Unlock()
	if !valid {
		return errors.New("session acknowledgment exceeds received events")
	}
	return connection.send(ctx, &agentv1.HostSessionMessage{AttachmentSequence: connection.attachment, Message: &agentv1.HostSessionMessage_Acknowledged{Acknowledged: &agentv1.SessionEventsAcknowledged{ThroughSequence: through}}})
}

// AcknowledgeLog is a durable acceptance receipt, independent of event handling.
func (connection *AgentSessionConnection) AcknowledgeLog(ctx context.Context, stream agentv1.SessionLog_Stream, through int64) error {
	return connection.acknowledgeLog(ctx, stream, through, false)
}

func (connection *AgentSessionConnection) acknowledgeLog(ctx context.Context, stream agentv1.SessionLog_Stream, through int64, expired bool) error {
	index := int(stream) - 1
	connection.mu.Lock()
	valid := index >= 0 && index < len(connection.logReceived) && through > 0 && through == connection.logReceived[index]
	connection.mu.Unlock()
	if !valid {
		return errors.New("session log acknowledgment does not match received record")
	}
	return connection.send(ctx, &agentv1.HostSessionMessage{AttachmentSequence: connection.attachment, Message: &agentv1.HostSessionMessage_LogAcknowledged{LogAcknowledged: &agentv1.SessionLogAcknowledged{Stream: stream, ThroughSequence: through, Expired: expired}}})
}
func validSessionLog(log *agentv1.SessionLog) bool {
	if log.GetSequence() <= 0 || log.GetThroughSequence() < log.GetSequence() || log.GetObservedAtUnixNano() <= 0 {
		return false
	}
	switch log.GetKind() {
	case agentv1.SessionLog_KIND_DATA:
		return log.GetSequence() == log.GetThroughSequence() && len(log.GetData()) > 0 && log.GetDroppedBytes() == 0 && !log.GetComplete()
	case agentv1.SessionLog_KIND_GAP:
		return len(log.GetData()) == 0 && log.GetDroppedBytes() > 0 && !log.GetComplete()
	case agentv1.SessionLog_KIND_END:
		return log.GetSequence() == log.GetThroughSequence() && len(log.GetData()) == 0 && log.GetDroppedBytes() == 0
	default:
		return false
	}
}

// Receive has one reader. Dispatch work separately so long-running waits never
// prevent receipt processing or host-owned authority renewal.
func (connection *AgentSessionConnection) Receive(ctx context.Context) (*agentv1.GuestSessionMessage, error) {
	if !connection.readMu.TryLock() {
		return nil, errors.New("session attachment already has a reader")
	}
	defer connection.readMu.Unlock()
	message := new(agentv1.GuestSessionMessage)
	err := connection.exchange(ctx, func() error {
		return frameio.ReadProtoFrameBounded(connection.stream, agentTransportFrameLimit, message)
	})
	if err != nil {
		return nil, err
	}
	connection.mu.Lock()
	valid := proto.Equal(message.GetIdentity(), connection.identity) && message.GetAttachmentSequence() == connection.attachment
	if log := message.GetLog(); log != nil {
		index := int(log.GetStream()) - 1
		valid = valid && message.GetEventSequence() == 0 && index >= 0 && index < len(connection.logReceived) && validSessionLog(log)
		if valid {
			valid = log.GetSequence() > connection.logReceived[index]
			if valid {
				connection.logReceived[index] = log.GetThroughSequence()
			}
		}
	} else {
		valid = valid && message.GetEventSequence() > connection.received && (message.GetEvent() != nil || message.GetStopped() != nil)
	}
	if event := message.GetEvent(); event != nil {
		valid = valid && proto.Equal(event.GetIdentity(), connection.identity)
		if event.GetOperation() != nil || event.GetCancelOperation() != nil {
			valid = valid && message.GetOperationAuthorityGeneration() > 0 && message.GetOperationLeaseEpoch() > 0
		}
	}
	if valid && message.GetLog() == nil {
		connection.received = message.GetEventSequence()
	}
	connection.mu.Unlock()
	if !valid {
		_ = connection.Close()
		return nil, errors.New("invalid Session event envelope")
	}
	// Original operation generations pass through unchanged for CP reconciliation.
	return message, nil
}
func (connection *AgentSessionConnection) CurrentGrant() *agentv1.SessionGrant {
	connection.mu.Lock()
	defer connection.mu.Unlock()
	return proto.Clone(connection.grant).(*agentv1.SessionGrant)
}
func (connection *AgentSessionConnection) Renew(ctx context.Context, grant *agentv1.SessionGrant) error {
	connection.renewMu.Lock()
	defer connection.renewMu.Unlock()
	return connection.renewLocked(ctx, grant)
}
func (connection *AgentSessionConnection) renewLocked(ctx context.Context, grant *agentv1.SessionGrant) error {
	current := connection.CurrentGrant()
	if grant == nil {
		return errors.New("session renewal has no grant")
	}
	next := proto.Clone(grant).(*agentv1.SessionGrant)
	owner := proto.Clone(next).(*agentv1.SessionGrant)
	owner.AuthorityGeneration = current.AuthorityGeneration
	owner.ExpiresAtUnixNano = current.ExpiresAtUnixNano
	if !proto.Equal(owner, current) || next.AuthorityGeneration < current.AuthorityGeneration || next.ExpiresAtUnixNano < current.ExpiresAtUnixNano {
		return errors.New("session renewal changed or regressed its owner")
	}
	if err := connection.send(ctx, &agentv1.HostSessionMessage{AttachmentSequence: connection.attachment, Message: &agentv1.HostSessionMessage_Renew{Renew: next}}); err != nil {
		return err
	}
	connection.mu.Lock()
	connection.grant = next
	connection.mu.Unlock()
	return nil
}

// MaintainAuthority renews short-lived upstream authority while user code remains
// hot or idle. The callback is host-owned CP communication, never authored code.
// Transient failures leave the existing process alone; expired guest authority
// rejects new work until a fresh grant arrives or CP lifecycle control ends it.
func (connection *AgentSessionConnection) MaintainAuthority(ctx context.Context, renew func(context.Context, *agentv1.SessionGrant) (*agentv1.SessionGrant, error)) error {
	if renew == nil {
		return errors.New("session authority renewal callback is required")
	}
	retry := false
	for {
		current := connection.CurrentGrant()
		delay := max(time.Second, time.Until(time.Unix(0, current.ExpiresAtUnixNano))/2)
		if retry {
			delay = time.Second
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-connection.closed:
			timer.Stop()
			return errors.New("session attachment is closed")
		case <-timer.C:
		}
		requestCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		next, err := renew(requestCtx, current)
		cancel()
		if err != nil {
			var rejected interface{ SessionAuthorityRejected() bool }
			var hostRejected interface{ WorkerAuthorityRejected() bool }
			if (errors.As(err, &rejected) && rejected.SessionAuthorityRejected()) || (errors.As(err, &hostRejected) && hostRejected.WorkerAuthorityRejected()) {
				return err
			}
			retry = true
			continue
		}
		if next == nil || next.ExpiresAtUnixNano < current.ExpiresAtUnixNano || (next.ExpiresAtUnixNano == current.ExpiresAtUnixNano && next.AuthorityGeneration <= current.AuthorityGeneration) {
			retry = true
			continue
		}
		if err := connection.reconcileRenewal(ctx, next); err != nil {
			return err
		}
		retry = false
	}
}

// reconcileRenewal handles a CP response that may have been overtaken by control
// delivery. Physical ownership remains immutable even for an obsolete response.
func (connection *AgentSessionConnection) reconcileRenewal(ctx context.Context, grant *agentv1.SessionGrant) error {
	connection.renewMu.Lock()
	defer connection.renewMu.Unlock()
	current := connection.CurrentGrant()
	if grant == nil {
		return errors.New("session renewal has no grant")
	}
	next := proto.Clone(grant).(*agentv1.SessionGrant)
	owner := proto.Clone(next).(*agentv1.SessionGrant)
	owner.AuthorityGeneration = current.AuthorityGeneration
	owner.ExpiresAtUnixNano = current.ExpiresAtUnixNano
	if !proto.Equal(owner, current) {
		return errors.New("session renewal changed its owner")
	}
	if next.AuthorityGeneration < current.AuthorityGeneration {
		return nil
	}
	next.ExpiresAtUnixNano = max(next.ExpiresAtUnixNano, current.ExpiresAtUnixNano)
	if proto.Equal(next, current) {
		return nil
	}
	return connection.renewLocked(ctx, next)
}
