package guestd

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/helmrdotdev/helmr/internal/mcppolicy"
	agentv1 "github.com/helmrdotdev/helmr/internal/proto/agent/v1"
	"github.com/helmrdotdev/helmr/internal/runtimemcp"
	"google.golang.org/protobuf/proto"
	"uuid"
)

type agentMCPConnection struct {
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers"`
}
type agentMCP struct {
	server             *http.Server
	connection         agentMCPConnection
	scopes             map[string]*agentMCPScope
	observationTimeout time.Duration
}
type agentMCPScope struct {
	id         string
	connection agentMCPConnection
	handler    http.Handler
}
type mcpHTTPContext struct{}
type mcpAdmission struct {
	ctx             context.Context
	grant           *agentv1.SessionGrant
	controlSequence int64
	scopeID         string
}

// Descriptor acquisition uses current Session authority during setup and idle.
// Authored code receives only the guest-local credential bound to the issued scope.
func (relay *agentRelay) receiveProgramEvent(event *agentv1.ProgramEvent) (*agentv1.GuestSessionMessage, *agentv1.GuestCommand, error) {
	if event.GetOperation().GetMethod() == agentv1.Operation_METHOD_GET_MCP_CONNECTION {
		relay.mu.Lock()
		relay.session.mu.Lock()
		rejected := relay.shuttingDown || relay.session.terminal
		if rejected {
			reply := relay.session.operationError(event.GetOperation().GetRequestId(), errors.New("session is shutting down"))
			relay.session.mu.Unlock()
			relay.mu.Unlock()
			return nil, reply, nil
		}
		relay.session.mu.Unlock()
		relay.mu.Unlock()
	}
	return relay.session.receive(event)
}

// Caller holds relay and Session locks. MCP is Session-scoped: setup and idle
// calls need no active Turn, and unknown provenance never becomes the latest Turn.
func (relay *agentRelay) authorizeMCPLocked() error {
	if err := relay.session.authorizeLocked(); err != nil {
		return err
	}
	if relay.session.held || relay.session.checkpointID != "" || relay.captureFrozen || relay.expiryFrozen || relay.shuttingDown {
		return errors.New("session MCP admission is unavailable")
	}
	return nil
}

func (relay *agentRelay) startMCPLocked(observationTimeout time.Duration) (*agentMCP, error) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	address := listener.Addr().String()
	bridge := &agentMCP{connection: agentMCPConnection{URL: "http://" + address + "/mcp"}, scopes: map[string]*agentMCPScope{}, observationTimeout: observationTimeout}
	bridge.server = &http.Server{ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 65 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 * 1024, Handler: http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		// The local token binds an Environment-trusted client to this physical
		// Session process. It has no meaning on a remote or management endpoint.
		if request.Host != address || request.URL.Path != "/mcp" || request.URL.RawQuery != "" || request.Header.Get("Origin") != "" {
			http.Error(w, "invalid local MCP connection", http.StatusForbidden)
			return
		}
		relay.mu.Lock()
		var scope *agentMCPScope
		for token, candidate := range bridge.scopes {
			if subtle.ConstantTimeCompare([]byte(request.Header.Get("Authorization")), []byte(token)) == 1 {
				scope = candidate
				break
			}
		}
		if scope == nil {
			relay.mu.Unlock()
			http.Error(w, "invalid local MCP connection", http.StatusForbidden)
			return
		}
		relay.session.mu.Lock()
		err := relay.authorizeMCPLocked()
		admission := mcpAdmission{grant: proto.Clone(relay.session.grant).(*agentv1.SessionGrant), controlSequence: relay.session.controlSequence, scopeID: scope.id}
		if err == nil && relay.mcpRequests >= 64 {
			err = errors.New("MCP request capacity exceeded")
		}
		if err == nil {
			relay.mcpRequests++
		}
		relay.session.mu.Unlock()
		relay.mu.Unlock()
		if err != nil {
			w.Header().Set("Retry-After", "1")
			http.Error(w, "Session MCP temporarily unavailable", http.StatusServiceUnavailable)
			return
		}
		defer func() { relay.mu.Lock(); relay.mcpRequests--; relay.notifyLocked(); relay.mu.Unlock() }()
		ctx, cancel := context.WithTimeout(request.Context(), 60*time.Second)
		defer cancel()
		// Older MCP protocol handlers may detach their context. Retain the original
		// HTTP lifetime so disconnect only cancels observation, never its mutation.
		admission.ctx = ctx
		ctx = context.WithValue(ctx, mcpHTTPContext{}, admission)
		scope.handler.ServeHTTP(w, request.WithContext(ctx))
		// net/http otherwise flushes its buffered response after this handler returns.
		// Keep capture pinned through that write, including a slow or lost client.
		_ = http.NewResponseController(w).Flush()
	})}
	go func() { _ = bridge.server.Serve(listener) }()
	return bridge, nil
}

func (relay *agentRelay) invokeMCP(ctx context.Context, tool string, arguments json.RawMessage, observationTimeout time.Duration) (json.RawMessage, error) {
	admission, ok := ctx.Value(mcpHTTPContext{}).(mcpAdmission)
	if !ok {
		return nil, errors.New("MCP request has no local admission")
	}
	// Preserve a tool's narrower deadline even when the protocol SDK detached
	// its context, while still observing the original HTTP lifetime.
	ctx, cancel := context.WithCancelCause(ctx)
	stopHTTP := context.AfterFunc(admission.ctx, func() { cancel(context.Cause(admission.ctx)) })
	defer func() { stopHTTP(); cancel(nil) }()
	if admission.ctx.Err() != nil {
		cancel(context.Cause(admission.ctx))
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	body, err := json.Marshal(struct {
		Tool      string          `json:"tool"`
		Arguments json.RawMessage `json:"arguments"`
	}{tool, arguments})
	if err != nil {
		return nil, err
	}
	relay.mu.Lock()
	session := relay.session
	session.mu.Lock()
	err = relay.authorizeMCPLocked()
	if err == nil {
		err = ctx.Err()
	}
	if err == nil && (!sameSessionGrantOwner(admission.grant, session.grant) || admission.grant.GetAuthorityGeneration() != session.grant.GetAuthorityGeneration() || admission.controlSequence != session.controlSequence) {
		err = errors.New("MCP request authority changed before operation admission")
	}
	if err == nil && relay.connection == nil {
		err = errors.New("MCP upstream is unavailable; retry with the same idempotency key")
	}
	size := len(body)
	for _, pending := range session.pending {
		size += proto.Size(pending.operation)
	}
	queuedBytes := len(body) + 1024
	for _, event := range relay.outbox {
		queuedBytes += proto.Size(event)
	}
	if err == nil && (len(relay.outbox) >= 1024 || queuedBytes > 64*1024*1024-2*1024*1024) {
		err = errors.New("session upstream event capacity exceeded")
	}
	if err == nil && (len(session.pending) >= 256 || size > 32*1024*1024) {
		err = errors.New("session pending operation limit exceeded")
	}
	if err != nil {
		session.mu.Unlock()
		relay.mu.Unlock()
		return nil, err
	}
	id := "mcp_" + rand.Text()
	operation := &agentv1.Operation{RequestId: id, Method: agentv1.Operation_METHOD_RUNTIME_MCP, PayloadJson: body}
	result := make(chan *agentv1.OperationResult, 1)
	session.pending[id] = &agentPendingOperation{operation: operation, generation: session.grant.GetAuthorityGeneration(), leaseEpoch: session.grant.GetComputerLeaseEpoch(), mcpResult: result}
	identity := proto.Clone(session.grant.GetIdentity()).(*agentv1.SessionIdentity)
	envelope := &agentv1.GuestSessionMessage{Identity: identity, OperationAuthorityGeneration: session.grant.GetAuthorityGeneration(), OperationLeaseEpoch: session.grant.GetComputerLeaseEpoch(), OperationMcpScopeId: admission.scopeID, Message: &agentv1.GuestSessionMessage_Event{Event: &agentv1.ProgramEvent{Identity: identity, Event: &agentv1.ProgramEvent_Operation{Operation: operation}}}, EventSequence: relay.nextSequence}
	session.mu.Unlock()
	relay.nextSequence++
	relay.outbox = append(relay.outbox, envelope)
	relay.notifyLocked()
	relay.mu.Unlock()
	timer := time.NewTimer(observationTimeout)
	defer timer.Stop()
	select {
	case reply := <-result:
		if failure := reply.GetError(); failure != nil {
			return nil, fmt.Errorf("%s: %s", failure.GetCode(), failure.GetMessage())
		}
		if !json.Valid(reply.GetValueJson()) {
			return nil, errors.New("invalid MCP operation response")
		}
		return reply.GetValueJson(), nil
	case <-timer.C:
		relay.cancelMCPObservation(envelope)
		return nil, errors.New("MCP observation timed out; outcome may be uncertain; retry with the same arguments and original idempotencyKey")
	case <-ctx.Done():
		relay.cancelMCPObservation(envelope)
		return nil, ctx.Err()
	case <-relay.finished:
		return nil, errors.New("session process ended")
	case <-relay.ctx.Done():
		return nil, relay.ctx.Err()
	}
}

// A cancelled observation still needs a host result to reconcile its retained
// pending entry. Cancellation is never permission to execute a mutation again.
func (relay *agentRelay) cancelMCPObservation(envelope *agentv1.GuestSessionMessage) {
	relay.enqueue(&agentv1.GuestSessionMessage{Identity: envelope.Identity, OperationAuthorityGeneration: envelope.OperationAuthorityGeneration, OperationLeaseEpoch: envelope.OperationLeaseEpoch, OperationMcpScopeId: envelope.OperationMcpScopeId, Message: &agentv1.GuestSessionMessage_Event{Event: &agentv1.ProgramEvent{Identity: envelope.Identity, Event: &agentv1.ProgramEvent_CancelOperation{CancelOperation: &agentv1.OperationCancel{RequestId: envelope.GetEvent().GetOperation().GetRequestId()}}}}})
}

// bindMCPScopeLocked consumes only an authenticated upstream receipt. The relay
// presents a guest-local credential; business authority remains in the receiver.
func (relay *agentRelay) bindMCPScopeLocked(command *agentv1.GuestCommand) error {
	result := command.GetOperationResult()
	if result == nil || result.GetError() != nil {
		return nil
	}
	relay.session.mu.Lock()
	pending := relay.session.pending[result.GetRequestId()]
	descriptor := pending != nil && pending.operation.GetMethod() == agentv1.Operation_METHOD_GET_MCP_CONNECTION
	relay.session.mu.Unlock()
	if !descriptor {
		return nil
	}
	var scope struct {
		ID     string          `json:"id"`
		Policy json.RawMessage `json:"policy"`
	}
	if err := decodeAgentJSON(result.GetValueJson(), &scope); err != nil {
		return err
	}
	if id, err := uuid.Parse(scope.ID); err != nil || id == uuid.Nil() {
		return errors.New("invalid MCP scope receipt")
	}
	policy, err := mcppolicy.Decode(scope.Policy)
	if err != nil {
		return err
	}
	if relay.mcp == nil {
		bridge, err := relay.startMCPLocked(45 * time.Second)
		if err != nil {
			return err
		}
		relay.mcp = bridge
	}
	var connection agentMCPConnection
	for _, existing := range relay.mcp.scopes {
		if existing.id == scope.ID {
			connection = existing.connection
			break
		}
	}
	if connection.URL == "" {
		token := "Bearer " + rand.Text()
		connection = agentMCPConnection{URL: relay.mcp.connection.URL, Headers: map[string]string{"Authorization": token}}
		handler := runtimemcp.NewScopedHandler(func(ctx context.Context, tool string, args json.RawMessage) (json.RawMessage, error) {
			return relay.invokeMCP(ctx, tool, args, relay.mcp.observationTimeout)
		}, policy)
		relay.mcp.scopes[token] = &agentMCPScope{id: scope.ID, connection: connection, handler: handler}
		if relay.mcp.connection.Headers == nil {
			relay.mcp.connection = connection
		}
	}
	body, err := json.Marshal(connection)
	if err != nil {
		return err
	}
	result.Outcome = &agentv1.OperationResult_ValueJson{ValueJson: body}
	return nil
}
