//go:build linux

package guestd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/helmrdotdev/helmr/internal/artifact"
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/frameio"
	agentv1 "github.com/helmrdotdev/helmr/internal/proto/agent/v1"
	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"
	productversion "github.com/helmrdotdev/helmr/internal/version"
	"golang.org/x/sys/unix"
	"google.golang.org/protobuf/proto"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestSessionCgroupDuplicateStartupPreservesRunningEpoch(t *testing.T) {
	nativeAuthFixture(t) // Establish the privileged cgroup environment.
	group, err := createSessionProcessCgroup(t.Name(), 1)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = group.kill(); _ = group.waitEmpty(); _ = group.close() })
	child := exec.Command("/bin/sleep", "30")
	fd := -1
	child.SysProcAttr = &syscall.SysProcAttr{PidFD: &fd}
	if err := group.attach(child); err != nil {
		t.Fatal(err)
	}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = child.Process.Kill(); _ = child.Wait(); _ = unix.Close(fd) })
	if _, err := createSessionProcessCgroup(t.Name(), 1); err == nil {
		t.Fatal("duplicate epoch replaced its scope")
	}
	if err := liveNativePID(fd); err != nil {
		t.Fatalf("duplicate startup killed the existing epoch: %v", err)
	}
	other, err := createSessionProcessCgroup(t.Name(), 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := other.close(); err != nil {
		t.Fatal(err)
	}
}

func TestAgentProcessBlockedPipeSupportsCancellationWithoutHoldingAuthority(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close()
	session, _ := agentSessionFixture(t)
	session.clock = time.Now
	session.grant.ExpiresAtUnixNano = time.Now().Add(100 * time.Millisecond).UnixNano()
	process := &agentProcess{stdin: writer, identity: session.grant.Identity}
	session.process = process
	result := make(chan error, 1)
	go func() {
		result <- session.send(&agentv1.GuestCommand{Identity: session.grant.Identity, DeliveryId: "blocked", Command: &agentv1.GuestCommand_Dispatch{Dispatch: &agentv1.TurnDispatch{TurnId: "turn", Sequence: 1, InputJson: make([]byte, 1024*1024)}}})
	}()
	time.Sleep(150 * time.Millisecond)
	unlocked := make(chan error, 1)
	go func() { session.mu.Lock(); err := session.authorizeLocked(); session.mu.Unlock(); unlocked <- err }()
	select {
	case err := <-unlocked:
		if !errors.Is(err, errSessionGrantExpired) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("blocked writer held authority lock")
	}
	select {
	case err := <-result:
		t.Fatalf("authority expiry destroyed retained pipe: %v", err)
	default:
	}
	_ = writer.Close()
	select {
	case err := <-result:
		var loss *agentPipeWriteError
		if !errors.As(err, &loss) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("physical close did not unblock pipe")
	}
	// Cancellation is independent of the longer fixed write timeout.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := process.write(ctx, &agentv1.GuestCommand{Identity: session.grant.Identity}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled write: %v", err)
	}
}

func TestAgentProcessExecutesRetainedRuntimeAndFreezes(t *testing.T) {
	testAgentProcessRetainedRuntime(t, false, false, false)
}
func TestAgentProcessCheckpointRetainsRuntime(t *testing.T) {
	testAgentProcessRetainedRuntime(t, true, false, false)
}
func TestAgentComputerCheckpointRetainsTwoActualProcesses(t *testing.T) {
	testAgentProcessRetainedRuntime(t, true, true, false)
}
func TestAgentComputerStopsCaptureFrozenActualProcess(t *testing.T) {
	testAgentProcessRetainedRuntime(t, true, false, true)
}
func testAgentProcessRetainedRuntime(t *testing.T, checkpoint, computerCapture, stopCaptured bool) {
	assets := os.Getenv("HELMR_AGENT_TEST_RUNTIME")
	if assets == "" {
		t.Skip("requires built Agent runtime assets in disposable Linux guest")
	}
	nativeAuthFixture(t)
	root := t.TempDir()
	if err := os.Chmod(root, 0755); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{"workspace", "etc", "tmp", "run", "opt"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0777); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(filepath.Join(root, "workspace"), 0777); err != nil {
		t.Fatal(err)
	}
	resolver, err := os.OpenFile(imageRuntimeResolverPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if err == nil {
		_, _ = resolver.WriteString("nameserver 127.0.0.1\n")
		_ = resolver.Close()
		t.Cleanup(func() { _ = os.Remove(imageRuntimeResolverPath) })
	} else if !errors.Is(err, os.ErrExist) {
		t.Fatal(err)
	}
	copyFile := func(source, target string) {
		if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
			t.Fatal(err)
		}
		input, err := os.Open(source)
		if err != nil {
			t.Fatal(err)
		}
		output, err := os.CreateTemp(filepath.Dir(target), ".runtime-fixture-*")
		if err != nil {
			_ = input.Close()
			t.Fatal(err)
		}
		defer os.Remove(output.Name())
		_, copyErr := io.Copy(output, input)
		modeErr := output.Chmod(0755)
		if err := errors.Join(copyErr, modeErr, input.Close(), output.Close()); err != nil {
			t.Fatal(err)
		}
		// Each case installs a new immutable inode, as runtime artifacts do.
		if err := os.Rename(output.Name(), target); err != nil {
			t.Fatal(err)
		}
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatal(err)
	}
	libs, err := exec.Command("ldd", node).Output()
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range strings.Fields(string(libs)) {
		if filepath.IsAbs(field) {
			copyFile(field, filepath.Join(root, field))
		}
	}
	runtimeRoot := "/var/lib/helmr/program/runtime"
	programRoot := "/var/lib/helmr/program/artifact"
	copyFile(node, filepath.Join(runtimeRoot, "bin/node"))
	for _, name := range []string{"entry.mjs", "module-preload.mjs"} {
		copyFile(filepath.Join(assets, name), filepath.Join(runtimeRoot, "helmr", name))
	}
	write := func(path string, body []byte) {
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, body, 0644); err != nil {
			t.Fatal(err)
		}
	}
	flags, err := artifact.NodeProgramFlags(productversion.Node())
	if err != nil {
		t.Fatal(err)
	}
	// This test exercises Linux execution mechanisms, not release artifact
	// architecture verification (which is qualified separately in the VM).
	architecture := definition.RuntimeArchitecture("x86_64")
	metadata, err := artifact.CanonicalRuntimeMetadata(artifact.RuntimeMetadata{ModulePolicyDigest: "sha256:" + strings.Repeat("a", 64), Architecture: architecture, FormatVersion: artifact.RuntimeMetadataFormatVersion, NodeVersion: productversion.Node(), ProgramNodeFlags: flags, RuntimeContract: definition.RuntimeContract})
	if err != nil {
		t.Fatal(err)
	}
	write(managedRuntimeMetadata, metadata)
	write(filepath.Join(programRoot, "package.json"), []byte(`{"type":"module"}`))
	write(filepath.Join(programRoot, "helmr/definition-index.json"), []byte(`{"apiVersion":"helmr.definition-index.v1","agents":[{"id":"agent","computerDefinitionId":"computer","modulePath":"helmr/app/entry-0.mjs","exportName":"agent"}],"computers":[{"id":"computer","modulePath":"helmr/app/entry-0.mjs","exportName":"agent","throughAgent":true}]}`))
	if checkpoint {
		write(filepath.Join(programRoot, "helmr/app/entry-0.mjs"), []byte(`export const agent={kind:"agent",id:"agent",computer:{kind:"computer",id:"computer"},setup(){return {count:0}},turn(turn,ctx){return {count:++ctx.setupResult.count,pid:process.pid}}}`))
	} else {
		write(filepath.Join(programRoot, "helmr/app/entry-0.mjs"), []byte(`export const agent={kind:"agent",id:"agent",computer:{kind:"computer",id:"computer"},setup(){setInterval(()=>{},1000);console.log("setup-log:"+"x".repeat(70*1024));return {count:0}},turn(turn,ctx){return {count:++ctx.setupResult.count,pid:process.pid}}}`))
	}

	fixture, _ := agentSessionFixture(t)
	fixture.entry.imageRoot = root
	fixture.entry.computerMount = defaultRuntimeWorkdir
	fixture.entry.runtimeUser = &resolvedRuntimeUser{UID: 1001, GID: 1001, Home: defaultRuntimeWorkdir}
	process, err := newAgentProcess(t.Context(), fixture.entry, agentProcessOptions{Program: bootProgramMounts(), Identity: fixture.grant.Identity})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		defer closeAgentFiles(process.stdout, process.stderr)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := process.close(ctx); err != nil {
			t.Error(err)
		}
	})
	if err := process.start(t.Context()); err != nil {
		t.Fatal(err)
	}
	session, err := newAgentSession(fixture.entry, fixture.grant, fixture.start, process, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if err := session.send(&agentv1.GuestCommand{Identity: session.grant.Identity, Command: &agentv1.GuestCommand_Start{Start: session.start}}); err != nil {
		t.Fatal(err)
	}
	read := func() *agentv1.ProgramEvent {
		t.Helper()
		_ = process.events.SetReadDeadline(time.Now().Add(5 * time.Second))
		event, err := process.read()
		if err != nil {
			t.Fatal(err)
		}
		return event
	}
	event := read()
	if event.GetReady() == nil {
		t.Fatalf("expected ready: %v", event)
	}
	if _, _, err := session.receive(event); err != nil {
		t.Fatal(err)
	}
	if err := process.converge(nil); err != nil {
		t.Fatal(err)
	}
	rootPID := process.cmd.Process.Pid
	guestPID := 0
	for index := 1; index <= 2; index++ {
		if index == 2 && computerCapture {
			actualAgentComputerContinuation(t, session, process, read)
		} else if index == 2 && checkpoint {
			relay := newAgentRelay(t.Context(), session, nil)
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			prepared := make(chan error, 1)
			go func() { prepared <- relay.prepareCapture(ctx, "actual-runtime-checkpoint") }()
			if _, _, err := session.receive(read()); err != nil {
				t.Fatal(err)
			}
			relay.mu.Lock()
			relay.notifyLocked()
			relay.mu.Unlock()
			if err := <-prepared; err != nil {
				t.Fatal(err)
			}
			if err := relay.freezeForCapture(ctx); err != nil {
				t.Fatal(err)
			}
			if stopCaptured {
				if err := process.verifyFrozen(); err != nil {
					t.Fatal(err)
				}
				capture := &agentComputerCapture{members: []*agentRelay{relay}, stopped: map[*agentRelay]bool{relay: true}, activationStarted: true}
				if err := stopAgentComputerMembers(capture); err != nil {
					t.Fatal(err)
				}
				if !session.physicalClosed || !process.closed {
					t.Fatal("frozen Session did not physically close")
				}
				if _, err := os.Stat(process.group.path); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("Session cgroup was not empty and removed: %v", err)
				}
				select {
				case <-process.waitDone:
				default:
					t.Fatal("frozen runtime was not joined")
				}
				return
			}
			host := attachAgentRelay(t, relay, 1, session.grant)
			if err := relay.activateAfterCapture(ctx); err != nil {
				t.Fatal(err)
			}
			_ = host.Close()
		} else if index == 2 {
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			if err := process.freeze(ctx); err != nil {
				t.Fatal(err)
			}
			if err := process.thaw(ctx); err != nil {
				t.Fatal(err)
			}
			cancel()
		}
		id := fmt.Sprintf("turn-%d", index)
		guestDispatch(t, session, id, int64(index))
		for {
			event := read()
			envelope, reply, err := session.receive(event)
			if err != nil {
				t.Fatal(err)
			}
			if reply != nil {
				if err := session.writeReply(t.Context(), reply); err != nil {
					t.Fatal(err)
				}
			}
			if operation := envelope.GetEvent().GetOperation(); operation != nil {
				value := []byte("null")
				switch operation.GetMethod() {
				case agentv1.Operation_METHOD_CLOSE_PROCESSING:
				case agentv1.Operation_METHOD_FINALIZE:
					var payload struct {
						Result struct {
							Count int
							PID   int
						} `json:"result"`
					}
					if err := json.Unmarshal(operation.PayloadJson, &payload); err != nil {
						t.Fatal(err)
					}
					if index == 1 {
						guestPID = payload.Result.PID
					}
					if payload.Result.Count != index || payload.Result.PID != guestPID || process.cmd.Process.Pid != rootPID {
						t.Fatalf("lost retained setup state: %+v", payload)
					}
					value, _ = json.Marshal(map[string]any{"status": "completed", "result": payload.Result})
				default:
					t.Fatalf("unexpected operation: %v", operation)
				}
				if err := session.send(&agentv1.GuestCommand{Identity: session.grant.Identity, Command: &agentv1.GuestCommand_OperationResult{OperationResult: &agentv1.OperationResult{RequestId: operation.RequestId, Outcome: &agentv1.OperationResult_ValueJson{ValueJson: value}}}}); err != nil {
					t.Fatal(err)
				}
			}
			if event.GetDeliveryResult() != nil {
				if event.GetDeliveryResult().GetError() != nil {
					t.Fatal(event)
				}
				break
			}
		}
	}
	if checkpoint {
		return
	}
	// Keep a live authored timer and withhold the log acknowledgment. Shutdown
	// must still enforce physical close and drain kernel pipe tails before Stopped.
	if err := process.events.SetReadDeadline(time.Time{}); err != nil {
		t.Fatal(err)
	}
	relayCtx, cancelRelay := context.WithCancel(t.Context())
	defer cancelRelay()
	relay := newAgentRelay(relayCtx, session, nil)
	relay.run()
	host := attachAgentRelay(t, relay, 1, session.grant)
	_ = host.SetDeadline(time.Now().Add(15 * time.Second))
	var first agentv1.GuestSessionMessage
	if err := frameio.ReadProtoFrameBounded(host, maxAgentTransportFrameBytes, &first); err != nil {
		t.Fatal(err)
	}
	if first.GetLog() == nil {
		t.Fatalf("missing customer log: %v", &first)
	}
	logs := append([]byte(nil), first.GetLog().GetData()...)
	ackLog := func(log *agentv1.SessionLog) {
		t.Helper()
		if err := writeAgentTransportFrame(host, &agentv1.HostSessionMessage{AttachmentSequence: 1, Message: &agentv1.HostSessionMessage_LogAcknowledged{LogAcknowledged: &agentv1.SessionLogAcknowledged{Stream: log.GetStream(), ThroughSequence: log.GetThroughSequence()}}}); err != nil {
			t.Fatal(err)
		}
	}
	var ended [2]bool
	stopped := false
	shutdown := &agentv1.GuestCommand{Identity: session.grant.Identity, DeliveryId: "shutdown", Command: &agentv1.GuestCommand_Shutdown{Shutdown: &agentv1.SessionShutdown{Reason: "test"}}}
	if err := writeAgentTransportFrame(host, &agentv1.HostSessionMessage{AttachmentSequence: 1, Message: &agentv1.HostSessionMessage_Command{Command: shutdown}}); err != nil {
		t.Fatal(err)
	}
	for {
		var message agentv1.GuestSessionMessage
		if err := frameio.ReadProtoFrameBounded(host, maxAgentTransportFrameBytes, &message); err != nil {
			t.Fatal(err)
		}
		if log := message.GetLog(); log != nil {
			logs = append(logs, log.GetData()...)
			if log.GetKind() == agentv1.SessionLog_KIND_END {
				ended[int(log.GetStream())-1] = true
			}
			ackLog(log)
		}
		if message.GetStopped() != nil {
			stopped = true
			ackLog(first.GetLog())
		}
		if stopped && ended[0] && ended[1] {
			break
		}
		if message.GetEvent().GetFailed() != nil {
			t.Fatalf("clean shutdown reported process loss: %v", &message)
		}
	}
	if string(logs) != "setup-log:"+strings.Repeat("x", 70*1024)+"\n" {
		t.Fatalf("final logs truncated: %d bytes", len(logs))
	}
	if !process.closed {
		t.Fatal("physical owner remained open")
	}
	failedGrant := proto.Clone(session.grant).(*agentv1.SessionGrant)
	failedGrant.Identity.ProcessEpoch++
	failed, err := newAgentProcess(t.Context(), session.entry, agentProcessOptions{Program: bootProgramMounts(), Identity: failedGrant.Identity})
	if err != nil {
		t.Fatal(err)
	}
	failed.cmd.Path = "/missing-agent-test-executable"
	_, _ = failed.cmd.Stderr.(*os.File).WriteString("startup diagnostic\n")
	failedSession, err := newAgentSession(session.entry, failedGrant, session.start, failed, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	failedRelay := newAgentRelay(t.Context(), failedSession, nil)
	failedRelay.startLogs()
	startErr := failed.start(t.Context())
	if startErr == nil {
		t.Fatal("missing executable started")
	}
	failedRelay.finish(startErr)
	if _, err := failed.stderr.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("failed startup log reader leaked: %v", err)
	}
	diagnostic := ""
	for _, buffer := range failedSession.logs {
		for {
			record, ok := buffer.peek()
			if !ok {
				break
			}
			diagnostic += string(record.Data)
			if err := buffer.acknowledge(record.Through); err != nil {
				t.Fatal(err)
			}
		}
	}
	if diagnostic != "startup diagnostic\n" {
		t.Fatalf("failed startup diagnostic lost: %q", diagnostic)
	}

	// A real loader failure races normal Node exit against the private event
	// pipe. The authored diagnostic must survive the independent exit watcher.
	failedGrant = proto.Clone(failedGrant).(*agentv1.SessionGrant)
	failedGrant.Identity.ProcessEpoch++
	invalidStart := proto.Clone(session.start).(*agentv1.SessionStart)
	invalidStart.AgentId = "absent-agent"
	invalid, err := newAgentProcess(t.Context(), session.entry, agentProcessOptions{Program: bootProgramMounts(), Identity: failedGrant.Identity})
	if err != nil {
		t.Fatal(err)
	}
	invalidSession, err := newAgentSession(session.entry, failedGrant, invalidStart, invalid, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	closed := make(chan error, 1)
	invalidRelay := newAgentRelay(relayCtx, invalidSession, func(err error) { closed <- err })
	invalidRelay.startLogs()
	if err := invalid.start(t.Context()); err != nil {
		t.Fatal(err)
	}
	invalidRelay.run()
	if err := invalidSession.send(&agentv1.GuestCommand{Identity: failedGrant.Identity, Command: &agentv1.GuestCommand_Start{Start: invalidStart}}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("failed bundle did not close")
	}
	found := false
	invalidRelay.mu.Lock()
	for _, event := range invalidRelay.outbox {
		if strings.Contains(event.GetEvent().GetFailed().GetMessage(), "Agent is absent from the verified bundle") {
			found = true
		}
	}
	invalidRelay.mu.Unlock()
	if !found {
		t.Fatal("real Node exit discarded its authored loader diagnostic")
	}

}

func actualAgentComputerContinuation(t *testing.T, session *agentSession, process *agentProcess, read func() *agentv1.ProgramEvent) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	peerGrant := proto.Clone(session.grant).(*agentv1.SessionGrant)
	peerGrant.Identity.SessionId = "peer"
	peerProcess, err := newAgentProcess(ctx, session.entry, agentProcessOptions{Program: bootProgramMounts(), Identity: peerGrant.Identity})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		stopCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if err := peerProcess.close(stopCtx); err != nil {
			t.Error(err)
		}
		closeAgentFiles(peerProcess.stdout, peerProcess.stderr)
	}()
	if err := peerProcess.start(ctx); err != nil {
		t.Fatal(err)
	}
	peer, err := newAgentSession(session.entry, peerGrant, session.start, peerProcess, session.entry.authorityNow)
	if err != nil {
		t.Fatal(err)
	}
	if err := peer.send(&agentv1.GuestCommand{Identity: peerGrant.Identity, Command: &agentv1.GuestCommand_Start{Start: peer.start}}); err != nil {
		t.Fatal(err)
	}
	peerRead := func() *agentv1.ProgramEvent {
		t.Helper()
		_ = peerProcess.events.SetReadDeadline(time.Now().Add(5 * time.Second))
		event, err := peerProcess.read()
		if err != nil {
			t.Fatal(err)
		}
		return event
	}
	if event := peerRead(); event.GetReady() == nil {
		t.Fatal("peer setup did not finish")
	} else if _, _, err := peer.receive(event); err != nil {
		t.Fatal(err)
	}
	if err := peer.send(&agentv1.GuestCommand{Identity: peerGrant.Identity, DeliveryId: "hold", ControlSequence: 1, Command: &agentv1.GuestCommand_Suspend{Suspend: &agentv1.SessionSuspend{Reason: "retained hold"}}}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := peer.receive(peerRead()); err != nil {
		t.Fatal(err)
	}
	session.clock = session.entry.authorityNow
	primaryPID, peerPID := process.cmd.Process.Pid, peerProcess.cmd.Process.Pid
	r := newComputerOperationRegistry()
	r.setWallClock = func(time.Time) error { return nil }
	session.entry.baseComputerDiskVersionID = "disk-v1"
	r.entries[session.entry.computerInstanceID] = session.entry
	root, err := os.Open(session.entry.imageRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	r.writeback = &computerWriteback{syncFilesystem: func() error { return unix.Syncfs(int(root.Fd())) }}
	primaryRelay := newAgentRelay(t.Context(), session, nil)
	peerRelay := newAgentRelay(t.Context(), peer, nil)
	r.agentSessions[session.grant.Identity.SessionId] = primaryRelay
	r.agentSessions[peer.grant.Identity.SessionId] = peerRelay
	request := &agentv1.ComputerSessionCapture{Envelope: &computerv0.ComputerOperationEnvelope{OperationId: "capture-op", ComputerId: session.grant.ComputerId, ComputerInstanceId: session.grant.ComputerInstanceId, WriterGeneration: uint64(session.grant.WriterGeneration), ChannelCredential: session.grant.ChannelCredential, OperationExpiresAtUnixNano: time.Now().Add(time.Minute).UnixNano()}, CheckpointId: "two-process-checkpoint", DesiredVersion: 1, MembershipRevision: 2, Sessions: []*agentv1.SessionIdentity{session.grant.Identity, peer.grant.Identity}}
	captured := make(chan error, 1)
	go func() { _, err := r.captureAgentComputer(ctx, request); captured <- err }()
	if _, _, err := session.receive(read()); err != nil {
		t.Fatal(err)
	}
	primaryRelay.mu.Lock()
	primaryRelay.notifyLocked()
	primaryRelay.mu.Unlock()
	if _, _, err := peer.receive(peerRead()); err != nil {
		t.Fatal(err)
	}
	peerRelay.mu.Lock()
	peerRelay.notifyLocked()
	peerRelay.mu.Unlock()
	if err := <-captured; err != nil {
		t.Fatal(err)
	}
	if err := process.verifyFrozen(); err != nil {
		t.Fatal(err)
	}
	if err := peerProcess.verifyFrozen(); err != nil {
		t.Fatal(err)
	}
	installation := agentComputerInstallation(r, request, false)
	if _, err := r.installAgentComputer(ctx, installation, false, testComputerAuthorityClock()); err != nil {
		t.Fatal(err)
	}
	applyAgentComputerTestControls(t, r)
	primaryHost := attachAgentRelay(t, primaryRelay, 1, installation.Grants[0])
	defer primaryHost.Close()
	if _, err := r.installAgentComputer(ctx, installation, true, testComputerAuthorityClock()); err == nil {
		t.Fatal("actual Computer thawed before peer attachment")
	}
	if err := process.verifyFrozen(); err != nil {
		t.Fatal(err)
	}
	peerHost := attachAgentRelay(t, peerRelay, 1, installation.Grants[1])
	defer peerHost.Close()
	if _, err := r.installAgentComputer(ctx, installation, true, testComputerAuthorityClock()); err != nil {
		t.Fatal(err)
	}
	if !peer.held || session.held || process.cmd.Process.Pid != primaryPID || peerProcess.cmd.Process.Pid != peerPID {
		t.Fatal("Computer continuation lost process identity or hold")
	}
	if err := peerProcess.verifyFrozen(); err == nil {
		t.Fatal("peer remained frozen")
	}
}
