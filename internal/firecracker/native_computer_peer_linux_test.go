//go:build linux && computerproof

package firecracker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"testing"

	"github.com/helmrdotdev/helmr/internal/computerhost"
	agentv1 "github.com/helmrdotdev/helmr/internal/proto/agent/v1"
	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"google.golang.org/protobuf/proto"
)

// Only operation admission is synthetic. Session framing, relay, managed Node,
// native process, local MCP and checkpoint transport are production paths.
type nativeKVMPeer struct {
	program      *agentv1.SessionProgram
	programFiles map[string]string
	grant        *agentv1.SessionGrant
	connection   *computerhost.AgentSessionConnection
	done         chan struct{}
	serveErr     error
	ready        chan struct{}
	results      chan nativeKVMResult
	mcp          chan workerapi.AgentOperationRequest
	mu           sync.Mutex
	result       nativeKVMResult
	completed    int
	receipts     map[string]nativeKVMReceipt
}
type nativeKVMReceipt struct {
	Request  workerapi.AgentOperationRequest
	Response workerapi.AgentOperationResponse
}
type nativeKVMResult struct {
	Count, PID, NativePID int
	Nonce                 string
}

func newNativeKVMPeer(grant *agentv1.SessionGrant) *nativeKVMPeer {
	return &nativeKVMPeer{receipts: make(map[string]nativeKVMReceipt), grant: grant, ready: make(chan struct{}, 1), results: make(chan nativeKVMResult, 2), mcp: make(chan workerapi.AgentOperationRequest, 2)}
}
func (p *nativeKVMPeer) attach(ctx context.Context, machine vm.GuestControlMachine, sequence uint64, initial bool) error {
	request := &agentv1.SessionAttach{Grant: p.grant, AttachmentSequence: sequence}
	if initial {
		request.Start = &agentv1.SessionStart{AgentId: p.grant.Identity.SessionId, ComputerId: p.grant.ComputerId, DeploymentId: "native-kvm", RecoveryKind: agentv1.SessionStart_RECOVERY_KIND_INITIAL, Program: p.program}
	}
	var connection *computerhost.AgentSessionConnection
	var attached *agentv1.SessionAttached
	var err error
	if initial {
		connection, attached, err = computerhost.StartAgentSession(ctx, machine, request, p)
	} else {
		connection, attached, err = computerhost.OpenAgentSession(ctx, machine, request)
	}
	if err != nil {
		return err
	}
	p.connection, p.done = connection, make(chan struct{})
	if attached.Ready {
		select {
		case p.ready <- struct{}{}:
		default:
		}
	}
	go func() {
		p.serveErr = computerhost.ServeAgentSession(ctx, connection, "native-kvm", p, p.observe)
		close(p.done)
	}()
	return nil
}
func (p *nativeKVMPeer) close(ctx context.Context) error {
	if p.connection == nil {
		return nil
	}
	_ = p.connection.Close()
	select {
	case <-p.done:
		p.connection = nil
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (p *nativeKVMPeer) RenewAgentAuthority(context.Context, workerapi.RuntimeSession) (workerapi.AgentAuthorityResponse, error) {
	return workerapi.AgentAuthorityResponse{}, errors.New("unexpected renewal during bounded native KVM fixture")
}
func (p *nativeKVMPeer) AgentOperation(_ context.Context, request workerapi.AgentOperationRequest) (response workerapi.AgentOperationResponse, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if prior, ok := p.receipts[request.RequestID]; ok {
		original := prior.Request
		if request.Session != original.Session || request.AuthorityGeneration != original.AuthorityGeneration || request.TurnID != original.TurnID || request.Method != original.Method || request.DrainEvidence != original.DrainEvidence || !bytes.Equal(request.Payload, original.Payload) {
			return response, errors.New("fixture operation replay changed original identity")
		}
		return prior.Response, nil
	}
	defer func() {
		if err == nil {
			request.Payload = append(json.RawMessage(nil), request.Payload...)
			p.receipts[request.RequestID] = nativeKVMReceipt{request, response}
		}
	}()
	var value json.RawMessage
	switch agentv1.Operation_Method(request.Method) {
	case agentv1.Operation_METHOD_REGISTER_MESSAGES, agentv1.Operation_METHOD_CLOSE_PROCESSING, agentv1.Operation_METHOD_HOLD:
		value = json.RawMessage(`null`)
	case agentv1.Operation_METHOD_ASK:
		value = json.RawMessage(`{"id":"fixture-approval"}`)
	case agentv1.Operation_METHOD_WAIT_ASK:
		value = json.RawMessage(`{"allow":true}`)
	case agentv1.Operation_METHOD_OUTPUT:
		value = json.RawMessage(`{"sequence":1}`)
	case agentv1.Operation_METHOD_RUNTIME_MCP:
		if request.TurnID != "" {
			return workerapi.AgentOperationResponse{}, errors.New("MCP inferred a Turn")
		}
		var call struct {
			Tool      string
			Arguments struct{ IdempotencyKey string }
		}
		if err := json.Unmarshal(request.Payload, &call); err != nil {
			return workerapi.AgentOperationResponse{}, err
		}
		if call.Tool != "enqueue" || call.Arguments.IdempotencyKey == "" {
			return workerapi.AgentOperationResponse{}, fmt.Errorf("unexpected MCP request: %s", request.Payload)
		}
		select {
		case p.mcp <- request:
		default:
			return response, errors.New("unexpected native MCP call count")
		}
		value, _ = json.Marshal(map[string]any{"sessionId": "peer", "turnId": "fixture-tool-turn-" + call.Arguments.IdempotencyKey, "sequence": 1})
	case agentv1.Operation_METHOD_FINALIZE:
		var payload struct{ Result json.RawMessage }
		if err := json.Unmarshal(request.Payload, &payload); err != nil {
			return workerapi.AgentOperationResponse{}, err
		}
		var result nativeKVMResult
		if err := json.Unmarshal(payload.Result, &result); err != nil {
			return workerapi.AgentOperationResponse{}, err
		}
		if result.Count > p.completed {
			p.result = result
		}
		value, _ = json.Marshal(map[string]any{"status": "completed", "result": payload.Result})
	default:
		return workerapi.AgentOperationResponse{}, fmt.Errorf("unexpected native operation: %d", request.Method)
	}
	return workerapi.AgentOperationResponse{RequestID: request.RequestID, Value: value}, nil
}
func (p *nativeKVMPeer) observe(_ context.Context, envelope *agentv1.GuestSessionMessage) error {
	if envelope.GetStopped() != nil {
		return fmt.Errorf("Session stopped: %v", envelope.GetStopped())
	}
	event := envelope.GetEvent()
	if event == nil {
		return nil
	}
	if event.GetReady() != nil {
		select {
		case p.ready <- struct{}{}:
		default:
		}
	}
	if failed := event.GetFailed(); failed != nil {
		return fmt.Errorf("native Session failed: %v", failed)
	}
	if delivery := event.GetDeliveryResult(); delivery != nil {
		if delivery.GetError() != nil {
			return fmt.Errorf("native dispatch: %v", delivery.GetError())
		}
		p.mu.Lock()
		result := p.result
		deliver := result.Count > p.completed && delivery.GetDeliveryId() == fmt.Sprintf("dispatch-%d", result.Count)
		if deliver {
			p.completed = result.Count
			p.result = nativeKVMResult{}
		}
		p.mu.Unlock()
		if deliver {
			p.results <- result
		}
	}
	return nil
}
func (p *nativeKVMPeer) waitReady(t *testing.T, ctx context.Context) {
	t.Helper()
	select {
	case <-p.ready:
	case <-p.done:
		t.Fatalf("native setup: %v", p.serveErr)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}
func (p *nativeKVMPeer) turn(t *testing.T, ctx context.Context, index int, generation, lease int64) nativeKVMResult {
	t.Helper()
	input, _ := json.Marshal(fmt.Sprintf("unique input %d", index))
	command := &agentv1.GuestCommand{Identity: p.grant.Identity, DeliveryId: fmt.Sprintf("dispatch-%d", index), Command: &agentv1.GuestCommand_Dispatch{Dispatch: &agentv1.TurnDispatch{TurnId: fmt.Sprintf("%s-turn-%d", p.grant.Identity.SessionId, index), Sequence: int64(index), InputJson: input, SourceJson: []byte(`{"kind":"api"}`)}}}
	if err := p.connection.SendCommand(ctx, command); err != nil {
		t.Fatal(err)
	}
	var result nativeKVMResult
	select {
	case result = <-p.results:
	case <-p.done:
		t.Fatalf("native turn: %v", p.serveErr)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	select {
	case request := <-p.mcp:
		if request.AuthorityGeneration != generation || request.Session.ComputerLeaseEpoch != lease {
			t.Fatalf("native MCP authority: %+v", request)
		}
	default:
		t.Fatal("native MCP call missing")
	}
	if result.Count != index || result.PID <= 0 || result.NativePID <= 0 || result.Nonce == "" {
		t.Fatalf("native result: %+v", result)
	}
	return result
}

// This owner is explicitly a fixture; it does not certify durable CP adoption.
type nativeKVMContinuation struct {
	machine vm.GuestControlMachine
	peers   []*nativeKVMPeer
}

func (o *nativeKVMContinuation) ValidateTarget(_ context.Context, _ *agentv1.ComputerSessionInstallation, receipt *agentv1.ComputerSessionReceipt) error {
	if !receipt.GetFrozen() || receipt.GetInstalled() {
		return errors.New("fixture target is not a fresh frozen image")
	}
	return nil
}
func (o *nativeKVMContinuation) AttachSessions(ctx context.Context, installation *agentv1.ComputerSessionInstallation) error {
	for i, peer := range o.peers {
		peer.grant = proto.Clone(installation.Grants[i]).(*agentv1.SessionGrant)
		if err := peer.attach(ctx, o.machine, 2, false); err != nil {
			return err
		}
	}
	return nil
}
func (*nativeKVMContinuation) PrepareActivation(_ context.Context, installation *agentv1.ComputerSessionInstallation, receipt *agentv1.ComputerSessionReceipt) error {
	if !receipt.GetInstalled() || receipt.GetDesiredVersion() != installation.GetDesiredVersion() {
		return errors.New("installed receipt required")
	}
	return nil
}
func (*nativeKVMContinuation) CurrentControls(_ context.Context, p *agentv1.ComputerSessionInstallation, _ *agentv1.ComputerSessionReceipt) (*agentv1.ComputerSessionControls, error) {
	controls := &agentv1.ComputerSessionControls{Envelope: proto.Clone(p.Envelope).(*computerv0.ComputerOperationEnvelope), CheckpointId: p.Capture.CheckpointId, DesiredVersion: p.DesiredVersion}
	stopped := map[string]bool{}
	for _, id := range p.StoppedSessions {
		stopped[id.SessionId] = true
	}
	for _, grant := range p.Grants {
		controls.Sessions = append(controls.Sessions, &agentv1.SessionContinuationControl{Identity: proto.Clone(grant.Identity).(*agentv1.SessionIdentity), AuthorityGeneration: grant.AuthorityGeneration, Held: stopped[grant.Identity.SessionId], Stopped: stopped[grant.Identity.SessionId]})
	}
	return controls, nil
}

// Both fixture Sessions stay runnable throughout the restore.
func (*nativeKVMContinuation) ReconcileControls(context.Context, *agentv1.ComputerSessionInstallation, *agentv1.ComputerSessionReceipt) error {
	return nil
}

func TestNativeKVMPeerReplaysOriginalReceiptOnce(t *testing.T) {
	peer := newNativeKVMPeer(nil)
	original := workerapi.AgentOperationRequest{Session: workerapi.RuntimeSession{SessionID: "codex", ProcessEpoch: 1, ComputerLeaseEpoch: 2}, RequestID: "retained-call", AuthorityGeneration: 3, Method: int32(agentv1.Operation_METHOD_RUNTIME_MCP), Payload: json.RawMessage(`{"tool":"enqueue","arguments":{"idempotencyKey":"codex-1"}}`)}
	first, err := peer.AgentOperation(t.Context(), original)
	nativeKVMMust(t, err)
	replay, err := peer.AgentOperation(t.Context(), original)
	nativeKVMMust(t, err)
	if !bytes.Equal(first.Value, replay.Value) || len(peer.mcp) != 1 {
		t.Fatal("replayed operation repeated native notification or changed receipt")
	}
	// A different test process receives only remote fixture metadata. Replaying
	// the retained call after that boundary must return the original receipt.
	saved, err := json.Marshal(nativeKVMSavedPeer{Grant: peer.grant, Receipts: peer.receipts, Completed: 1})
	nativeKVMMust(t, err)
	var restored nativeKVMSavedPeer
	nativeKVMMust(t, json.Unmarshal(saved, &restored))
	replacement := newNativeKVMPeer(restored.Grant)
	replacement.receipts, replacement.completed = restored.Receipts, restored.Completed
	replay, err = replacement.AgentOperation(t.Context(), original)
	nativeKVMMust(t, err)
	if !bytes.Equal(first.Value, replay.Value) || len(replacement.mcp) != 0 || replacement.completed != 1 {
		t.Fatal("remote fixture receipt replay repeated work or lost completion")
	}
	changed := original
	changed.AuthorityGeneration++
	if _, err := peer.AgentOperation(t.Context(), changed); err == nil {
		t.Fatal("replay changed original authority")
	}
	changed = original
	changed.Payload = json.RawMessage(`{"tool":"enqueue","arguments":{"idempotencyKey":"different"}}`)
	if _, err := peer.AgentOperation(t.Context(), changed); err == nil {
		t.Fatal("replay changed original payload")
	}
}

func (p *nativeKVMPeer) WriteArtifact(ctx context.Context, d *agentv1.SessionProgramArtifact, w io.Writer) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	path, ok := p.programFiles[d.Digest]
	if !ok {
		return errors.New("unknown native fixture Program")
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = io.CopyN(w, file, d.SizeBytes)
	return err
}
func (p *nativeKVMPeer) ReleaseStart(ctx context.Context) (*agentv1.SessionGrant, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return proto.Clone(p.grant).(*agentv1.SessionGrant), nil
}
