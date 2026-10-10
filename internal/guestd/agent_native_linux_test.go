//go:build linux

package guestd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/artifact"
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/frameio"
	agentv1 "github.com/helmrdotdev/helmr/internal/proto/agent/v1"
	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"
	productversion "github.com/helmrdotdev/helmr/internal/version"
	"google.golang.org/protobuf/proto"
)

// This disposable Linux test uses real managed Node/native processes and the
// retained Session protocol. Its local operation responder is not CP evidence;
// VM serialization and remote restore require the separate KVM qualification.
func TestAgentNativeComputerContinuation(t *testing.T) {
	root, entry, registry, baseGrant, baseStart := nativeComputerFixture(t)
	program := nativeProgramInput(t, baseGrant, baseStart)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	var peers []*nativeContinuationPeer
	for _, provider := range []string{"codex", "claude"} {
		grant := proto.Clone(baseGrant).(*agentv1.SessionGrant)
		grant.Identity.SessionId = provider
		start := proto.Clone(baseStart).(*agentv1.SessionStart)
		start.AgentId = provider
		input := *program
		input.grant = grant
		relay, err := registry.openAgentSession(ctx, &agentv1.SessionAttach{Grant: grant, Start: start, AttachmentSequence: 1}, &input)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { relay.finish(context.Canceled); <-relay.finished })
		peer := &nativeContinuationPeer{ctx: ctx, relay: relay, identity: grant.Identity, results: make(chan json.RawMessage, 4), errors: make(chan nativeContinuationError, 4), ready: make(chan struct{}), mcp: make(chan *agentv1.GuestSessionMessage, 4)}
		peer.attach(t, grant, 1)
		peers = append(peers, peer)
	}
	for _, peer := range peers {
		select {
		case <-peer.ready:
		case err := <-peer.errors:
			t.Fatal(err.err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		peer.dispatch(t, 1)
	}
	first := make([]nativeContinuationResult, len(peers))
	for i, peer := range peers {
		first[i] = peer.result(t, ctx)
		peer.assertMCP(t, 3)
		if first[i].Count != 1 || first[i].NativePID <= 0 {
			t.Fatalf("initial: %+v", first[i])
		}
	}
	for _, peer := range peers {
		waitAgentRelay(t, peer.relay, func() bool { return len(peer.relay.outbox) == 0 })
	}
	capture := &agentv1.ComputerSessionCapture{Envelope: &computerv0.ComputerOperationEnvelope{OperationId: "native-capture", ComputerId: entry.computerID, ComputerInstanceId: entry.computerInstanceID, WriterGeneration: uint64(entry.writerGeneration), ChannelCredential: entry.channelCredential, OperationExpiresAtUnixNano: time.Now().Add(time.Minute).UnixNano()}, CheckpointId: "native-checkpoint", DesiredVersion: 1, MembershipRevision: 2}
	for _, peer := range peers {
		capture.Sessions = append(capture.Sessions, peer.identity)
	}
	if receipt, err := registry.captureAgentComputer(ctx, capture); err != nil || !receipt.GetFrozen() {
		t.Fatalf("capture: %v %v", receipt, err)
	}
	for i, peer := range peers {
		process := peer.relay.session.process.(*agentProcess)
		assertNativeFrozen(t, process.group, process.cmd.Process.Pid)
		func() {
			process.launcher.mu.Lock()
			defer process.launcher.mu.Unlock()
			if len(process.launcher.launches) != 1 {
				t.Fatalf("want one retained native launch, got %d", len(process.launcher.launches))
			}
			for _, launch := range process.launcher.launches {
				if !launch.started || launch.command == nil || launch.command.Process == nil {
					t.Fatal("native launch not started")
				}
				assertNativeFrozen(t, launch.group, launch.command.Process.Pid)
				if launch.command.Process.Pid != first[i].NativePID {
					t.Fatalf("reported native PID %d does not identify frozen launch %d", first[i].NativePID, launch.command.Process.Pid)
				}
			}
		}()
	}
	// Source-abort keeps physical ownership; VM restore is a separate proof.
	installation := agentComputerInstallation(registry, capture, true)
	if _, err := registry.installAgentComputer(ctx, installation, false, testComputerAuthorityClock()); err != nil {
		t.Fatal(err)
	}
	applyAgentComputerTestControls(t, registry)
	for i, peer := range peers {
		peer.attach(t, installation.Grants[i], 2)
	}
	if receipt, err := registry.installAgentComputer(ctx, installation, true, testComputerAuthorityClock()); err != nil || !receipt.GetActivated() {
		t.Fatalf("activate: %v %v", receipt, err)
	}
	for _, peer := range peers {
		peer.dispatch(t, 2)
	}
	for i, peer := range peers {
		second := peer.result(t, ctx)
		peer.assertMCP(t, 4)
		if second.Count != 2 || second.PID != first[i].PID || second.NativePID != first[i].NativePID || second.Nonce != first[i].Nonce {
			t.Fatalf("continuation first=%+v second=%+v", first[i], second)
		}
		t.Logf("%s retained managed PID=%d native PID=%d setup nonce=%s", peer.identity.SessionId, second.PID, second.NativePID, second.Nonce)
	}
	// Ordinary commands remain independent of customer bundle startup. Keep
	// both native continuations resident, with one explicitly held, while making
	// the bundle unavailable to any fresh Session process.
	held := peers[0].relay.session
	if err := held.send(&agentv1.GuestCommand{Identity: peers[0].identity, DeliveryId: "command-hold", ControlSequence: 10, Command: &agentv1.GuestCommand_Suspend{Suspend: &agentv1.SessionSuspend{Reason: "hold"}}}); err != nil {
		t.Fatal(err)
	}
	func() {
		const artifact = "/var/lib/helmr/program/artifact/helmr/definition-index.json"
		const unavailable = "/var/lib/helmr/program/artifact/helmr/agents.unavailable"
		if err := os.Rename(artifact, unavailable); err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := os.Rename(unavailable, artifact); err != nil {
				t.Error(err)
			}
		}()
		broken, err := newAgentProcess(ctx, entry, agentProcessOptions{Program: bootProgramMounts(), Identity: &agentv1.SessionIdentity{SessionId: "unavailable-bundle", ProcessEpoch: 1}})
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := broken.close(context.Background()); err != nil {
				t.Error(err)
			}
			closeAgentFiles(broken.stdout, broken.stderr)
		}()
		if err := broken.start(ctx); err != nil {
			t.Fatal(err)
		}
		if err := broken.write(ctx, &agentv1.GuestCommand{Identity: broken.identity, Command: &agentv1.GuestCommand_Start{Start: peers[0].relay.session.start}}); err != nil {
			t.Fatal(err)
		}
		if err := broken.events.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
			t.Fatal(err)
		}
		failed, err := broken.read()
		if err != nil || failed.GetFailed() == nil || !strings.Contains(failed.GetFailed().GetMessage(), "definition-index.json") {
			t.Fatalf("missing bundle must report loader failure: %v %v", failed, err)
		}
		body, err := json.Marshal(computerBasicExecSpec{Command: []string{"/bin/sh", "-ceu", "printf retained-command > /workspace/command-recovery"}, Cwd: "/workspace", TimeoutMS: 10000})
		if err != nil {
			t.Fatal(err)
		}
		authority := &computerv0.ComputerCommandAuthority{OperationId: "native-command", ComputerId: entry.computerID, ComputerInstanceId: entry.computerInstanceID, WriterGeneration: entry.writerGeneration, ChannelCredential: entry.channelCredential, RequestFingerprint: "native-command-write", OperationExpiresAtUnixNano: time.Now().Add(time.Minute).UnixNano()}
		result := framedBasicExec(t, ctx, registry, &computerv0.ComputerBasicExecRequest{LogLimits: &computerv0.CommandLogLimits{ChunkBytes: 65536, BufferBytes: 1048576, BufferRecords: 64}, Envelope: authority, RequestJson: string(body)})
		if result.GetOutcome() != "exited" || result.GetExitCode() != 0 {
			t.Fatalf("command with unavailable bundle: %v", result)
		}
		if err := registry.releaseCommand(authority, false); err != nil {
			t.Fatal(err)
		}
		content, err := os.ReadFile(filepath.Join(entry.computerRoot, "command-recovery"))
		if err != nil || string(content) != "retained-command" {
			t.Fatalf("ordinary command write: %q %v", content, err)
		}
		for index, peer := range peers {
			peer.relay.session.mu.Lock()
			preserved := peer.relay.session.held == (index == 0) && !peer.relay.session.terminal
			peer.relay.session.mu.Unlock()
			if !preserved {
				t.Fatal("command changed peer hold or lifecycle")
			}
		}
	}()
	// Only this explicit control releases the hold. Both native clients must
	// continue the same setup result and process after the independent command.
	if err := held.send(&agentv1.GuestCommand{Identity: peers[0].identity, DeliveryId: "command-resume", ControlSequence: 11, Command: &agentv1.GuestCommand_Resume{Resume: &agentv1.SessionResume{}}}); err != nil {
		t.Fatal(err)
	}
	waitAgentRelay(t, peers[0].relay, func() bool { held.mu.Lock(); defer held.mu.Unlock(); return !held.held && held.pendingResume == "" })
	for _, peer := range peers {
		peer.dispatch(t, 3)
	}
	for index, peer := range peers {
		third := peer.result(t, ctx)
		peer.assertMCP(t, 4)
		if third.Count != 3 || third.PID != first[index].PID || third.NativePID != first[index].NativePID || third.Nonce != first[index].Nonce {
			t.Fatalf("command changed native continuation: first=%+v after=%+v", first[index], third)
		}
	}
	raw, err := os.ReadFile(filepath.Join(root, "workspace/native-models.json"))
	if err != nil {
		t.Fatal(err)
	}
	var endpoints struct{ Proof int }
	if err := json.Unmarshal(raw, &endpoints); err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Timeout: 5 * time.Second}
	response, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/", endpoints.Proof))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var history map[string][]json.RawMessage
	if err := json.NewDecoder(response.Body).Decode(&history); err != nil {
		t.Fatal(err)
	}
	for _, provider := range []string{"codex", "claude"} {
		requests := history[provider]
		if len(requests) < 2 {
			t.Fatalf("missing %s model requests", provider)
		}
		last := string(requests[len(requests)-1])
		if !strings.Contains(last, "unique input 1") || !strings.Contains(last, "unique input 2") || !strings.Contains(last, "fixture-tool-turn-"+provider+"-3") {
			t.Fatalf("%s native history omitted retained input: %s", provider, last)
		}
	}

}

func nativeComputerFixture(t *testing.T) (string, *computerMountEntry, *computerOperationRegistry, *agentv1.SessionGrant, *agentv1.SessionStart) {
	t.Helper()
	root := os.Getenv("HELMR_NATIVE_COMPUTER_ROOT")
	if root == "" {
		t.Skip("requires prepared disposable native Computer root")
	}
	if os.Getenv("HELMR_PRIVILEGED_PROGRAM_TEST") != "1" {
		t.Skip("requires disposable privileged Linux")
	}
	if os.Geteuid() != 0 {
		t.Fatal("native continuation requires root")
	}
	if _, err := os.Stat(processCgroupRoot); errors.Is(err, os.ErrNotExist) {
		prepareProgramTestCgroup(t)
	} else if err != nil {
		t.Fatal(err)
	}
	flags, err := artifact.NodeProgramFlags(productversion.Node())
	if err != nil {
		t.Fatal(err)
	}
	// Namespace qualification uses the local Linux CPU. Release artifact
	// architecture (x86_64) is separately checked in the KVM qualification.
	metadata, err := artifact.CanonicalRuntimeMetadata(artifact.RuntimeMetadata{ModulePolicyDigest: "sha256:" + strings.Repeat("a", 64), Architecture: definition.RuntimeArchitecture("x86_64"), FormatVersion: artifact.RuntimeMetadataFormatVersion, NodeVersion: productversion.Node(), ProgramNodeFlags: flags, RuntimeContract: definition.RuntimeContract})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(managedRuntimeMetadata, metadata, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "workspace"), 0777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(root, "workspace"), 0777); err != nil {
		t.Fatal(err)
	}
	fixture, _ := agentSessionFixture(t)
	entry := fixture.entry
	entry.imageRoot, entry.computerMount = root, defaultRuntimeWorkdir
	entry.computerRoot = filepath.Join(root, "workspace")
	entry.runtimeUser = &resolvedRuntimeUser{UID: 1001, GID: 1001, Home: defaultRuntimeWorkdir}
	entry.baseComputerDiskVersionID = "disk-v1"
	registry := newComputerOperationRegistry()
	registry.setWallClock = func(time.Time) error { return nil }
	registry.entries[entry.computerInstanceID] = entry
	// There is no block device in this namespace qualification. Real writeback is
	// covered by the KVM path; this test only qualifies process continuation.
	registry.writeback = &computerWriteback{syncFilesystem: func() error { return nil }}
	return root, entry, registry, fixture.grant, fixture.start
}

type nativeContinuationResult struct {
	Count     int
	PID       int
	NativePID int
	Nonce     string
}
type nativeContinuationError struct {
	sequence uint64
	err      error
}

type nativeContinuationPeer struct {
	ctx        context.Context
	attachment atomic.Uint64
	relay      *agentRelay
	identity   *agentv1.SessionIdentity
	results    chan json.RawMessage
	errors     chan nativeContinuationError
	ready      chan struct{}
	mcp        chan *agentv1.GuestSessionMessage
	readyOnce  sync.Once
}

func (p *nativeContinuationPeer) attach(t *testing.T, grant *agentv1.SessionGrant, sequence uint64) {
	t.Helper()
	p.attachment.Store(sequence)
	host := attachAgentRelay(t, p.relay, sequence, grant)
	_ = host.SetDeadline(time.Time{})
	go func() {
		if err := p.serve(host, sequence); err != nil && p.attachment.Load() == sequence && p.ctx.Err() == nil {
			select {
			case p.errors <- nativeContinuationError{sequence, err}:
			default:
			}
		}
	}()
}
func (p *nativeContinuationPeer) serve(host net.Conn, sequence uint64) error {
	ctx, cancel := context.WithCancel(p.ctx)
	incoming := make(chan *agentv1.GuestSessionMessage, 1)
	readErrors := make(chan error, 1)
	joined := make(chan struct{})
	go func() {
		defer close(joined)
		for {
			envelope := new(agentv1.GuestSessionMessage)
			if err := frameio.ReadProtoFrameBounded(host, maxAgentTransportFrameBytes, envelope); err != nil {
				readErrors <- err
				return
			}
			select {
			case incoming <- envelope:
			case <-ctx.Done():
				return
			}
		}
	}()
	defer func() { cancel(); _ = host.Close(); <-joined }()
	var outcome json.RawMessage
	for {
		var envelope *agentv1.GuestSessionMessage
		select {
		case envelope = <-incoming:
		case err := <-readErrors:
			return fmt.Errorf("%s host read: %w", p.identity.SessionId, err)
		case <-ctx.Done():
			return ctx.Err()
		}
		event := envelope.GetEvent()
		if event.GetReady() != nil {
			p.readyOnce.Do(func() { close(p.ready) })
		}
		if event.GetFailed() != nil {
			return fmt.Errorf("Session %s failed: %v", p.identity.SessionId, event.GetFailed())
		}
		if log := envelope.GetLog(); log != nil {
			fmt.Fprintf(os.Stderr, "%s: %s", p.identity.SessionId, log.GetData())
			if err := frameio.WriteProtoFrame(host, &agentv1.HostSessionMessage{AttachmentSequence: sequence, Message: &agentv1.HostSessionMessage_LogAcknowledged{LogAcknowledged: &agentv1.SessionLogAcknowledged{Stream: log.GetStream(), ThroughSequence: log.GetThroughSequence()}}}); err != nil {
				return err
			}
		}
		if operation := event.GetOperation(); operation != nil {
			value := json.RawMessage("null")
			switch operation.Method {
			case agentv1.Operation_METHOD_REGISTER_MESSAGES, agentv1.Operation_METHOD_CLOSE_PROCESSING, agentv1.Operation_METHOD_HOLD, agentv1.Operation_METHOD_RESPOND:
			case agentv1.Operation_METHOD_ASK:
				value = json.RawMessage(`{"id":"fixture-approval"}`)
			case agentv1.Operation_METHOD_WAIT_ASK:
				value = json.RawMessage(`{"answer":{"selected":[{"id":"allow","value":true}]},"respondedBy":{"kind":"api_key","id":"fixture-operator"}}`)
			case agentv1.Operation_METHOD_RUNTIME_MCP:
				if operation.TurnId != nil {
					return fmt.Errorf("MCP inferred current Turn")
				}
				var call struct {
					Tool      string
					Arguments struct{ IdempotencyKey string }
				}
				if err := json.Unmarshal(operation.PayloadJson, &call); err != nil {
					return err
				}
				if call.Tool != "enqueue" || call.Arguments.IdempotencyKey == "" {
					return fmt.Errorf("unexpected MCP call: %s", operation.PayloadJson)
				}
				p.mcp <- envelope
				value, _ = json.Marshal(map[string]any{"sessionId": "peer", "turnId": "fixture-tool-turn-" + call.Arguments.IdempotencyKey, "sequence": 1})
			case agentv1.Operation_METHOD_OUTPUT:
				value = json.RawMessage(`{"sequence":1}`)
			case agentv1.Operation_METHOD_FINALIZE:
				var payload struct{ Result json.RawMessage }
				if err := json.Unmarshal(operation.PayloadJson, &payload); err != nil {
					return err
				}
				outcome = payload.Result
				value, _ = json.Marshal(map[string]any{"status": "completed", "result": payload.Result})
			default:
				return fmt.Errorf("unexpected native operation: %v", operation)
			}
			command := &agentv1.GuestCommand{Identity: p.identity, Command: &agentv1.GuestCommand_OperationResult{OperationResult: &agentv1.OperationResult{RequestId: operation.RequestId, Outcome: &agentv1.OperationResult_ValueJson{ValueJson: value}}}}
			if err := frameio.WriteProtoFrame(host, &agentv1.HostSessionMessage{AttachmentSequence: sequence, Message: &agentv1.HostSessionMessage_Command{Command: command}}); err != nil {
				return err
			}
		}
		if envelope.EventSequence > 0 {
			if err := frameio.WriteProtoFrame(host, &agentv1.HostSessionMessage{AttachmentSequence: sequence, Message: &agentv1.HostSessionMessage_Acknowledged{Acknowledged: &agentv1.SessionEventsAcknowledged{ThroughSequence: envelope.EventSequence}}}); err != nil {
				return err
			}
		}
		if delivery := event.GetDeliveryResult(); delivery != nil {
			if delivery.GetError() != nil {
				return fmt.Errorf("native dispatch: %v", delivery.GetError())
			}
			if outcome != nil {
				p.results <- outcome
				outcome = nil
			}
		}
	}
}
func (p *nativeContinuationPeer) dispatch(t *testing.T, index int) {
	t.Helper()
	text, _ := json.Marshal(fmt.Sprintf("unique input %d", index))
	input, _ := json.Marshal([]map[string]string{{"type": "text", "text": string(text)}})
	if err := p.relay.session.send(&agentv1.GuestCommand{Identity: p.identity, DeliveryId: fmt.Sprintf("dispatch-%d", index), Command: &agentv1.GuestCommand_Dispatch{Dispatch: &agentv1.TurnDispatch{TurnId: fmt.Sprintf("%s-turn-%d", p.identity.SessionId, index), Sequence: int64(index), InputJson: input, SourceJson: []byte(`{"kind":"api"}`)}}}); err != nil {
		t.Fatal(err)
	}
}
func (p *nativeContinuationPeer) result(t *testing.T, ctx context.Context) nativeContinuationResult {
	t.Helper()
	for {
		select {
		case raw := <-p.results:
			var result nativeContinuationResult
			if err := json.Unmarshal(raw, &result); err != nil {
				t.Fatal(err)
			}
			return result
		case failure := <-p.errors:
			if failure.sequence == p.attachment.Load() {
				t.Fatal(failure.err)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
}

func (p *nativeContinuationPeer) assertMCP(t *testing.T, generation int64) {
	t.Helper()
	select {
	case request := <-p.mcp:
		if request.OperationAuthorityGeneration != generation || request.OperationLeaseEpoch != 2 {
			t.Fatalf("MCP generation=%d want=%d lease=%d want=2", request.OperationAuthorityGeneration, generation, request.OperationLeaseEpoch)
		}
	default:
		t.Fatal("native client did not invoke managed MCP")
	}
}

func assertNativeFrozen(t *testing.T, group *linuxProcessCgroup, pid int) {
	t.Helper()
	events, err := os.ReadFile(filepath.Join(group.path, "cgroup.events"))
	if err != nil || !strings.Contains(string(events), "frozen 1") {
		t.Fatalf("cgroup not frozen: %s %v", events, err)
	}
	processes, err := os.ReadFile(filepath.Join(group.path, "cgroup.procs"))
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range strings.Fields(string(processes)) {
		if value == strconv.Itoa(pid) {
			return
		}
	}
	t.Fatalf("PID %d outside frozen cgroup: %s", pid, processes)
}
