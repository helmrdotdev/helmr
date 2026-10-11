package guestd

import (
	"context"
	"errors"
	"github.com/helmrdotdev/helmr/internal/frameio"
	"github.com/helmrdotdev/helmr/internal/wire"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

	agentv1 "github.com/helmrdotdev/helmr/internal/proto/agent/v1"
	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"
	"google.golang.org/protobuf/proto"
)

func agentComputerFixture(t *testing.T) (*computerOperationRegistry, *agentv1.ComputerSessionCapture, []*fakeAgentProcess) {
	t.Helper()
	first, firstProcess := agentSessionFixture(t)
	second, secondProcess := agentSessionFixture(t)
	second.entry = first.entry
	first.clock = first.entry.authorityNow
	second.clock = first.entry.authorityNow
	second.grant.Identity.SessionId = "peer"
	first.entry.baseComputerDiskVersionID = "disk-v1"
	r := newComputerOperationRegistry()
	r.setWallClock = func(time.Time) error { return nil }
	r.entries[first.entry.computerInstanceID] = first.entry
	r.writeback = &computerWriteback{syncFilesystem: func() error { return nil }}
	for _, session := range []*agentSession{first, second} {
		relay := newAgentRelay(t.Context(), session, nil)
		r.agentSessions[session.grant.Identity.SessionId] = relay
		process := session.process.(*fakeAgentProcess)
		process.writeHook = func(_ context.Context, command *agentv1.GuestCommand) error {
			if command.GetCheckpoint() == nil {
				return nil
			}
			_, _, err := session.receive(&agentv1.ProgramEvent{Identity: session.grant.Identity, Event: &agentv1.ProgramEvent_CheckpointReady{CheckpointReady: &agentv1.SessionCheckpointReady{CheckpointId: command.GetCheckpoint().GetCheckpointId(), Outcome: &agentv1.SessionCheckpointReady_ScopesJson{ScopesJson: []byte("[]")}}}})
			return err
		}
	}
	request := &agentv1.ComputerSessionCapture{Envelope: &computerv0.ComputerOperationEnvelope{OperationId: "capture-op", ComputerId: "computer", ComputerInstanceId: "instance", WriterGeneration: 1, ChannelCredential: "channel", OperationExpiresAtUnixNano: time.Now().Add(time.Hour).UnixNano()}, CheckpointId: "checkpoint", DesiredVersion: 1, MembershipRevision: 2, Sessions: []*agentv1.SessionIdentity{proto.Clone(first.grant.Identity).(*agentv1.SessionIdentity), proto.Clone(second.grant.Identity).(*agentv1.SessionIdentity)}}
	return r, request, []*fakeAgentProcess{firstProcess, secondProcess}
}
func agentComputerInstallation(r *computerOperationRegistry, capture *agentv1.ComputerSessionCapture, abort bool) *agentv1.ComputerSessionInstallation {
	envelope := proto.Clone(capture.Envelope).(*computerv0.ComputerOperationEnvelope)
	envelope.OperationId = "continuation-op"
	disk := "disk-v2"
	if !abort {
		envelope.ComputerInstanceId = "restored-instance"
		envelope.WriterGeneration++
		envelope.ChannelCredential = "new-channel"
	} else {
		disk = "disk-v1"
	}
	request := &agentv1.ComputerSessionInstallation{Capture: proto.Clone(capture).(*agentv1.ComputerSessionCapture), Envelope: envelope, DesiredVersion: 2, BaseComputerDiskVersionId: disk, SourceAbort: abort}
	for _, member := range capture.Sessions {
		old := r.agentSessions[member.SessionId].session.grant
		next := proto.Clone(old).(*agentv1.SessionGrant)
		next.ComputerInstanceId = envelope.ComputerInstanceId
		next.WriterGeneration = int64(envelope.WriterGeneration)
		next.ChannelCredential = envelope.ChannelCredential
		next.AuthorityGeneration++
		next.ExpiresAtUnixNano += int64(time.Minute)
		if !abort {
			next.WorkerHostId = "new-worker"
			next.ComputerLeaseEpoch++
		}
		request.Grants = append(request.Grants, next)
	}
	return request
}

func TestAgentComputerCaptureSealsOnlyAnExactIdleSet(t *testing.T) {
	for _, kind := range []string{"missing", "extra", "duplicate", "epoch", "active", "pending-command"} {
		t.Run(kind, func(t *testing.T) {
			r, request, _ := agentComputerFixture(t)
			switch kind {
			case "missing":
				request.Sessions = request.Sessions[:1]
			case "extra":
				request.Sessions = append(request.Sessions, &agentv1.SessionIdentity{SessionId: "absent", ProcessEpoch: 1})
			case "duplicate":
				request.Sessions = append(request.Sessions, request.Sessions[0])
			case "epoch":
				request.Sessions[1].ProcessEpoch++
			case "active":
				r.agentSessions["peer"].session.activeTurn = "active"
			case "pending-command":
				r.agentSessions["peer"].writeBytes.Store(1)
			}
			if _, err := r.captureAgentComputer(t.Context(), request); err == nil {
				t.Fatal("captured incomplete or busy membership")
			}
			if r.captureSealed() {
				t.Fatal("failed precondition sealed a partial set")
			}
			for _, relay := range r.agentSessions {
				if relay.session.checkpointID != "" {
					t.Fatal("peer was sealed before all members validated")
				}
			}
		})
	}
}

func TestAgentComputerRestoreInstallsCompleteAuthorityBeforeAnyThaw(t *testing.T) {
	r, request, processes := agentComputerFixture(t)
	r.agentSessions["peer"].session.held = true
	receipt, err := r.captureAgentComputer(t.Context(), request)
	if err != nil || !receipt.GetFrozen() {
		t.Fatalf("capture: %v %v", receipt, err)
	}
	if err := r.verifyAgentComputerCaptureLocked(r.agentCapture); err != nil {
		t.Fatal(err)
	}
	if !r.captureSealed() {
		t.Fatal("capture reopened Computer admission")
	}
	installation := agentComputerInstallation(r, request, false)
	before := proto.Clone(r.agentSessions["session"].session.grant)
	invalid := proto.Clone(installation).(*agentv1.ComputerSessionInstallation)
	invalid.Grants[1].Identity.ProcessEpoch++
	if _, err := r.installAgentComputer(t.Context(), invalid, false, testComputerAuthorityClock()); err == nil {
		t.Fatal("accepted changed process epoch")
	}
	if !proto.Equal(before, r.agentSessions["session"].session.grant) || r.entries["instance"] == nil {
		t.Fatal("invalid set partially installed authority")
	}
	if _, err := r.installAgentComputer(t.Context(), installation, false, testComputerAuthorityClock()); err != nil {
		t.Fatal(err)
	}
	applyAgentComputerTestControls(t, r)
	if r.entries["instance"] != nil || r.entries["restored-instance"] == nil {
		t.Fatal("retained filesystem did not move to fresh ownership")
	}
	if err := r.agentSessions["session"].session.renew(before.(*agentv1.SessionGrant)); err == nil {
		t.Fatal("obsolete owner renewed restored Session")
	}
	attachAgentRelay(t, r.agentSessions["session"], 1, installation.Grants[0])
	if _, err := r.installAgentComputer(t.Context(), installation, true, testComputerAuthorityClock()); err == nil {
		t.Fatal("activated with a missing peer connection")
	}
	for _, process := range processes {
		if !process.physicallyFrozen {
			t.Fatal("partially thawed before all attachments")
		}
	}
	attachAgentRelay(t, r.agentSessions["peer"], 1, installation.Grants[1])
	receipt, err = r.installAgentComputer(t.Context(), installation, true, testComputerAuthorityClock())
	if err != nil || !receipt.GetActivated() || !receipt.GetActivationStarted() {
		t.Fatalf("activation: %v %v", receipt, err)
	}
	if r.captureSealed() || !r.agentSessions["peer"].session.held {
		t.Fatal("activation retained barrier or released a hold")
	}
	for _, process := range processes {
		if process.physicallyFrozen {
			t.Fatal("activation left member frozen")
		}
	}
	if err := r.verifyAgentComputerCaptureLocked(r.agentCapture); err == nil {
		t.Fatal("activated capture still advertised restorable")
	}
	if _, err := r.installAgentComputer(t.Context(), installation, true, testComputerAuthorityClock()); err != nil {
		t.Fatal("lost activation receipt was not replayable")
	}
}

func TestAgentComputerCaptureRejectsChangedFrozenKernelState(t *testing.T) {
	r, request, processes := agentComputerFixture(t)
	if _, err := r.captureAgentComputer(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	processes[1].physicallyFrozen = false
	if err := r.verifyAgentComputerCaptureLocked(r.agentCapture); err == nil {
		t.Fatal("accepted stale in-memory frozen flag")
	}
	if _, err := r.installAgentComputer(t.Context(), agentComputerInstallation(r, request, false), false, testComputerAuthorityClock()); err == nil {
		t.Fatal("installed restored authority on an unfrozen member")
	}
}

func TestAgentComputerSourceAbortPreservesLateProbeAndHeldState(t *testing.T) {
	r, request, processes := agentComputerFixture(t)
	r.agentSessions["peer"].session.held = true
	processes[1].writeHook = func(context.Context, *agentv1.GuestCommand) error { return nil }
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer cancel()
	if _, err := r.captureAgentComputer(ctx, request); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("capture: %v", err)
	}
	if !r.captureSealed() {
		t.Fatal("timeout silently reopened admission")
	}
	// An interrupted freezer may have set the kernel control before timing out.
	processes[1].physicallyFrozen = true
	installation := agentComputerInstallation(r, request, true)
	if _, err := r.installAgentComputer(t.Context(), installation, false, testComputerAuthorityClock()); err != nil {
		t.Fatal(err)
	}
	applyAgentComputerTestControls(t, r)
	for _, grant := range installation.Grants {
		attachAgentRelay(t, r.agentSessions[grant.Identity.SessionId], 1, grant)
	}
	if _, err := r.installAgentComputer(t.Context(), installation, true, testComputerAuthorityClock()); err != nil {
		t.Fatal(err)
	}
	peer := r.agentSessions["peer"].session
	if !peer.held || peer.terminal || processes[1].physicallyFrozen {
		t.Fatal("abort lost continuation, hold or thaw")
	}
	event := &agentv1.ProgramEvent{Identity: peer.grant.Identity, Event: &agentv1.ProgramEvent_CheckpointReady{CheckpointReady: &agentv1.SessionCheckpointReady{CheckpointId: request.CheckpointId, Outcome: &agentv1.SessionCheckpointReady_ScopesJson{ScopesJson: []byte("[]")}}}}
	if _, _, err := peer.receive(event); err != nil {
		t.Fatal("late probe destroyed source continuation", err)
	}
}

func TestAgentComputerActivationFailureReconcilesSameLiveProcesses(t *testing.T) {
	r, request, _ := agentComputerFixture(t)
	if _, err := r.captureAgentComputer(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	installation := agentComputerInstallation(r, request, false)
	installation.Envelope.OperationExpiresAtUnixNano = time.Now().Add(20 * time.Millisecond).UnixNano()
	if _, err := r.installAgentComputer(t.Context(), installation, false, testComputerAuthorityClock()); err != nil {
		t.Fatal(err)
	}
	applyAgentComputerTestControls(t, r)
	for _, grant := range installation.Grants {
		attachAgentRelay(t, r.agentSessions[grant.Identity.SessionId], 1, grant)
	}
	late := r.agentCapture.members[1].session.process.(*fakeAgentProcess)
	late.thawHook = func(context.Context) error { return errors.New("uncertain physical thaw") }
	receipt, err := r.installAgentComputer(t.Context(), installation, true, testComputerAuthorityClock())
	if err == nil || !receipt.GetActivationStarted() || receipt.GetActivated() || receipt.GetFrozen() {
		t.Fatalf("lost possible-execution receipt: %v %v", receipt, err)
	}
	if err := r.verifyAgentComputerCaptureLocked(r.agentCapture); err == nil {
		t.Fatal("possible execution authorized the old image")
	}
	if !r.captureSealed() {
		t.Fatal("partial activation reopened admission")
	}
	// Original operation expiry does not erase a retained receipt or prevent
	// reconciliation under still-current independently renewed Session grants.
	time.Sleep(25 * time.Millisecond)
	if inspected, err := r.inspectAgentComputer(request); err != nil || !inspected.GetActivationStarted() {
		t.Fatalf("lost status: %v %v", inspected, err)
	}
	late.thawHook = nil
	receipt, err = r.installAgentComputer(t.Context(), installation, true, testComputerAuthorityClock())
	if err != nil || !receipt.GetActivated() {
		t.Fatalf("same-source activation retry: %v %v", receipt, err)
	}
	if _, err := r.captureAgentComputer(t.Context(), request); err == nil {
		t.Fatal("reused obsolete source capture")
	}
}

func TestAgentComputerActivationDoesNotBlockPeerExpiry(t *testing.T) {
	r, request, _ := agentComputerFixture(t)
	if _, err := r.captureAgentComputer(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	installation := agentComputerInstallation(r, request, false)
	if _, err := r.installAgentComputer(t.Context(), installation, false, testComputerAuthorityClock()); err != nil {
		t.Fatal(err)
	}
	applyAgentComputerTestControls(t, r)
	for _, grant := range installation.Grants {
		attachAgentRelay(t, r.agentSessions[grant.Identity.SessionId], 1, grant)
	}
	early, late := r.agentCapture.members[0], r.agentCapture.members[1]
	early.session.clock = time.Now
	early.session.grant.ExpiresAtUnixNano = time.Now().Add(50 * time.Millisecond).UnixNano()
	expired := make(chan struct{}, 1)
	early.session.process.(*fakeAgentProcess).freezeHook = func() {
		select {
		case expired <- struct{}{}:
		default:
		}
	}
	go early.expiryLoop()
	late.session.process.(*fakeAgentProcess).thawHook = func(ctx context.Context) error {
		select {
		case <-expired:
			return errors.New("peer expiry remained live")
		case <-ctx.Done():
			// The watchdog is independently scheduled at the same grant deadline.
			select {
			case <-expired:
				return errors.New("peer expired")
			case <-time.After(time.Second):
				return errors.New("peer watchdog was blocked")
			}
		}
	}
	if _, err := r.installAgentComputer(t.Context(), installation, true, testComputerAuthorityClock()); err == nil {
		t.Fatal("activated through expired grant")
	}
	early.mu.Lock()
	frozen := early.expiryFrozen
	early.mu.Unlock()
	if !frozen {
		t.Fatal("peer watchdog could not enforce expiry during another thaw")
	}
}

func TestAgentComputerRestoreRejectsInconsistentPhysicalOwners(t *testing.T) {
	r, request, _ := agentComputerFixture(t)
	if _, err := r.captureAgentComputer(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	installation := agentComputerInstallation(r, request, false)
	installation.Grants[1].WorkerHostId = "different-worker"
	if _, err := r.installAgentComputer(t.Context(), installation, false, testComputerAuthorityClock()); err == nil {
		t.Fatal("installed two physical owners for one Computer")
	}
	if r.entries["instance"] == nil {
		t.Fatal("invalid owner set mutated the mount")
	}
}

func TestAgentComputerConnectionReturnsCurrentReceipt(t *testing.T) {
	r, request, _ := agentComputerFixture(t)
	guest, host := net.Pipe()
	defer guest.Close()
	defer host.Close()
	_ = host.SetDeadline(time.Now().Add(5 * time.Second))
	done := make(chan error, 1)
	go func() { err := handleConnection(t.Context(), guest, slog.Default(), r); done <- err }()
	if err := wire.WriteStreamFrameHeader(host, wire.StreamHeader{Type: wire.StreamTypeAgentComputer}, 0); err != nil {
		t.Fatal(err)
	}
	if err := writeAgentTransportFrame(host, &agentv1.ComputerSessionControl{Operation: &agentv1.ComputerSessionControl_Capture{Capture: request}}); err != nil {
		t.Fatal(err)
	}
	var receipt agentv1.ComputerSessionReceipt
	if err := frameio.ReadProtoFrameBounded(host, maxAgentTransportFrameBytes, &receipt); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if receipt.GetError() != "" || receipt.GetCheckpointId() != request.CheckpointId || !receipt.GetFrozen() {
		t.Fatalf("wire receipt: %v", &receipt)
	}
}

func TestAgentComputerInstallationSynchronizesClosedRelayReaders(t *testing.T) {
	r, request, _ := agentComputerFixture(t)
	closed, _ := agentSessionFixture(t)
	closed.entry = r.agentSessions["session"].session.entry
	closed.grant.Identity.SessionId = "closed"
	closed.terminal, closed.physicalClosed = true, true
	relay := newAgentRelay(t.Context(), closed, nil)
	close(relay.finished)
	r.agentSessions["closed"] = relay
	if _, err := r.captureAgentComputer(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	var readers sync.WaitGroup
	readers.Add(1)
	ready := make(chan struct{})
	go func() {
		defer readers.Done()
		close(ready)
		for ctx.Err() == nil {
			closed.mu.Lock()
			_ = validateSessionGrant(closed.entry, closed.grant, time.Now())
			closed.mu.Unlock()
		}
	}()
	<-ready
	defer func() { cancel(); readers.Wait() }()
	if _, err := r.installAgentComputer(t.Context(), agentComputerInstallation(r, request, false), false, testComputerAuthorityClock()); err != nil {
		t.Fatal(err)
	}
	applyAgentComputerTestControls(t, r)
}

func TestAgentComputerCaptureWaitsForTerminalReceipts(t *testing.T) {
	r, request, _ := agentComputerFixture(t)
	closed, _ := agentSessionFixture(t)
	closed.entry = r.agentSessions["session"].session.entry
	closed.grant.Identity.SessionId = "closed"
	closed.terminal, closed.physicalClosed = true, true
	relay := newAgentRelay(t.Context(), closed, nil)
	close(relay.finished)
	relay.outbox = []*agentv1.GuestSessionMessage{{Identity: closed.grant.Identity, Message: &agentv1.GuestSessionMessage_Stopped{Stopped: &agentv1.SessionStopped{}}}}
	r.agentSessions["closed"] = relay
	if _, err := r.captureAgentComputer(t.Context(), request); err == nil {
		t.Fatal("capture stranded pending terminal receipt under old ownership")
	}
}

func TestAgentComputerUncertainThawCannotRetainStaleExpiryFreeze(t *testing.T) {
	r, request, _ := agentComputerFixture(t)
	if _, err := r.captureAgentComputer(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	installation := agentComputerInstallation(r, request, false)
	if _, err := r.installAgentComputer(t.Context(), installation, false, testComputerAuthorityClock()); err != nil {
		t.Fatal(err)
	}
	applyAgentComputerTestControls(t, r)
	for _, grant := range installation.Grants {
		attachAgentRelay(t, r.agentSessions[grant.Identity.SessionId], 1, grant)
	}
	relay := r.agentCapture.members[0]
	process := relay.session.process.(*fakeAgentProcess)
	relay.session.clock = time.Now
	relay.session.grant.ExpiresAtUnixNano = time.Now().Add(75 * time.Millisecond).UnixNano()
	relay.expiryFrozen = true // Fresh attach preserved the earlier physical expiry stop.
	process.thawHook = func(context.Context) error { process.physicallyFrozen = false; return errors.New("thaw reply lost") }
	process.freezeError = errors.New("refreeze failed")
	go relay.expiryLoop()
	if _, err := r.installAgentComputer(t.Context(), installation, true, testComputerAuthorityClock()); err == nil {
		t.Fatal("ignored uncertain thaw")
	}
	select {
	case <-relay.finished:
	case <-time.After(2 * time.Second):
		t.Fatal("stale expiry flag suppressed physical enforcement")
	}
	relay.session.mu.Lock()
	closed := relay.session.physicalClosed
	relay.session.mu.Unlock()
	if !closed {
		t.Fatal("expired unfenced process survived")
	}
}

func testComputerAuthorityClock() *computerAuthorityClock {
	now := time.Now()
	return &computerAuthorityClock{anchor: now, authority: now.UnixNano()}
}

func TestAgentComputerAbsentCaptureRequiresGuestAdmissionExpiry(t *testing.T) {
	r, request, _ := agentComputerFixture(t)
	if _, err := r.inspectAgentComputer(request); err == nil || errors.Is(err, errComputerCaptureAbsentAfterExpiry) {
		t.Fatalf("live guest admission falsely closed: %v", err)
	}
	// The host's earlier observation cannot exclude a still-admissible request.
	if _, err := r.captureAgentComputer(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	if got, err := r.inspectAgentComputer(request); err != nil || !got.GetFrozen() {
		t.Fatalf("delayed capture lost: %v", err)
	}
	r2, expired, _ := agentComputerFixture(t)
	expired.Envelope.OperationExpiresAtUnixNano = time.Now().Add(-time.Second).UnixNano()
	if _, err := r2.inspectAgentComputer(expired); !errors.Is(err, errComputerCaptureAbsentAfterExpiry) {
		t.Fatalf("missing guest expiry proof: %v", err)
	}
	if _, err := r2.captureAgentComputer(t.Context(), expired); !errors.Is(err, errComputerAuthorityExpired) {
		t.Fatalf("expired delayed capture accepted: %v", err)
	}
}
func TestAgentComputerInstallReturnsExpiredAuthorityWithRetainedCapture(t *testing.T) {
	for _, kind := range []string{"envelope", "grant"} {
		t.Run(kind, func(t *testing.T) {
			r, request, _ := agentComputerFixture(t)
			if _, err := r.captureAgentComputer(t.Context(), request); err != nil {
				t.Fatal(err)
			}
			install := agentComputerInstallation(r, request, false)
			if kind == "envelope" {
				install.Envelope.OperationExpiresAtUnixNano = time.Now().Add(-time.Second).UnixNano()
			} else {
				install.Grants[0].ExpiresAtUnixNano = time.Now().Add(-time.Second).UnixNano()
			}
			receipt, err := r.installAgentComputer(t.Context(), install, false, testComputerAuthorityClock())
			if (!errors.Is(err, errComputerAuthorityExpired) && !errors.Is(err, errSessionGrantExpired)) || receipt.GetCheckpointId() != request.CheckpointId || receipt.GetInstalled() {
				t.Fatalf("expiry lost capture: receipt=%v error=%v", receipt, err)
			}
		})
	}
}

func TestAgentComputerRestoreRejectsUnusableCapture(t *testing.T) {
	for _, kind := range []string{"absent", "unfrozen", "member-unfrozen"} {
		t.Run(kind, func(t *testing.T) {
			r, request, _ := agentComputerFixture(t)
			if _, err := r.captureAgentComputer(t.Context(), request); err != nil {
				t.Fatal(err)
			}
			install := agentComputerInstallation(r, request, false)
			switch kind {
			case "absent":
				r.agentCapture = nil
			case "unfrozen":
				r.agentCapture.frozen = false
			case "member-unfrozen":
				r.agentCapture.members[0].captureFrozen = false
			}
			if _, err := r.installAgentComputer(t.Context(), install, false, testComputerAuthorityClock()); !errors.Is(err, errComputerCaptureUnusable) {
				t.Fatalf("unusable image not identified: %v", err)
			}
		})
	}
}
