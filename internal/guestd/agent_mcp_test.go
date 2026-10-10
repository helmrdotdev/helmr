package guestd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/frameio"
	agentv1 "github.com/helmrdotdev/helmr/internal/proto/agent/v1"
	"google.golang.org/protobuf/proto"
)

func mcpFixture(t *testing.T) (*agentRelay, agentMCPConnection) {
	t.Helper()
	session, _ := agentSessionFixture(t)
	relay := newAgentRelay(t.Context(), session, nil)
	go relay.sendLoop()
	descriptor := mcpDescriptor(t, relay)
	t.Cleanup(func() { _ = relay.mcp.server.Close() })
	return relay, descriptor
}
func mcpDescriptor(t *testing.T, relay *agentRelay) agentMCPConnection {
	t.Helper()
	_, reply, err := relay.receiveProgramEvent(&agentv1.ProgramEvent{Identity: relay.session.grant.Identity, Event: &agentv1.ProgramEvent_Operation{Operation: &agentv1.Operation{RequestId: "descriptor", Method: agentv1.Operation_METHOD_GET_MCP_CONNECTION, PayloadJson: []byte("null")}}})
	if err != nil || reply.GetOperationResult().GetError() != nil {
		t.Fatalf("descriptor error: %v %v", err, reply)
	}
	var connection agentMCPConnection
	if err := json.Unmarshal(reply.GetOperationResult().GetValueJson(), &connection); err != nil {
		t.Fatal(err)
	}
	return connection
}

type mcpHTTPResult struct {
	status int
	body   string
	err    error
}

func mcpPost(ctx context.Context, descriptor agentMCPConnection, body string) mcpHTTPResult {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, descriptor.URL, strings.NewReader(body))
	if err != nil {
		return mcpHTTPResult{err: err}
	}
	for key, value := range descriptor.Headers {
		request.Header.Set(key, value)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	request.Header.Set("MCP-Protocol-Version", "2025-11-25")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return mcpHTTPResult{err: err}
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	return mcpHTTPResult{response.StatusCode, string(data), err}
}

const mcpSpawnBody = `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"spawn","arguments":{"agentId":"helper","input":[{"type":"text","text":"work"}],"idempotencyKey":"stable"}}}`

func readMCPEvent(t *testing.T, host io.Reader) *agentv1.GuestSessionMessage {
	t.Helper()
	result := new(agentv1.GuestSessionMessage)
	if err := frameio.ReadProtoFrameBounded(host, maxAgentTransportFrameBytes, result); err != nil {
		t.Fatal(err)
	}
	if result.GetEvent().GetOperation().GetMethod() != agentv1.Operation_METHOD_RUNTIME_MCP {
		t.Fatalf("wrong event: %v", result)
	}
	return result
}
func replyMCP(t *testing.T, host io.Writer, sequence uint64, event *agentv1.GuestSessionMessage) {
	t.Helper()
	reply := &agentv1.GuestCommand{Identity: event.Identity, Command: &agentv1.GuestCommand_OperationResult{OperationResult: &agentv1.OperationResult{RequestId: event.GetEvent().GetOperation().GetRequestId(), Outcome: &agentv1.OperationResult_ValueJson{ValueJson: []byte(`{"created":true}`)}}}}
	if err := writeAgentTransportFrame(host, &agentv1.HostSessionMessage{AttachmentSequence: sequence, Message: &agentv1.HostSessionMessage_Command{Command: reply}}); err != nil {
		t.Fatal(err)
	}
}
func TestAgentMCPSetupIdleAndReattachment(t *testing.T) {
	relay, descriptor := mcpFixture(t)
	relay.session.mu.Lock()
	relay.session.ready = false
	relay.session.mu.Unlock()
	if again := mcpDescriptor(t, relay); again.URL != descriptor.URL || again.Headers["Authorization"] != descriptor.Headers["Authorization"] {
		t.Fatal("descriptor changed during setup")
	}
	first := attachAgentRelay(t, relay, 1, relay.session.grant)
	response := make(chan mcpHTTPResult, 1)
	go func() { response <- mcpPost(t.Context(), descriptor, mcpSpawnBody) }()
	original := readMCPEvent(t, first)
	if original.GetEvent().GetOperation().GetTurnId() != "" || original.GetOperationAuthorityGeneration() != 3 {
		t.Fatal("fabricated Turn or wrong authority")
	}
	_ = first.Close()
	renewed := proto.Clone(relay.session.grant).(*agentv1.SessionGrant)
	renewed.AuthorityGeneration++
	second := attachAgentRelay(t, relay, 2, renewed)
	replay := readMCPEvent(t, second)
	if !proto.Equal(original.GetEvent(), replay.GetEvent()) || original.GetOperationAuthorityGeneration() != replay.GetOperationAuthorityGeneration() || original.GetEventSequence() != replay.GetEventSequence() {
		t.Fatal("uncertain operation changed on reconnect")
	}
	replyMCP(t, second, 2, replay)
	got := <-response
	if got.err != nil || got.status != 200 || !strings.Contains(got.body, `\"created\":true`) {
		t.Fatalf("MCP response: %+v", got)
	}
	relay.session.mu.Lock()
	relay.session.ready = true
	relay.session.mu.Unlock()
	go func() { response <- mcpPost(t.Context(), descriptor, mcpSpawnBody) }()
	next := readMCPEvent(t, second)
	if next.GetOperationAuthorityGeneration() != 4 || next.GetEvent().GetOperation().GetTurnId() != "" {
		t.Fatal("new call did not use renewed Session authority")
	}
	replyMCP(t, second, 2, next)
	if got := <-response; got.err != nil || got.status != 200 {
		t.Fatalf("renewed call: %+v", got)
	}
}
func TestAgentMCPRejectsExpiryHoldsAndInvalidClientsWithoutClosingSession(t *testing.T) {
	relay, descriptor := mcpFixture(t)
	host := attachAgentRelay(t, relay, 1, relay.session.grant)
	_ = host
	catalog := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`
	if got := mcpPost(t.Context(), descriptor, catalog); got.status != 200 || !strings.Contains(got.body, "spawn") || strings.Contains(got.body, "delete_computer") {
		t.Fatalf("catalog: %+v", got)
	}
	wrong := descriptor
	wrong.Headers = map[string]string{"Authorization": "Bearer wrong"}
	if got := mcpPost(t.Context(), wrong, catalog); got.status != 403 {
		t.Fatalf("invalid credential: %+v", got)
	}
	relay.session.mu.Lock()
	relay.session.held = true
	relay.session.mu.Unlock()
	if got := mcpPost(t.Context(), descriptor, catalog); got.status != 503 {
		t.Fatalf("hold: %+v", got)
	}
	relay.session.mu.Lock()
	relay.session.held = false
	future := time.Unix(0, relay.session.grant.ExpiresAtUnixNano).Add(time.Hour)
	relay.session.clock = func() time.Time { return future }
	relay.session.mu.Unlock()
	if got := mcpPost(t.Context(), descriptor, mcpSpawnBody); got.status != 503 {
		t.Fatalf("expiry: %+v", got)
	}
	relay.session.mu.Lock()
	held, terminal := relay.session.held, relay.session.terminal
	renewed := proto.Clone(relay.session.grant).(*agentv1.SessionGrant)
	relay.session.mu.Unlock()
	if held || terminal {
		t.Fatal("transient expiry changed Session lifecycle")
	}
	renewed.ExpiresAtUnixNano = future.Add(time.Hour).UnixNano()
	renewed.AuthorityGeneration++
	if err := relay.session.renew(renewed); err != nil {
		t.Fatal(err)
	}
	if got := mcpPost(t.Context(), descriptor, catalog); got.status != 200 {
		t.Fatalf("renewed local descriptor: %+v", got)
	}
	if got := mcpPost(t.Context(), descriptor, strings.Replace(mcpSpawnBody, `"agentId":"helper"`, `"callerSessionId":"other","agentId":"helper"`, 1)); !strings.Contains(got.body, "error") && !strings.Contains(got.body, "isError") {
		t.Fatalf("caller supplied identity accepted: %+v", got)
	}
	relay.mu.Lock()
	defer relay.mu.Unlock()
	if len(relay.outbox) != 0 {
		t.Fatal("rejected calls reached upstream")
	}
}
func TestAgentMCPCancelKeepsOriginalPendingMutationAndCaptureFence(t *testing.T) {
	relay, descriptor := mcpFixture(t)
	host := attachAgentRelay(t, relay, 1, relay.session.grant)
	ctx, cancel := context.WithCancel(t.Context())
	response := make(chan mcpHTTPResult, 1)
	go func() { response <- mcpPost(ctx, descriptor, mcpSpawnBody) }()
	event := readMCPEvent(t, host)
	cancel()
	<-response
	// The SDK's older protocol context can outlive the HTTP request. The bridge
	// still uses the original HTTP lifetime and sends cancellation of observation.
	var cancelled agentv1.GuestSessionMessage
	if err := frameio.ReadProtoFrameBounded(host, maxAgentTransportFrameBytes, &cancelled); err != nil {
		t.Fatal(err)
	}
	if cancelled.GetEvent().GetCancelOperation().GetRequestId() != event.GetEvent().GetOperation().GetRequestId() {
		t.Fatal("lost observation cancellation identity")
	}
	waitAgentRelay(t, relay, func() bool { return relay.mcpRequests == 0 })
	relay.mu.Lock()
	relay.session.mu.Lock()
	captureErr := relay.canCaptureLocked("capture")
	pending := relay.session.pending[event.GetEvent().GetOperation().GetRequestId()]
	relay.session.mu.Unlock()
	relay.mu.Unlock()
	if captureErr == nil || pending == nil || !bytes.Equal(pending.operation.PayloadJson, event.GetEvent().GetOperation().PayloadJson) {
		t.Fatal("uncertain mutation lost its capture fence")
	}
	replyMCP(t, host, 1, event)
	waitAgentRelay(t, relay, func() bool {
		relay.session.mu.Lock()
		defer relay.session.mu.Unlock()
		return len(relay.session.pending) == 0
	})
	relay.mu.Lock()
	relay.session.mu.Lock()
	captureErr = relay.canCaptureLocked("capture")
	relay.session.mu.Unlock()
	relay.mu.Unlock()
	if captureErr != nil {
		t.Fatal(captureErr)
	}
}

func TestAgentMCPSlowBodyCannotInheritResumedAuthority(t *testing.T) {
	relay, descriptor := mcpFixture(t)
	_ = attachAgentRelay(t, relay, 1, relay.session.grant)
	input, output := io.Pipe()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, descriptor.URL, input)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", descriptor.Headers["Authorization"])
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	request.Header.Set("MCP-Protocol-Version", "2025-11-25")
	response := make(chan mcpHTTPResult, 1)
	go func() {
		value, err := http.DefaultClient.Do(request)
		if err != nil {
			response <- mcpHTTPResult{err: err}
			return
		}
		defer value.Body.Close()
		body, err := io.ReadAll(value.Body)
		response <- mcpHTTPResult{value.StatusCode, string(body), err}
	}()
	// Sending only the first byte forces HTTP admission before the MCP tool exists.
	if _, err := output.Write([]byte("{")); err != nil {
		t.Fatal(err)
	}
	// HTTP admission does not normally need to wake a relay sender.
	deadline := time.Now().Add(time.Second)
	for {
		relay.mu.Lock()
		admitted := relay.mcpRequests == 1
		relay.mu.Unlock()
		if admitted {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("request was not admitted")
		}
		time.Sleep(time.Millisecond)
	}
	relay.session.mu.Lock()
	relay.session.held = true
	renewed := proto.Clone(relay.session.grant).(*agentv1.SessionGrant)
	renewed.AuthorityGeneration++
	relay.session.mu.Unlock()
	if err := relay.session.renew(renewed); err != nil {
		t.Fatal(err)
	}
	relay.session.mu.Lock()
	relay.session.held = false
	relay.session.controlSequence++
	relay.session.mu.Unlock()
	if _, err := output.Write([]byte(mcpSpawnBody[1:])); err != nil {
		t.Fatal(err)
	}
	_ = output.Close()
	got := <-response
	if got.err != nil || !strings.Contains(got.body, "authority changed") {
		t.Fatalf("stale HTTP request: %+v", got)
	}
	relay.mu.Lock()
	defer relay.mu.Unlock()
	if len(relay.outbox) != 0 {
		t.Fatal("stale HTTP request inherited fresh authority")
	}
}

type mcpClosingListener struct {
	net.Listener
	accepted chan struct{}
	closing  chan struct{}
	release  chan struct{}
	once     sync.Once
}

func (listener *mcpClosingListener) Accept() (net.Conn, error) {
	listener.once.Do(func() { close(listener.accepted) })
	return listener.Listener.Accept()
}
func (listener *mcpClosingListener) Close() error {
	close(listener.closing)
	<-listener.release
	return listener.Listener.Close()
}
func TestAgentMCPFinishSealsAdmissionBeforeClosingListener(t *testing.T) {
	session, _ := agentSessionFixture(t)
	relay := newAgentRelay(t.Context(), session, nil)
	socket, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	listener := &mcpClosingListener{Listener: socket, accepted: make(chan struct{}), closing: make(chan struct{}), release: make(chan struct{})}
	server := new(http.Server)
	relay.mcp = &agentMCP{server: server}
	go func() { _ = server.Serve(listener) }()
	<-listener.accepted
	go relay.finish(nil)
	<-listener.closing
	_, reply, err := relay.receiveProgramEvent(&agentv1.ProgramEvent{Identity: session.grant.Identity, Event: &agentv1.ProgramEvent_Operation{Operation: &agentv1.Operation{RequestId: "late-descriptor", Method: agentv1.Operation_METHOD_GET_MCP_CONNECTION, PayloadJson: []byte("null")}}})
	close(listener.release)
	<-relay.finished
	if err != nil || reply.GetOperationResult().GetError() == nil {
		t.Fatal("descriptor admitted while Session teardown was closing the bridge")
	}
}

func TestAgentMCPTimeoutReturnsUncertainOutcomeWhileHTTPRemainsAlive(t *testing.T) {
	session, _ := agentSessionFixture(t)
	relay := newAgentRelay(t.Context(), session, nil)
	bridge, err := relay.startMCPLocked(50 * time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	relay.mcp = bridge
	t.Cleanup(func() { _ = bridge.server.Close() })
	go relay.sendLoop()
	host := attachAgentRelay(t, relay, 1, session.grant)
	response := make(chan mcpHTTPResult, 1)
	go func() { response <- mcpPost(t.Context(), bridge.connection, mcpSpawnBody) }()
	event := readMCPEvent(t, host)
	var cancelled agentv1.GuestSessionMessage
	if err := frameio.ReadProtoFrameBounded(host, maxAgentTransportFrameBytes, &cancelled); err != nil {
		t.Fatal(err)
	}
	if cancelled.GetEvent().GetCancelOperation().GetRequestId() != event.GetEvent().GetOperation().GetRequestId() {
		t.Fatal("timeout did not retain original operation identity")
	}
	got := <-response
	if got.err != nil || got.status != 200 || !strings.Contains(got.body, `"isError":true`) || !strings.Contains(got.body, "original idempotencyKey") || strings.Contains(got.body, event.GetEvent().GetOperation().GetRequestId()) {
		t.Fatalf("timeout lost retry guidance: %+v", got)
	}
	replyMCP(t, host, 1, event)
}
func TestAgentMCPDescriptorSurvivesHoldAndWaitsForExpiredAuthority(t *testing.T) {
	relay, original := mcpFixture(t)
	relay.session.mu.Lock()
	relay.session.held = true
	relay.session.mu.Unlock()
	if got := mcpDescriptor(t, relay); got.URL != original.URL {
		t.Fatal("hold changed descriptor")
	}
	relay.session.mu.Lock()
	future := time.Unix(0, relay.session.grant.ExpiresAtUnixNano).Add(time.Hour)
	relay.session.clock = func() time.Time { return future }
	relay.session.mu.Unlock()
	_, reply, err := relay.receiveProgramEvent(&agentv1.ProgramEvent{Identity: relay.session.grant.Identity, Event: &agentv1.ProgramEvent_Operation{Operation: &agentv1.Operation{RequestId: "expired-descriptor", Method: agentv1.Operation_METHOD_GET_MCP_CONNECTION, PayloadJson: []byte("null")}}})
	if !errors.Is(err, errSessionGrantExpired) || reply != nil {
		t.Fatal("expiry produced permanent setup rejection")
	}
}

func TestAgentMCPWaitDeadlineSurvivesHTTPAdmissionContext(t *testing.T) {
	for _, stalled := range []string{"observation", "cancellation"} {
		t.Run(stalled, func(t *testing.T) {
			relay, descriptor := mcpFixture(t)
			host := attachAgentRelay(t, relay, 1, relay.session.grant)
			response := make(chan mcpHTTPResult, 1)
			go func() {
				response <- mcpPost(t.Context(), descriptor, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"wait_turn","arguments":{"sessionId":"child","turnId":"turn","timeoutMs":100}}}`)
			}()
			var event *agentv1.GuestSessionMessage
			if stalled == "cancellation" {
				event = readMCPEvent(t, host)
			}
			// Withhold transport reads until HTTP response EOF, not just the CP
			// result. Neither observation nor cancellation writes may delay it.
			select {
			case got := <-response:
				if got.err != nil || got.status != 200 || !strings.Contains(got.body, `\"status\":\"timeout\"`) {
					t.Fatalf("wait response: %+v", got)
				}
			case <-time.After(time.Second):
				t.Fatal("bounded wait blocked behind upstream socket I/O")
			}
			if event == nil {
				event = readMCPEvent(t, host)
			}
			if !strings.Contains(string(event.GetEvent().GetOperation().GetPayloadJson()), `"tool":"inspect_turn"`) {
				t.Fatal("wait did not observe exact Turn")
			}
			var cancelled agentv1.GuestSessionMessage
			if err := frameio.ReadProtoFrameBounded(host, maxAgentTransportFrameBytes, &cancelled); err != nil {
				t.Fatal(err)
			}
			if cancelled.GetEvent().GetCancelOperation().GetRequestId() != event.GetEvent().GetOperation().GetRequestId() {
				t.Fatal("wrong observation cancelled")
			}
			waitAgentRelay(t, relay, func() bool { return relay.mcpRequests == 0 })
			relay.session.mu.Lock()
			pending := relay.session.pending[event.GetEvent().GetOperation().GetRequestId()]
			held, terminal := relay.session.held, relay.session.terminal
			relay.session.mu.Unlock()
			if pending == nil || held || terminal {
				t.Fatal("timeout changed work or discarded unreconciled operation")
			}
			replyMCP(t, host, 1, event)
			waitAgentRelay(t, relay, func() bool {
				relay.session.mu.Lock()
				defer relay.session.mu.Unlock()
				return len(relay.session.pending) == 0
			})
		})
	}
}
