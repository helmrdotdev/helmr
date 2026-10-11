//go:build linux

package guestd

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/computerhost"
	"github.com/helmrdotdev/helmr/internal/frameio"
	agentv1 "github.com/helmrdotdev/helmr/internal/proto/agent/v1"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/wire"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workerclient"
	"google.golang.org/protobuf/proto"
)

// The outer CP test seeds a real database and authenticated HTTP server. Only
// MCP calls and authority renewal use that server; other authored operations are
// synthetic so this fixture makes no claim about durable Turn finalization.
func TestAgentNativeManagedMCP(t *testing.T) {
	path := os.Getenv("HELMR_NATIVE_CP_CONFIG")
	if path == "" {
		t.Skip("requires the outer CP/native qualification")
	}
	var config struct {
		BaseURL, Environment, Computer, Worker, Service, Secret, TargetSession, ServerName string
		Certificate                                                                        []byte
		Sessions                                                                           map[string]string
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(body, &config); err != nil {
		t.Fatal(err)
	}
	_, entry, registry, baseGrant, baseStart := nativeComputerFixture(t)
	entry.computerID = config.Computer
	certificate, err := x509.ParseCertificate(config.Certificate)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(certificate)
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: config.ServerName, MinVersion: tls.VersionTLS12}}
	defer transport.CloseIdleConnections()
	client, err := workerclient.New(config.BaseURL, workerclient.WithAuth(config.Worker, config.Secret), workerclient.WithService(config.Service), workerclient.WithHTTPClient(&http.Client{Transport: transport, Timeout: 10 * time.Second}))
	if err != nil {
		t.Fatal(err)
	}
	program := nativeProgramInput(t, baseGrant, baseStart)
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	var peers []*nativeManagedMCPPeer
	for _, provider := range []string{"codex", "claude"} {
		grant := proto.Clone(baseGrant).(*agentv1.SessionGrant)
		grant.Identity.SessionId = config.Sessions[provider]
		grant.ComputerId, grant.WorkerHostId = config.Computer, config.Worker
		grant.ComputerLeaseEpoch, grant.AuthorityGeneration = 1, 1
		start := proto.Clone(baseStart).(*agentv1.SessionStart)
		start.AgentId, start.ComputerId = provider, config.Computer
		input := *program
		input.grant = grant
		authority, err := client.AcquireAgentAttachment(ctx, workerapi.RuntimeSession{EnvironmentID: config.Environment, SessionID: grant.Identity.SessionId, ProcessEpoch: 1, ComputerLeaseEpoch: 1})
		if err != nil {
			t.Fatal(err)
		}
		if authority.AttachmentSequence <= 0 {
			t.Fatal("CP returned an invalid attachment sequence")
		}
		grant.AuthorityGeneration = authority.AuthorityGeneration
		grant.ExpiresAtUnixNano = authority.ExpiresAt.UnixNano()
		// Setup in this native fixture is synthetic; CP owns MCP/control renewal.
		// Shorten only the released envelope, after artifact work, so the test
		// observes hot renewal without making image packing a timing dependency.
		input.release = func(ctx context.Context) (*agentv1.SessionGrant, error) {
			authority, err := client.RenewAgentAuthority(ctx, workerapi.RuntimeSession{EnvironmentID: config.Environment, SessionID: grant.Identity.SessionId, ProcessEpoch: 1, ComputerLeaseEpoch: 1})
			if err != nil {
				return nil, err
			}
			grant.AuthorityGeneration = authority.AuthorityGeneration
			grant.ExpiresAtUnixNano = min(authority.ExpiresAt.UnixNano(), time.Now().Add(3*time.Second).UnixNano())
			return proto.Clone(grant).(*agentv1.SessionGrant), nil
		}
		relay, err := registry.openAgentSession(ctx, &agentv1.SessionAttach{Grant: grant, Start: start, AttachmentSequence: uint64(authority.AttachmentSequence)}, &input)
		if err != nil {
			t.Fatal(err)
		}
		peer := &nativeManagedMCPPeer{client: client, relay: relay, ready: make(chan struct{}), results: make(chan json.RawMessage, 2), stopped: make(chan error, 1)}
		connection, _, err := computerhost.OpenAgentSession(ctx, nativeMCPMachine{relay}, &agentv1.SessionAttach{Grant: grant, AttachmentSequence: uint64(authority.AttachmentSequence)})
		if err != nil {
			relay.finish(err)
			t.Fatal(err)
		}
		peer.connection = connection
		serviceCtx, stopService := context.WithCancel(ctx)
		peer.stop = stopService
		peer.serving = true
		go func() {
			peer.stopped <- computerhost.ServeAgentSession(serviceCtx, connection, config.Environment, peer, peer.observe)
		}()
		t.Cleanup(func() {
			peer.stop()
			_ = peer.connection.Close()
			relay.finish(context.Canceled)
			<-relay.finished
			if peer.serving {
				<-peer.stopped
			}
		})
		select {
		case <-peer.ready:
		case err := <-peer.stopped:
			peer.stopped <- err
			t.Fatal(err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		peers = append(peers, peer)
	}
	first := make([]nativeContinuationResult, len(peers))
	descriptors := make([]agentMCPConnection, len(peers))
	for index, peer := range peers {
		peer.dispatch(t, 1)
		first[index] = peer.result(t, ctx)
		peer.relay.mu.Lock()
		descriptors[index] = peer.relay.mcp.connection
		peer.relay.mu.Unlock()
		if first[index].Count != 1 || first[index].NativePID <= 0 {
			t.Fatalf("initial native state: %+v", first[index])
		}
	}
	// Both the initial guest envelopes and initial host JWTs expire during
	// this interval. The production service renews without authored assistance.
	timer := time.NewTimer(36 * time.Second)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	for index, peer := range peers {
		// Replace all host transport/client state while retaining the guest.
		// The replacement discovers its ordering from CP, not a local counter.
		peer.stop()
		_ = peer.connection.Close()
		<-peer.stopped
		peer.serving = false
		replacement, err := workerclient.New(config.BaseURL, workerclient.WithAuth(config.Worker, config.Secret), workerclient.WithService(config.Service), workerclient.WithHTTPClient(&http.Client{Transport: transport, Timeout: 10 * time.Second}))
		if err != nil {
			t.Fatal(err)
		}
		grant := peer.connection.CurrentGrant()
		authority, err := replacement.AcquireAgentAttachment(ctx, workerapi.RuntimeSession{EnvironmentID: config.Environment, SessionID: grant.Identity.SessionId, ProcessEpoch: 1, ComputerLeaseEpoch: 1})
		if err != nil || authority.AttachmentSequence != 2 {
			t.Fatalf("replacement attachment: %+v %v", authority, err)
		}
		grant.AuthorityGeneration, grant.ExpiresAtUnixNano = authority.AuthorityGeneration, authority.ExpiresAt.UnixNano()
		connection, attached, err := computerhost.OpenAgentSession(ctx, nativeMCPMachine{peer.relay}, &agentv1.SessionAttach{Grant: grant, AttachmentSequence: uint64(authority.AttachmentSequence)})
		if err != nil {
			t.Fatal(err)
		}
		peer.client, peer.connection = replacement, connection
		serviceCtx, stopService := context.WithCancel(ctx)
		peer.stop = stopService
		peer.serving = true
		go func() {
			peer.stopped <- computerhost.ServeAgentSession(serviceCtx, connection, config.Environment, peer, peer.observe)
		}()
		if !attached.Ready || attached.Terminal {
			t.Fatal("reattachment lost the retained process")
		}
		peer.dispatch(t, 2)
		second := peer.result(t, ctx)
		if second.Count != 2 || second.PID != first[index].PID || second.NativePID != first[index].NativePID || second.Nonce != first[index].Nonce {
			t.Fatalf("hot renewal replaced setup or process: first=%+v next=%+v", first[index], second)
		}
		peer.relay.mu.Lock()
		next := peer.relay.mcp.connection
		peer.relay.mu.Unlock()
		if next.URL != descriptors[index].URL || !maps.Equal(next.Headers, descriptors[index].Headers) || peer.renewals.Load() == 0 || peer.mcpCalls.Load() != 2 {
			t.Fatal("managed descriptor changed or authenticated renewal/MCP was not exercised")
		}
		t.Logf("Session %s retained native PID=%d setup nonce=%s across internal renewal", peer.connection.CurrentGrant().Identity.SessionId, second.NativePID, second.Nonce)
	}
}

type nativeMCPMachine struct{ relay *agentRelay }

func (nativeMCPMachine) Stream() vm.Stream          { return nil }
func (nativeMCPMachine) Wait(context.Context) error { return nil }
func (nativeMCPMachine) Close(context.Context) error {
	return errors.New("attachment must not close the Computer")
}
func (nativeMCPMachine) WithRunningGuestControl(ctx context.Context, _ vm.GuestControlStage, run func(context.Context) error) error {
	return run(ctx)
}
func (machine nativeMCPMachine) OpenStream(context.Context) (vm.Stream, error) {
	host, guest := net.Pipe()
	go func() {
		header, _, err := wire.ReadStreamFrameHeader(guest)
		if err != nil || header.Type != wire.StreamTypeAgentSession {
			_ = guest.Close()
			return
		}
		var request agentv1.SessionAttach
		if err := frameio.ReadProtoFrameBounded(guest, maxAgentTransportFrameBytes, &request); err != nil {
			_ = guest.Close()
			return
		}
		if err := machine.relay.attach(guest, &request); err != nil {
			_ = guest.Close()
			return
		}
		_ = machine.relay.serve(guest, request.AttachmentSequence)
	}()
	return host, nil
}

type nativeManagedMCPPeer struct {
	client     *workerclient.Client
	relay      *agentRelay
	connection *computerhost.AgentSessionConnection
	ready      chan struct{}
	readyOnce  sync.Once
	results    chan json.RawMessage
	stopped    chan error
	stop       context.CancelFunc
	serving    bool
	mu         sync.Mutex
	outcome    json.RawMessage
	renewals   atomic.Int64
	mcpCalls   atomic.Int64
}

func (peer *nativeManagedMCPPeer) RenewAgentAuthority(ctx context.Context, session workerapi.RuntimeSession) (workerapi.AgentAuthorityResponse, error) {
	result, err := peer.client.RenewAgentAuthority(ctx, session)
	if err == nil {
		peer.renewals.Add(1)
	}
	return result, err
}
func (peer *nativeManagedMCPPeer) AgentOperation(ctx context.Context, request workerapi.AgentOperationRequest) (workerapi.AgentOperationResponse, error) {
	if agentv1.Operation_Method(request.Method) == agentv1.Operation_METHOD_RUNTIME_MCP {
		result, err := peer.client.AgentOperation(ctx, request)
		if err == nil && result.Error == nil {
			peer.mcpCalls.Add(1)
		}
		return result, err
	}
	// These fixture-owned receipts do not represent CP lifecycle acceptance.
	value := json.RawMessage("null")
	switch agentv1.Operation_Method(request.Method) {
	case agentv1.Operation_METHOD_REGISTER_MESSAGES, agentv1.Operation_METHOD_CLOSE_PROCESSING, agentv1.Operation_METHOD_HOLD:
	case agentv1.Operation_METHOD_ASK:
		value = json.RawMessage(`{"id":"fixture-approval"}`)
	case agentv1.Operation_METHOD_WAIT_ASK:
		value = json.RawMessage(`{"allow":true}`)
	case agentv1.Operation_METHOD_OUTPUT:
		value = json.RawMessage(`{"sequence":1}`)
	case agentv1.Operation_METHOD_FINALIZE:
		var payload struct{ Result json.RawMessage }
		if err := json.Unmarshal(request.Payload, &payload); err != nil {
			return workerapi.AgentOperationResponse{}, err
		}
		peer.mu.Lock()
		peer.outcome = payload.Result
		peer.mu.Unlock()
		value, _ = json.Marshal(map[string]any{"status": "completed", "result": payload.Result})
	default:
		return workerapi.AgentOperationResponse{}, fmt.Errorf("unexpected authored operation: %d", request.Method)
	}
	return workerapi.AgentOperationResponse{RequestID: request.RequestID, Value: value}, nil
}
func (peer *nativeManagedMCPPeer) observe(ctx context.Context, message *agentv1.GuestSessionMessage) error {
	event := message.GetEvent()
	if event.GetReady() != nil {
		peer.readyOnce.Do(func() { close(peer.ready) })
	}
	if event.GetFailed() != nil {
		return fmt.Errorf("native Session failed: %v", event.GetFailed())
	}
	if delivery := event.GetDeliveryResult(); delivery != nil {
		if delivery.GetError() != nil {
			return fmt.Errorf("native delivery failed: %v", delivery.GetError())
		}
		peer.mu.Lock()
		outcome := peer.outcome
		peer.outcome = nil
		peer.mu.Unlock()
		if outcome != nil {
			select {
			case peer.results <- outcome:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	return nil
}
func (peer *nativeManagedMCPPeer) dispatch(t *testing.T, index int) {
	t.Helper()
	text, _ := json.Marshal(fmt.Sprintf("unique input %d", index))
	input, _ := json.Marshal([]map[string]string{{"type": "text", "text": string(text)}})
	identity := peer.connection.CurrentGrant().Identity
	if err := peer.connection.SendCommand(t.Context(), &agentv1.GuestCommand{Identity: identity, DeliveryId: fmt.Sprintf("delivery-%d", index), Command: &agentv1.GuestCommand_Dispatch{Dispatch: &agentv1.TurnDispatch{TurnId: fmt.Sprintf("turn-%d", index), Sequence: int64(index), InputJson: input, SourceJson: []byte(`{"kind":"api"}`)}}}); err != nil {
		t.Fatal(err)
	}
}
func (peer *nativeManagedMCPPeer) result(t *testing.T, ctx context.Context) nativeContinuationResult {
	t.Helper()
	select {
	case body := <-peer.results:
		var result nativeContinuationResult
		if err := json.Unmarshal(body, &result); err != nil {
			t.Fatal(err)
		}
		return result
	case err := <-peer.stopped:
		peer.stopped <- err
		t.Fatal(err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	return nativeContinuationResult{}
}
