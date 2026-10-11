package guestd

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/frameio"
	agentv1 "github.com/helmrdotdev/helmr/internal/proto/agent/v1"
	"google.golang.org/protobuf/proto"
)

func attachAgentRelay(t *testing.T, relay *agentRelay, sequence uint64, grant *agentv1.SessionGrant) net.Conn {
	t.Helper()
	guest, host := net.Pipe()
	t.Cleanup(func() { _ = guest.Close(); _ = host.Close() })
	_ = host.SetDeadline(time.Now().Add(5 * time.Second))
	attached := make(chan error, 1)
	go func() {
		attached <- relay.attach(guest, &agentv1.SessionAttach{Grant: grant, AttachmentSequence: sequence})
	}()
	var message agentv1.GuestSessionMessage
	if err := frameio.ReadProtoFrameBounded(host, maxAgentTransportFrameBytes, &message); err != nil {
		t.Fatal(err)
	}
	if message.GetAttached() == nil || message.GetAttachmentSequence() != sequence {
		t.Fatal("attachment receipt missing")
	}
	if err := <-attached; err != nil {
		t.Fatal(err)
	}
	go func() { _ = relay.serve(guest, sequence) }()
	return host
}
func waitAgentRelay(t *testing.T, relay *agentRelay, condition func() bool) {
	t.Helper()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for {
		relay.mu.Lock()
		done, changed := condition(), relay.changed
		relay.mu.Unlock()
		if done {
			return
		}
		select {
		case <-changed:
		case <-timer.C:
			t.Fatal("relay state did not converge")
		}
	}
}

func TestAgentRelayReattachmentKeepsPendingMutationAndOriginalAuthority(t *testing.T) {
	session, process := agentSessionFixture(t)
	guestDispatch(t, session, "turn", 1)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	relay := newAgentRelay(ctx, session, nil)
	go relay.sendLoop()
	envelope, _ := guestOperation(t, session, "output", agentv1.Operation_METHOD_OUTPUT, `{"outputId":"stable","value":7}`)
	sequence := relay.enqueue(envelope)
	first := attachAgentRelay(t, relay, 1, session.grant)
	var original agentv1.GuestSessionMessage
	if err := frameio.ReadProtoFrameBounded(first, maxAgentTransportFrameBytes, &original); err != nil {
		t.Fatal(err)
	}
	if original.GetEventSequence() != sequence {
		t.Fatal("event sequence changed")
	}
	if err := writeAgentTransportFrame(first, &agentv1.HostSessionMessage{AttachmentSequence: 1, Message: &agentv1.HostSessionMessage_Acknowledged{Acknowledged: &agentv1.SessionEventsAcknowledged{ThroughSequence: sequence}}}); err != nil {
		t.Fatal(err)
	}
	waitAgentRelay(t, relay, func() bool { return relay.acknowledged == sequence })
	relay.mu.Lock()
	retained := len(relay.outbox)
	relay.mu.Unlock()
	if retained != 1 {
		t.Fatal("host observation discarded an unresolved mutation")
	}
	_ = first.Close()
	newer := proto.Clone(session.grant).(*agentv1.SessionGrant)
	newer.AuthorityGeneration++
	second := attachAgentRelay(t, relay, 2, newer)
	var replay agentv1.GuestSessionMessage
	if err := frameio.ReadProtoFrameBounded(second, maxAgentTransportFrameBytes, &replay); err != nil {
		t.Fatal(err)
	}
	if replay.GetEventSequence() != original.GetEventSequence() || replay.GetOperationAuthorityGeneration() != original.GetOperationAuthorityGeneration() || !proto.Equal(replay.GetEvent(), original.GetEvent()) {
		t.Fatal("reattachment rewrote the original mutation")
	}
	result := &agentv1.GuestCommand{Identity: newer.Identity, Command: &agentv1.GuestCommand_OperationResult{OperationResult: &agentv1.OperationResult{RequestId: "output", Outcome: &agentv1.OperationResult_ValueJson{ValueJson: []byte(`{"sequence":1}`)}}}}
	if err := writeAgentTransportFrame(second, &agentv1.HostSessionMessage{AttachmentSequence: 2, Message: &agentv1.HostSessionMessage_Command{Command: result}}); err != nil {
		t.Fatal(err)
	}
	waitAgentRelay(t, relay, func() bool { return len(relay.outbox) == 0 })
	session.mu.Lock()
	last := process.commands[len(process.commands)-1]
	session.mu.Unlock()
	if !proto.Equal(last, result) {
		t.Fatal("receipt did not reach the retained local pipe")
	}
}

func TestAgentRelayAttachmentReportsUnretiredTurn(t *testing.T) {
	for _, settled := range []bool{false, true} {
		t.Run(map[bool]string{false: "running", true: "settled"}[settled], func(t *testing.T) {
			session, _ := agentSessionFixture(t)
			guestDispatch(t, session, "turn", 1)
			if settled {
				// The terminal DeliveryResult clears active execution, but the
				// host still owes a TurnAcknowledged before retiring this proof.
				session.activeTurn = ""
				session.turns["turn"].settled = true
			}
			relay := newAgentRelay(t.Context(), session, nil)
			guest, host := net.Pipe()
			defer guest.Close()
			defer host.Close()
			_ = host.SetDeadline(time.Now().Add(5 * time.Second))
			done := make(chan error, 1)
			go func() {
				done <- relay.attach(guest, &agentv1.SessionAttach{Grant: session.grant, AttachmentSequence: 1})
			}()
			var receipt agentv1.GuestSessionMessage
			if err := frameio.ReadProtoFrameBounded(host, maxAgentTransportFrameBytes, &receipt); err != nil {
				t.Fatal(err)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if receipt.GetAttached().GetPendingTurnId() != "turn" {
				t.Fatalf("lost unretired Turn: %v", &receipt)
			}
		})
	}
}

func TestAgentRelayCaptureRequiresReattachmentBeforePhysicalThaw(t *testing.T) {
	session, process := agentSessionFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	relay := newAgentRelay(ctx, session, nil)
	go relay.sendLoop()
	first := attachAgentRelay(t, relay, 1, session.grant)
	process.writeHook = func(_ context.Context, command *agentv1.GuestCommand) error {
		_, _, err := session.receive(&agentv1.ProgramEvent{Identity: session.grant.Identity, Event: &agentv1.ProgramEvent_CheckpointReady{CheckpointReady: &agentv1.SessionCheckpointReady{CheckpointId: command.GetCheckpoint().GetCheckpointId(), Outcome: &agentv1.SessionCheckpointReady_ScopesJson{ScopesJson: []byte("[]")}}}})
		return err
	}
	if err := relay.prepareCapture(t.Context(), "capture"); err != nil {
		t.Fatal(err)
	}
	if err := relay.freezeForCapture(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := relay.activateAfterCapture(t.Context()); err == nil {
		t.Fatal("thawed without upstream authority")
	}
	var data [1]byte
	if _, err := first.Read(data[:]); err == nil {
		t.Fatal("capture retained a live host socket")
	}
	second := attachAgentRelay(t, relay, 2, session.grant)
	relay.mu.Lock()
	frozen := relay.captureFrozen
	relay.mu.Unlock()
	if !frozen {
		t.Fatal("attachment prematurely thawed a captured Session")
	}
	if err := relay.activateAfterCapture(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := writeAgentTransportFrame(second, &agentv1.HostSessionMessage{AttachmentSequence: 1, Message: &agentv1.HostSessionMessage_Renew{Renew: session.grant}}); err != nil {
		t.Fatal(err)
	}
	if _, err := second.Read(data[:]); err == nil {
		t.Fatal("old attachment command was accepted")
	}
}

func TestAgentRelayBlockedWriterKeepsReceiptsRenewalAndExpiryIndependent(t *testing.T) {
	session, process := agentSessionFixture(t)
	session.clock = time.Now
	started, release, frozen := make(chan struct{}), make(chan struct{}), make(chan struct{})
	process.writeHook = func(ctx context.Context, _ *agentv1.GuestCommand) error {
		close(started)
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	process.freezeHook = func() { close(frozen) }
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	relay := newAgentRelay(ctx, session, nil)
	go relay.sendLoop()
	go relay.expiryLoop()
	sequence := relay.enqueue(&agentv1.GuestSessionMessage{Identity: session.grant.Identity, Message: &agentv1.GuestSessionMessage_Event{Event: &agentv1.ProgramEvent{Identity: session.grant.Identity, Event: &agentv1.ProgramEvent_Ready{Ready: &agentv1.SessionReady{}}}}})
	host := attachAgentRelay(t, relay, 1, session.grant)
	var event agentv1.GuestSessionMessage
	if err := frameio.ReadProtoFrameBounded(host, maxAgentTransportFrameBytes, &event); err != nil {
		t.Fatal(err)
	}
	command := &agentv1.GuestCommand{Identity: session.grant.Identity, DeliveryId: "dispatch", Command: &agentv1.GuestCommand_Dispatch{Dispatch: &agentv1.TurnDispatch{TurnId: "turn", Sequence: 1}}}
	if err := writeAgentTransportFrame(host, &agentv1.HostSessionMessage{AttachmentSequence: 1, Message: &agentv1.HostSessionMessage_Command{Command: command}}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("writer did not start")
	}
	if err := writeAgentTransportFrame(host, &agentv1.HostSessionMessage{AttachmentSequence: 1, Message: &agentv1.HostSessionMessage_Acknowledged{Acknowledged: &agentv1.SessionEventsAcknowledged{ThroughSequence: sequence}}}); err != nil {
		t.Fatal(err)
	}
	waitAgentRelay(t, relay, func() bool { return relay.acknowledged == sequence })
	renewed := proto.Clone(session.grant).(*agentv1.SessionGrant)
	renewed.AuthorityGeneration++
	// Renew first, then advance the injected clock to its expiration while the
	// same local frame remains blocked. Neither operation may depend on the pipe.
	if err := writeAgentTransportFrame(host, &agentv1.HostSessionMessage{AttachmentSequence: 1, Message: &agentv1.HostSessionMessage_Renew{Renew: renewed}}); err != nil {
		t.Fatal(err)
	}
	waitAgentRelay(t, relay, func() bool {
		session.mu.Lock()
		defer session.mu.Unlock()
		return session.grant.AuthorityGeneration == renewed.AuthorityGeneration
	})
	relay.mu.Lock()
	session.mu.Lock()
	session.clock = func() time.Time { return time.Unix(0, renewed.ExpiresAtUnixNano) }
	session.mu.Unlock()
	relay.notifyLocked()
	relay.mu.Unlock()
	select {
	case <-frozen:
	case <-time.After(time.Second):
		t.Fatal("expiry was blocked by local pipe")
	}
	session.mu.Lock()
	terminal := session.terminal
	session.mu.Unlock()
	if terminal {
		t.Fatal("expiry destroyed retained Session")
	}
	close(release)
}

func TestAgentRelayDetachedEventBackpressureIsBounded(t *testing.T) {
	session, _ := agentSessionFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	relay := newAgentRelay(ctx, session, nil)
	for range 1024 {
		relay.enqueue(&agentv1.GuestSessionMessage{Identity: session.grant.Identity})
	}
	available := make(chan bool, 1)
	go func() { available <- relay.awaitEventCapacity() }()
	select {
	case <-available:
		t.Fatal("full detached outbox admitted another event")
	case <-time.After(20 * time.Millisecond):
	}
	relay.mu.Lock()
	relay.outbox = relay.outbox[:0]
	relay.notifyLocked()
	relay.mu.Unlock()
	select {
	case ok := <-available:
		if !ok {
			t.Fatal("capacity was not restored")
		}
	case <-time.After(time.Second):
		t.Fatal("outbox never resumed")
	}
}

func TestAgentRelayFullWriterQueueCannotPreventShutdown(t *testing.T) {
	session, _ := agentSessionFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	closed := make(chan error, 1)
	relay := newAgentRelay(ctx, session, func(err error) { closed <- err })
	guest, host := net.Pipe()
	defer guest.Close()
	defer host.Close()
	relay.connection, relay.attachment = guest, 1
	for range cap(relay.writes) {
		relay.writes <- agentQueuedWrite{write: func() error { return nil }}
	}
	command := &agentv1.GuestCommand{Identity: session.grant.Identity, DeliveryId: "shutdown", Command: &agentv1.GuestCommand_Shutdown{Shutdown: &agentv1.SessionShutdown{Reason: "stop unresponsive reader"}}}
	if err := relay.handleHostMessage(guest, 1, &agentv1.HostSessionMessage{AttachmentSequence: 1, Message: &agentv1.HostSessionMessage_Command{Command: command}}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(agentShutdownTimeout + 2*time.Second):
		t.Fatal("full writer queue prevented physical close")
	}
	session.mu.Lock()
	physicallyClosed := session.physicalClosed
	session.mu.Unlock()
	if !physicallyClosed {
		t.Fatal("shutdown never joined process")
	}
}

func TestAgentRelayPhysicalFailureIsReportedWhileFrozen(t *testing.T) {
	session, process := agentSessionFixture(t)
	process.closeErr = errors.New("scope still populated")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	relay := newAgentRelay(ctx, session, nil)
	relay.expiryFrozen = true
	relay.finish(errors.New("root lost"))
	go relay.sendLoop()
	host := attachAgentRelay(t, relay, 1, session.grant)
	var message agentv1.GuestSessionMessage
	if err := frameio.ReadProtoFrameBounded(host, maxAgentTransportFrameBytes, &message); err != nil {
		t.Fatal(err)
	}
	if message.GetEvent().GetFailed() == nil {
		t.Fatal("physical failure was hidden by frozen state")
	}
	relay.mu.Lock()
	frozen := relay.expiryFrozen
	relay.mu.Unlock()
	if !frozen || !session.entry.recoveryRequired {
		t.Fatal("unproved physical scope lost its fence")
	}
}

func TestAgentRelayProcessExitBypassesFullOutbox(t *testing.T) {
	session, process := agentSessionFixture(t)
	process.waitHook = func(context.Context) error { return errors.New("root exited") }
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	closed := make(chan error, 1)
	relay := newAgentRelay(ctx, session, func(err error) { closed <- err })
	for range 1024 {
		relay.enqueue(&agentv1.GuestSessionMessage{Identity: session.grant.Identity})
	}
	go relay.watchProcess()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("outbox backpressure concealed process exit")
	}
}

func TestAgentRelayExitWatcherPreservesAuthoredFailureTail(t *testing.T) {
	session, process := agentSessionFixture(t)
	process.waitHook = func(context.Context) error { return errors.New("root exited") }
	process.readHook = func() (*agentv1.ProgramEvent, error) {
		time.Sleep(30 * time.Millisecond)
		return &agentv1.ProgramEvent{Identity: session.grant.Identity, Event: &agentv1.ProgramEvent_Failed{Failed: &agentv1.SessionFailed{Code: "bundle_error", Message: "selected export missing"}}}, nil
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	closed := make(chan error, 1)
	relay := newAgentRelay(ctx, session, func(err error) { closed <- err })
	relay.run()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("exit drainage never finished")
	}
	relay.mu.Lock()
	defer relay.mu.Unlock()
	found := false
	for _, event := range relay.outbox {
		if event.GetEvent().GetFailed().GetCode() == "bundle_error" {
			found = true
		}
	}
	if !found {
		t.Fatal("process watcher discarded authored failure tail")
	}
}

func TestAgentRelayCheckpointSealsAdmissionAndRejectsUnfinishedWork(t *testing.T) {
	for _, kind := range []string{"turn", "pending", "writer", "terminal", "expired"} {
		t.Run(kind, func(t *testing.T) {
			session, process := agentSessionFixture(t)
			relay := newAgentRelay(t.Context(), session, nil)
			switch kind {
			case "turn":
				session.activeTurn = "turn"
			case "pending":
				session.pending["operation"] = &agentPendingOperation{}
			case "writer":
				relay.writeBytes.Store(1)
			case "terminal":
				session.terminal = true
			case "expired":
				session.grant.ExpiresAtUnixNano = 1
			}
			if err := relay.prepareCapture(t.Context(), "checkpoint"); err == nil {
				t.Fatal("captured unfinished work")
			}
			if len(process.commands) != 1 || session.checkpointID != "" {
				t.Fatal("rejected capture changed local state")
			}
		})
	}
	session, process := agentSessionFixture(t)
	relay := newAgentRelay(t.Context(), session, nil)
	session.held = true
	process.writeHook = func(_ context.Context, command *agentv1.GuestCommand) error {
		if command.GetCheckpoint() == nil {
			t.Fatal("checkpoint was not private")
		}
		_, _, err := session.receive(&agentv1.ProgramEvent{Identity: session.grant.Identity, Event: &agentv1.ProgramEvent_CheckpointReady{CheckpointReady: &agentv1.SessionCheckpointReady{CheckpointId: "checkpoint", Outcome: &agentv1.SessionCheckpointReady_ScopesJson{ScopesJson: []byte("[]")}}}})
		return err
	}
	if err := relay.prepareCapture(t.Context(), "checkpoint"); err != nil {
		t.Fatal(err)
	}
	if !session.held {
		t.Fatal("checkpoint released a hold")
	}
	if err := session.send(&agentv1.GuestCommand{Identity: session.grant.Identity, DeliveryId: "resume", ControlSequence: 1, Command: &agentv1.GuestCommand_Resume{Resume: &agentv1.SessionResume{}}}); err == nil {
		t.Fatal("command crossed the capture barrier")
	}
	if !session.held {
		t.Fatal("sealed resume changed the hold")
	}
	_, reply := guestOperation(t, session, "late", agentv1.Operation_METHOD_HOLD, `{}`)
	if reply.GetOperationResult().GetError() == nil || len(session.pending) != 0 {
		t.Fatal("operation crossed the capture barrier")
	}
	process.convergenceErr = errors.New("scope membership changed")
	if err := relay.freezeForCapture(t.Context()); err == nil {
		t.Fatal("accepted stale native evidence")
	}
	if relay.captureFrozen {
		t.Fatal("claimed physical freeze after failed proof")
	}
	process.convergenceErr = nil
	if err := relay.freezeForCapture(t.Context()); err != nil {
		t.Fatal(err)
	}
	if process.proofs != 3 {
		t.Fatal("did not repeat physical verification")
	}
}

func TestAgentRelayCheckpointFailureRetainsSeal(t *testing.T) {
	session, process := agentSessionFixture(t)
	relay := newAgentRelay(t.Context(), session, nil)
	process.writeHook = func(_ context.Context, _ *agentv1.GuestCommand) error {
		_, _, err := session.receive(&agentv1.ProgramEvent{Identity: session.grant.Identity, Event: &agentv1.ProgramEvent_CheckpointReady{CheckpointReady: &agentv1.SessionCheckpointReady{CheckpointId: "checkpoint", Outcome: &agentv1.SessionCheckpointReady_Error{Error: &agentv1.OperationError{Message: "native continuation lost"}}}}})
		return err
	}
	if err := relay.prepareCapture(t.Context(), "checkpoint"); err == nil {
		t.Fatal("ignored checkpoint failure")
	}
	if session.checkpointID == "" {
		t.Fatal("failure reopened admission")
	}
	if err := relay.freezeForCapture(t.Context()); err == nil {
		t.Fatal("froze after failed checkpoint")
	}
	if process.proofs != 0 {
		t.Fatal("failed runtime claim reached physical proof")
	}
}

func TestAgentRelayCheckpointWriteFailureClosesUnusablePipe(t *testing.T) {
	session, process := agentSessionFixture(t)
	relay := newAgentRelay(t.Context(), session, nil)
	process.writeHook = func(context.Context, *agentv1.GuestCommand) error { return errors.New("partial probe write") }
	if err := relay.prepareCapture(t.Context(), "checkpoint"); err == nil {
		t.Fatal("ignored failed probe write")
	}
	<-relay.finished
	if !session.terminal || !session.physicalClosed {
		t.Fatal("partially written local frame left a reusable process")
	}
}

func TestAgentRelayCheckpointVerifiesFrozenMembership(t *testing.T) {
	session, process := agentSessionFixture(t)
	relay := newAgentRelay(t.Context(), session, nil)
	process.writeHook = func(_ context.Context, _ *agentv1.GuestCommand) error {
		_, _, err := session.receive(&agentv1.ProgramEvent{Identity: session.grant.Identity, Event: &agentv1.ProgramEvent_CheckpointReady{CheckpointReady: &agentv1.SessionCheckpointReady{CheckpointId: "checkpoint", Outcome: &agentv1.SessionCheckpointReady_ScopesJson{ScopesJson: []byte("[]")}}}})
		return err
	}
	if err := relay.prepareCapture(t.Context(), "checkpoint"); err != nil {
		t.Fatal(err)
	}
	process.freezeHook = func() { process.convergenceErr = errors.New("child appeared before physical freeze") }
	if err := relay.freezeForCapture(t.Context()); err == nil {
		t.Fatal("accepted membership changed during freeze")
	}
	if !relay.captureFrozen || session.checkpointID == "" {
		t.Fatal("failed proof released physical capture or admission")
	}
	if process.proofs != 2 {
		t.Fatal("did not verify frozen state")
	}
	if err := relay.activateAfterCapture(t.Context()); err == nil {
		t.Fatal("activated a capture with failed physical verification")
	}
}

func TestAgentRelayCheckpointCancellationRetainsAdmittedProbe(t *testing.T) {
	session, process := agentSessionFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	relay := newAgentRelay(t.Context(), session, nil)
	written := make(chan struct{})
	process.writeHook = func(writeCtx context.Context, _ *agentv1.GuestCommand) error {
		cancel()
		if writeCtx.Err() != nil {
			return writeCtx.Err()
		}
		close(written)
		return nil
	}
	if err := relay.prepareCapture(ctx, "checkpoint"); !errors.Is(err, context.Canceled) {
		t.Fatalf("capture returned %v", err)
	}
	<-written
	if session.terminal || session.physicalClosed || session.checkpointID != "checkpoint" {
		t.Fatal("caller cancellation discarded healthy retained continuation")
	}
}

func TestAgentRelayReconstructionRequiredIsRetainedReceipt(t *testing.T) {
	session, _ := agentSessionFixture(t)
	session.reconstructRequired = true
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	relay := newAgentRelay(ctx, session, nil)
	go relay.sendLoop()
	host := attachAgentRelay(t, relay, 1, session.grant)
	command := &agentv1.GuestCommand{Identity: session.grant.Identity, DeliveryId: "resume", ControlSequence: 1, Command: &agentv1.GuestCommand_Resume{Resume: &agentv1.SessionResume{}}}
	if err := writeAgentTransportFrame(host, &agentv1.HostSessionMessage{AttachmentSequence: 1, Message: &agentv1.HostSessionMessage_Command{Command: command}}); err != nil {
		t.Fatal(err)
	}
	var receipt agentv1.GuestSessionMessage
	if err := frameio.ReadProtoFrameBounded(host, maxAgentTransportFrameBytes, &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.GetEvent().GetDeliveryResult().GetError().GetCode() != "reconstruction_required" {
		t.Fatalf("missing typed failure: %v", &receipt)
	}
	_ = host.Close()
	second := attachAgentRelay(t, relay, 2, session.grant)
	var replay agentv1.GuestSessionMessage
	if err := frameio.ReadProtoFrameBounded(second, maxAgentTransportFrameBytes, &replay); err != nil {
		t.Fatal(err)
	}
	if replay.EventSequence != receipt.EventSequence || !proto.Equal(replay.GetEvent(), receipt.GetEvent()) {
		t.Fatal("reconstruction receipt lost on reconnect")
	}
}

func TestAgentRelayClosedMessageRetainsOneRejection(t *testing.T) {
	session, process := agentSessionFixture(t)
	guestDispatch(t, session, "turn", 1)
	session.turns["turn"].closed = true
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	relay := newAgentRelay(ctx, session, nil)
	go relay.sendLoop()
	host := attachAgentRelay(t, relay, 1, session.grant)
	command := &agentv1.GuestCommand{Identity: session.grant.Identity, DeliveryId: "message:turn:id", Command: &agentv1.GuestCommand_Message{Message: &agentv1.MessageDelivery{TurnId: "turn", MessageId: "id", InputJson: []byte(`null`)}}}
	if err := writeAgentTransportFrame(host, &agentv1.HostSessionMessage{AttachmentSequence: 1, Message: &agentv1.HostSessionMessage_Command{Command: command}}); err != nil {
		t.Fatal(err)
	}
	var receipt agentv1.GuestSessionMessage
	if err := frameio.ReadProtoFrameBounded(host, maxAgentTransportFrameBytes, &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.GetEvent().GetDeliveryResult().GetError().GetCode() != "turn_closed" {
		t.Fatalf("missing rejection: %v", &receipt)
	}
	if write, err := session.prepareCommand(command); err != nil || write != nil {
		t.Fatalf("duplicate rejection produced another receipt: %v", err)
	}
	if len(process.commands) != 2 {
		t.Fatal("closed message reached runtime")
	}
	_ = host.Close()
	second := attachAgentRelay(t, relay, 2, session.grant)
	var replay agentv1.GuestSessionMessage
	if err := frameio.ReadProtoFrameBounded(second, maxAgentTransportFrameBytes, &replay); err != nil {
		t.Fatal(err)
	}
	if replay.EventSequence != receipt.EventSequence || !proto.Equal(replay.GetEvent(), receipt.GetEvent()) {
		t.Fatal("message rejection lost on reconnect")
	}
}
