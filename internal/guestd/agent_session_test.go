package guestd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	agentv1 "github.com/helmrdotdev/helmr/internal/proto/agent/v1"
	"google.golang.org/protobuf/proto"
)

type fakeAgentProcess struct {
	physicallyFrozen bool
	thawHook         func(context.Context) error
	freezeError      error
	commands         []*agentv1.GuestCommand
	convergenceErr   error
	closeErr         error
	closeStarted     chan struct{}
	closeRelease     chan struct{}
	proofs           int
	writeHook        func(context.Context, *agentv1.GuestCommand) error
	freezeHook       func()
	waitHook         func(context.Context) error
	readHook         func() (*agentv1.ProgramEvent, error)
}

func (*fakeAgentProcess) start(context.Context) error { return nil }
func (p *fakeAgentProcess) write(ctx context.Context, command *agentv1.GuestCommand) error {
	if p.writeHook != nil {
		return p.writeHook(ctx, command)
	}
	p.commands = append(p.commands, proto.Clone(command).(*agentv1.GuestCommand))
	return nil
}
func (p *fakeAgentProcess) read() (*agentv1.ProgramEvent, error) {
	if p.readHook != nil {
		return p.readHook()
	}
	return nil, io.EOF
}
func (*fakeAgentProcess) logs() (io.Reader, io.Reader) {
	return strings.NewReader(""), strings.NewReader("")
}
func (p *fakeAgentProcess) converge([]nativeScopeEvidence) error { p.proofs++; return p.convergenceErr }
func (p *fakeAgentProcess) freeze(context.Context) error {
	if p.freezeError != nil {
		return p.freezeError
	}
	p.physicallyFrozen = true
	if p.freezeHook != nil {
		p.freezeHook()
	}
	return nil
}
func (p *fakeAgentProcess) thaw(ctx context.Context) error {
	if p.thawHook != nil {
		if err := p.thawHook(ctx); err != nil {
			return err
		}
	}
	p.physicallyFrozen = false
	return nil
}
func (p *fakeAgentProcess) verifyFrozen() error {
	if !p.physicallyFrozen {
		return errors.New("scope not frozen")
	}
	return nil
}
func (p *fakeAgentProcess) close(context.Context) error {
	if p.closeStarted != nil {
		close(p.closeStarted)
		<-p.closeRelease
	}
	return p.closeErr
}

func agentSessionFixture(t *testing.T) (*agentSession, *fakeAgentProcess) {
	t.Helper()
	now := time.Now()
	entry := &computerMountEntry{computerID: "computer", computerInstanceID: "instance", writerGeneration: 1, channelCredential: "channel"}
	grant := &agentv1.SessionGrant{Identity: &agentv1.SessionIdentity{SessionId: "session", ProcessEpoch: 1}, ComputerId: "computer", ComputerInstanceId: "instance", WriterGeneration: 1, WorkerHostId: "worker", ComputerLeaseEpoch: 2, AuthorityGeneration: 3, ExpiresAtUnixNano: now.Add(time.Hour).UnixNano(), ChannelCredential: "channel"}
	start := &agentv1.SessionStart{LogLimits: &agentv1.SessionLogLimits{ChunkBytes: 16 * 1024, BufferBytes: 128 * 1024, BufferRecords: 16}, AgentId: "agent", ComputerId: "computer", DeploymentId: "deployment", RecoveryKind: agentv1.SessionStart_RECOVERY_KIND_INITIAL}
	process := new(fakeAgentProcess)
	session, err := newAgentSession(entry, grant, start, process, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	command := &agentv1.GuestCommand{Identity: grant.Identity, Command: &agentv1.GuestCommand_Start{Start: start}}
	if err := session.send(command); err != nil {
		t.Fatal(err)
	}
	if err := session.send(command); err != nil {
		t.Fatal(err)
	}
	if len(process.commands) != 1 {
		t.Fatal("setup was replayed")
	}
	if _, _, err := session.receive(&agentv1.ProgramEvent{Identity: grant.Identity, Event: &agentv1.ProgramEvent_Ready{Ready: &agentv1.SessionReady{}}}); err != nil {
		t.Fatal(err)
	}
	return session, process
}
func guestDispatch(t *testing.T, session *agentSession, id string, sequence int64) {
	t.Helper()
	if err := session.send(&agentv1.GuestCommand{Identity: session.grant.Identity, DeliveryId: "dispatch-" + id, Command: &agentv1.GuestCommand_Dispatch{Dispatch: &agentv1.TurnDispatch{TurnId: id, Sequence: sequence, InputJson: []byte(`[{"type":"text","text":"fixture input"}]`), SourceJson: []byte(`{"kind":"api"}`)}}}); err != nil {
		t.Fatal(err)
	}
}
func guestOperation(t *testing.T, session *agentSession, id string, method agentv1.Operation_Method, payload string) (*agentv1.GuestSessionMessage, *agentv1.GuestCommand) {
	t.Helper()
	event := &agentv1.ProgramEvent{Identity: session.grant.Identity, Event: &agentv1.ProgramEvent_Operation{Operation: &agentv1.Operation{RequestId: id, TurnId: proto.String("turn"), Method: method, PayloadJson: []byte(payload)}}}
	envelope, reply, err := session.receive(event)
	if err != nil {
		t.Fatal(err)
	}
	return envelope, reply
}
func guestReceipt(t *testing.T, session *agentSession, id, value string) {
	t.Helper()
	if err := session.send(&agentv1.GuestCommand{Identity: session.grant.Identity, Command: &agentv1.GuestCommand_OperationResult{OperationResult: &agentv1.OperationResult{RequestId: id, Outcome: &agentv1.OperationResult_ValueJson{ValueJson: []byte(value)}}}}); err != nil {
		t.Fatal(err)
	}
}
func requireGuestRejected(t *testing.T, envelope *agentv1.GuestSessionMessage, reply *agentv1.GuestCommand) {
	t.Helper()
	if envelope != nil || reply.GetOperationResult().GetError() == nil {
		t.Fatal("operation was not rejected")
	}
}

func TestAgentSessionFinalizationRequiresIndependentPhysicalAndMutationProof(t *testing.T) {
	session, process := agentSessionFixture(t)
	guestDispatch(t, session, "turn", 1)
	// Failure convergence can arrive before processing closure is acknowledged.
	envelope, reply := guestOperation(t, session, "converge", agentv1.Operation_METHOD_CONVERGE_NATIVE, `{"disposition":"failed","scopes":[]}`)
	if envelope != nil || reply.GetOperationResult().GetError() != nil {
		t.Fatal("early physical convergence was rejected")
	}
	envelope, reply = guestOperation(t, session, "early-final", agentv1.Operation_METHOD_FINALIZE, `{"result":1}`)
	requireGuestRejected(t, envelope, reply)
	envelope, reply = guestOperation(t, session, "close", agentv1.Operation_METHOD_CLOSE_PROCESSING, `null`)
	if envelope == nil || reply != nil {
		t.Fatal("closure was not forwarded")
	}
	envelope, reply = guestOperation(t, session, "before-close-ack", agentv1.Operation_METHOD_FINALIZE, `{"result":1}`)
	requireGuestRejected(t, envelope, reply)
	guestReceipt(t, session, "close", `null`)
	guestOperation(t, session, "output", agentv1.Operation_METHOD_OUTPUT, `{"value":1}`)
	guestOperation(t, session, "converge-output", agentv1.Operation_METHOD_CONVERGE_NATIVE, `{"disposition":"returned","scopes":[]}`)
	envelope, reply = guestOperation(t, session, "before-output-ack", agentv1.Operation_METHOD_FINALIZE, `{"result":1}`)
	requireGuestRejected(t, envelope, reply)
	guestReceipt(t, session, "output", `{"sequence":1}`)
	envelope, reply = guestOperation(t, session, "final", agentv1.Operation_METHOD_FINALIZE, `{"result":1,"drainEvidence":"authored"}`)
	if envelope == nil || reply != nil || envelope.GetDrainEvidence() == "" || envelope.GetDrainEvidence() == "authored" {
		t.Fatal("missing independent guest attestation")
	}
	repeated, _ := guestOperation(t, session, "final", agentv1.Operation_METHOD_FINALIZE, `{"result":1,"drainEvidence":"authored"}`)
	if repeated.GetDrainEvidence() != envelope.GetDrainEvidence() {
		t.Fatal("replay changed its original evidence")
	}
	if process.proofs != 3 {
		t.Fatalf("physical checks = %d", process.proofs)
	}
	guestReceipt(t, session, "final", `{"status":"completed","result":1,"response":[{"type":"text","text":"done"}]}`)
	if _, _, err := session.receive(&agentv1.ProgramEvent{Identity: session.grant.Identity, Event: &agentv1.ProgramEvent_DeliveryResult{DeliveryResult: &agentv1.DeliveryResult{DeliveryId: "dispatch-turn", Outcome: &agentv1.DeliveryResult_ValueJson{ValueJson: []byte(`{"status":"completed","result":1,"response":[{"type":"text","text":"done"}]}`)}}}}); err != nil {
		t.Fatal(err)
	}
	guestDispatch(t, session, "next", 2)
}

func TestAgentSessionResponseMustBeAdmittedBeforeCloseAndJoined(t *testing.T) {
	session, _ := agentSessionFixture(t)
	guestDispatch(t, session, "turn", 1)
	envelope, reply := guestOperation(t, session, "response", agentv1.Operation_METHOD_RESPOND, `{"responseId":"response","value":[]}`)
	if envelope == nil || reply != nil {
		t.Fatal("response was not forwarded")
	}
	guestOperation(t, session, "close", agentv1.Operation_METHOD_CLOSE_PROCESSING, `null`)
	guestReceipt(t, session, "close", `null`)
	envelope, reply = guestOperation(t, session, "late-response", agentv1.Operation_METHOD_RESPOND, `{"responseId":"late","value":[]}`)
	requireGuestRejected(t, envelope, reply)
	guestOperation(t, session, "converge", agentv1.Operation_METHOD_CONVERGE_NATIVE, `{"disposition":"returned","scopes":[]}`)
	envelope, reply = guestOperation(t, session, "before-response-ack", agentv1.Operation_METHOD_FINALIZE, `{"result":null}`)
	requireGuestRejected(t, envelope, reply)
	guestReceipt(t, session, "response", `null`)
	envelope, reply = guestOperation(t, session, "final", agentv1.Operation_METHOD_FINALIZE, `{"result":null}`)
	if envelope == nil || reply != nil || envelope.GetDrainEvidence() == "" {
		t.Fatal("acknowledged response did not allow finalization")
	}
}

func TestAgentSessionRejectsUnprovedTerminalReceiptAndLostPhysicalProof(t *testing.T) {
	session, process := agentSessionFixture(t)
	guestDispatch(t, session, "turn", 1)
	if _, _, err := session.receive(&agentv1.ProgramEvent{Identity: session.grant.Identity, Event: &agentv1.ProgramEvent_DeliveryResult{DeliveryResult: &agentv1.DeliveryResult{DeliveryId: "dispatch-turn", Outcome: &agentv1.DeliveryResult_ValueJson{ValueJson: []byte(`{"status":"completed"}`)}}}}); err == nil {
		t.Fatal("authored receipt bypassed finalization")
	}
	guestOperation(t, session, "close", agentv1.Operation_METHOD_CLOSE_PROCESSING, `null`)
	guestReceipt(t, session, "close", `null`)
	guestOperation(t, session, "converge", agentv1.Operation_METHOD_CONVERGE_NATIVE, `{"disposition":"returned","scopes":[]}`)
	process.convergenceErr = errors.New("unjoined child")
	envelope, reply := guestOperation(t, session, "final", agentv1.Operation_METHOD_FINALIZE, `{"result":1}`)
	requireGuestRejected(t, envelope, reply)
}

func TestAgentSessionRenewalRetainsOriginalPendingGeneration(t *testing.T) {
	session, _ := agentSessionFixture(t)
	guestDispatch(t, session, "turn", 1)
	initial, _ := guestOperation(t, session, "output", agentv1.Operation_METHOD_OUTPUT, `{"value":1}`)
	grant := proto.Clone(session.grant).(*agentv1.SessionGrant)
	grant.AuthorityGeneration++
	if err := session.renew(grant); err != nil {
		t.Fatal(err)
	}
	replay, _ := guestOperation(t, session, "output", agentv1.Operation_METHOD_OUTPUT, `{"value":1}`)
	if replay.GetOperationAuthorityGeneration() != initial.GetOperationAuthorityGeneration() {
		t.Fatal("pending mutation gained newer authority")
	}
	grant.ComputerLeaseEpoch++
	if err := session.renew(grant); err == nil {
		t.Fatal("renewal moved Computer ownership")
	}
	session.clock = func() time.Time { return time.Unix(0, session.grant.ExpiresAtUnixNano) }
	if _, _, err := session.receive(&agentv1.ProgramEvent{Identity: session.grant.Identity, Event: &agentv1.ProgramEvent_Ready{Ready: &agentv1.SessionReady{}}}); err == nil {
		t.Fatal("expired grant accepted authored work")
	}
}

func TestAgentSessionStaleResumeCannotReleaseNewerHold(t *testing.T) {
	session, _ := agentSessionFixture(t)
	for _, command := range []*agentv1.GuestCommand{
		{Identity: session.grant.Identity, DeliveryId: "hold", ControlSequence: 3, Command: &agentv1.GuestCommand_Suspend{Suspend: &agentv1.SessionSuspend{Reason: "hold"}}},
		{Identity: session.grant.Identity, DeliveryId: "stale-resume", ControlSequence: 2, Command: &agentv1.GuestCommand_Resume{Resume: &agentv1.SessionResume{}}},
	} {
		if err := session.send(command); err != nil {
			t.Fatal(err)
		}
	}
	if !session.held {
		t.Fatal("stale resume released hold")
	}
	if err := session.send(&agentv1.GuestCommand{Identity: session.grant.Identity, DeliveryId: "resume", ControlSequence: 4, Command: &agentv1.GuestCommand_Resume{Resume: &agentv1.SessionResume{}}}); err != nil {
		t.Fatal(err)
	}
	if !session.held {
		t.Fatal("resume released admission before runtime acknowledgment")
	}
	guestControlReceipt(t, session, "resume", false)
	guestDispatch(t, session, "turn", 1)
}

func guestControlReceipt(t *testing.T, session *agentSession, id string, failed bool) {
	t.Helper()
	receipt := &agentv1.DeliveryResult{DeliveryId: id, Outcome: &agentv1.DeliveryResult_ValueJson{ValueJson: []byte(`null`)}}
	if failed {
		receipt.Outcome = &agentv1.DeliveryResult_Error{Error: &agentv1.OperationError{Code: "cannot_resume", Message: "runtime is held"}}
	}
	if _, _, err := session.receive(&agentv1.ProgramEvent{Identity: session.grant.Identity, Event: &agentv1.ProgramEvent_DeliveryResult{DeliveryResult: receipt}}); err != nil {
		t.Fatal(err)
	}
}

func TestAgentSessionSupersededResumeReceiptKeepsHold(t *testing.T) {
	session, _ := agentSessionFixture(t)
	for _, command := range []*agentv1.GuestCommand{
		{Identity: session.grant.Identity, DeliveryId: "hold", ControlSequence: 3, Command: &agentv1.GuestCommand_Suspend{Suspend: &agentv1.SessionSuspend{}}},
		{Identity: session.grant.Identity, DeliveryId: "same-sequence-resume", ControlSequence: 3, Command: &agentv1.GuestCommand_Resume{Resume: &agentv1.SessionResume{}}},
	} {
		if err := session.send(command); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := session.receive(&agentv1.ProgramEvent{Identity: session.grant.Identity, Event: &agentv1.ProgramEvent_DeliveryResult{DeliveryResult: &agentv1.DeliveryResult{DeliveryId: "same-sequence-resume", Outcome: &agentv1.DeliveryResult_ValueJson{ValueJson: []byte(`{"superseded":true}`)}}}}); err != nil {
		t.Fatal(err)
	}
	if !session.held {
		t.Fatal("superseded Resume cleared guest hold while runtime remained held")
	}
}

func TestAgentSessionResumeReceiptCannotClearLaterHold(t *testing.T) {
	for _, later := range []string{"suspend", "failed", "runtime_hold", "shutdown", "resume_error"} {
		t.Run(later, func(t *testing.T) {
			session, _ := agentSessionFixture(t)
			if err := session.send(&agentv1.GuestCommand{Identity: session.grant.Identity, DeliveryId: "hold", ControlSequence: 1, Command: &agentv1.GuestCommand_Suspend{Suspend: &agentv1.SessionSuspend{}}}); err != nil {
				t.Fatal(err)
			}
			if err := session.send(&agentv1.GuestCommand{Identity: session.grant.Identity, DeliveryId: "resume", ControlSequence: 2, Command: &agentv1.GuestCommand_Resume{Resume: &agentv1.SessionResume{}}}); err != nil {
				t.Fatal(err)
			}
			switch later {
			case "suspend":
				if err := session.send(&agentv1.GuestCommand{Identity: session.grant.Identity, DeliveryId: "new-hold", ControlSequence: 3, Command: &agentv1.GuestCommand_Suspend{Suspend: &agentv1.SessionSuspend{}}}); err != nil {
					t.Fatal(err)
				}
			case "shutdown":
				if err := session.send(&agentv1.GuestCommand{Identity: session.grant.Identity, DeliveryId: "shutdown", Command: &agentv1.GuestCommand_Shutdown{Shutdown: &agentv1.SessionShutdown{}}}); err != nil {
					t.Fatal(err)
				}
			case "failed":
				if _, _, err := session.receive(&agentv1.ProgramEvent{Identity: session.grant.Identity, Event: &agentv1.ProgramEvent_Failed{Failed: &agentv1.SessionFailed{Code: "setup_failed"}}}); err != nil {
					t.Fatal(err)
				}
			case "runtime_hold":
				if _, _, err := session.receive(&agentv1.ProgramEvent{Identity: session.grant.Identity, Event: &agentv1.ProgramEvent_Operation{Operation: &agentv1.Operation{RequestId: "runtime-hold", Method: agentv1.Operation_METHOD_HOLD, PayloadJson: []byte(`{"reason":"native_convergence_failed"}`)}}}); err != nil {
					t.Fatal(err)
				}
			}
			guestControlReceipt(t, session, "resume", later == "resume_error")
			if !session.held {
				t.Fatal("old or failed resume released hold")
			}
			if later == "failed" || later == "runtime_hold" {
				if err := session.send(&agentv1.GuestCommand{Identity: session.grant.Identity, DeliveryId: "new-resume", ControlSequence: 4, Command: &agentv1.GuestCommand_Resume{Resume: &agentv1.SessionResume{}}}); err == nil {
					t.Fatal("resume repaired a process requiring reconstruction")
				}
			}
		})
	}
}

func TestAgentSessionFailedPhysicalCloseFencesComputer(t *testing.T) {
	session, process := agentSessionFixture(t)
	process.closeErr = errors.New("scope still populated")
	if err := session.close(t.Context()); err == nil {
		t.Fatal("failed close was accepted")
	}
	if !session.entry.recoveryRequired || !session.terminal {
		t.Fatal("unjoined scope lost its ownership fence")
	}
}

func TestAgentSessionReconciliationRetiresInputsAndAllowsNextTurn(t *testing.T) {
	session, _ := agentSessionFixture(t)
	guestDispatch(t, session, "turn", 1)
	guestOperation(t, session, "close", agentv1.Operation_METHOD_CLOSE_PROCESSING, `null`)
	guestReceipt(t, session, "close", `null`)
	guestOperation(t, session, "converge", agentv1.Operation_METHOD_CONVERGE_NATIVE, `{"disposition":"returned","scopes":[]}`)
	guestOperation(t, session, "final", agentv1.Operation_METHOD_FINALIZE, `{"result":1}`)
	failure := &agentv1.OperationError{Code: "temporary", Message: "retry"}
	if err := session.send(&agentv1.GuestCommand{Identity: session.grant.Identity, Command: &agentv1.GuestCommand_OperationResult{OperationResult: &agentv1.OperationResult{RequestId: "final", Outcome: &agentv1.OperationResult_Error{Error: failure}}}}); err != nil {
		t.Fatal(err)
	}
	receipt := func(id string, failed bool) {
		result := &agentv1.DeliveryResult{DeliveryId: id, Outcome: &agentv1.DeliveryResult_ValueJson{ValueJson: []byte("null")}}
		if failed {
			result.Outcome = &agentv1.DeliveryResult_Error{Error: failure}
		}
		if _, _, err := session.receive(&agentv1.ProgramEvent{Identity: session.grant.Identity, Event: &agentv1.ProgramEvent_DeliveryResult{DeliveryResult: result}}); err != nil {
			t.Fatal(err)
		}
	}
	receipt("dispatch-turn", true)
	if err := session.send(&agentv1.GuestCommand{Identity: session.grant.Identity, DeliveryId: "reconcile", Command: &agentv1.GuestCommand_Reconcile{Reconcile: &agentv1.TurnReconcile{TurnId: "turn"}}}); err != nil {
		t.Fatal(err)
	}
	guestOperation(t, session, "retry-final", agentv1.Operation_METHOD_FINALIZE, `{"result":1}`)
	guestReceipt(t, session, "retry-final", `{"status":"completed","result":1}`)
	receipt("reconcile", false)
	ack := &agentv1.GuestCommand{Identity: session.grant.Identity, DeliveryId: "ack", Command: &agentv1.GuestCommand_Acknowledged{Acknowledged: &agentv1.TurnAcknowledged{TurnId: "turn", Sequence: 1}}}
	if err := session.send(ack); err != nil {
		t.Fatal(err)
	}
	if err := session.send(ack); err != nil {
		t.Fatal(err)
	}
	receipt("ack", false)
	if len(session.deliveries) != 0 || len(session.turns) != 0 {
		t.Fatal("retired Turn retained input payloads")
	}
	if err := session.send(ack); err != nil {
		t.Fatal("lost acknowledgment cannot replay:", err)
	}
	receipt("ack", false)
	guestDispatch(t, session, "next", 2)
	if err := session.send(&agentv1.GuestCommand{Identity: session.grant.Identity, DeliveryId: "old", Command: &agentv1.GuestCommand_Dispatch{Dispatch: &agentv1.TurnDispatch{TurnId: "turn", Sequence: 1}}}); err == nil {
		t.Fatal("retired dispatch restarted")
	}
}

func TestAgentSessionReconstructionWaitsForPhysicalClose(t *testing.T) {
	session, process := agentSessionFixture(t)
	process.closeStarted, process.closeRelease = make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() { done <- session.close(t.Context()) }()
	<-process.closeStarted
	registry := &computerOperationRegistry{agentSessions: map[string]*agentRelay{"session": newAgentRelay(t.Context(), session, nil)}}
	grant := proto.Clone(session.grant).(*agentv1.SessionGrant)
	grant.Identity.ProcessEpoch++
	start := proto.Clone(session.start).(*agentv1.SessionStart)
	start.RecoveryKind = agentv1.SessionStart_RECOVERY_KIND_RECONSTRUCTED
	_, err := registry.openAgentSession(t.Context(), &agentv1.SessionAttach{Grant: grant, Start: start, AttachmentSequence: 1}, nil)
	if err == nil || !strings.Contains(err.Error(), "closed earlier process") {
		t.Errorf("reconstruction passed pending teardown: %v", err)
	}
	close(process.closeRelease)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !session.physicalClosed {
		t.Fatal("successful physical close not recorded")
	}
}

func TestAgentSessionPendingControlReplayDoesNotDuplicateLocalReceipt(t *testing.T) {
	session, process := agentSessionFixture(t)
	command := &agentv1.GuestCommand{Identity: session.grant.Identity, DeliveryId: "hold", ControlSequence: 1, Command: &agentv1.GuestCommand_Suspend{Suspend: &agentv1.SessionSuspend{Reason: "hold"}}}
	before := len(process.commands)
	for range 2 {
		if err := session.send(command); err != nil {
			t.Fatal(err)
		}
	}
	if len(process.commands) != before+1 {
		t.Fatal("pending control replay produced a second local write")
	}
	if _, _, err := session.receive(&agentv1.ProgramEvent{Identity: session.grant.Identity, Event: &agentv1.ProgramEvent_DeliveryResult{DeliveryResult: &agentv1.DeliveryResult{DeliveryId: "hold", Outcome: &agentv1.DeliveryResult_ValueJson{ValueJson: []byte("null")}}}}); err != nil {
		t.Fatal(err)
	}
	if len(session.deliveries) != 0 {
		t.Fatal("control payload retained after receipt")
	}
}

func TestAgentSessionRenewalReattestsRetainedConvergence(t *testing.T) {
	session, process := agentSessionFixture(t)
	guestDispatch(t, session, "turn", 1)
	guestOperation(t, session, "close", agentv1.Operation_METHOD_CLOSE_PROCESSING, `null`)
	guestReceipt(t, session, "close", `null`)
	guestOperation(t, session, "converge", agentv1.Operation_METHOD_CONVERGE_NATIVE, `{"disposition":"returned","scopes":[]}`)
	previous := session.turns["turn"].evidence
	grant := proto.Clone(session.grant).(*agentv1.SessionGrant)
	grant.AuthorityGeneration++
	if err := session.renew(grant); err != nil {
		t.Fatal(err)
	}
	envelope, reply := guestOperation(t, session, "final", agentv1.Operation_METHOD_FINALIZE, `{"result":1}`)
	if envelope == nil || reply != nil || envelope.GetDrainEvidence() == previous || envelope.GetOperationAuthorityGeneration() != grant.AuthorityGeneration || process.proofs != 2 {
		t.Fatal("renewed finalization did not acquire fresh physical evidence")
	}
	previous = envelope.GetDrainEvidence()
	grant = proto.Clone(grant).(*agentv1.SessionGrant)
	grant.AuthorityGeneration++
	if err := session.renew(grant); err != nil {
		t.Fatal(err)
	}
	replay, _ := guestOperation(t, session, "final", agentv1.Operation_METHOD_FINALIZE, `{"result":1}`)
	if replay.GetDrainEvidence() != previous || replay.GetOperationAuthorityGeneration() != grant.AuthorityGeneration-1 || process.proofs != 2 {
		t.Fatal("pending mutation changed authority on replay")
	}
}

func TestAgentSessionRejectsPostSettlementMutationAndLateRetiredReceipt(t *testing.T) {
	session, _ := agentSessionFixture(t)
	guestDispatch(t, session, "turn", 1)
	session.turns["turn"].settled = true
	envelope, reply := guestOperation(t, session, "second-final", agentv1.Operation_METHOD_FINALIZE, `{"result":2}`)
	requireGuestRejected(t, envelope, reply)
	session.pending["old-final"] = &agentPendingOperation{operation: &agentv1.Operation{TurnId: proto.String("turn"), Method: agentv1.Operation_METHOD_FINALIZE}}
	delete(session.turns, "turn")
	guestReceipt(t, session, "old-final", `{"status":"completed","result":1}`)
	if len(session.pending) != 0 {
		t.Fatal("late retired mutation retained")
	}
	if err := session.send(&agentv1.GuestCommand{Identity: session.grant.Identity, DeliveryId: "empty-ack", Command: &agentv1.GuestCommand_Acknowledged{Acknowledged: &agentv1.TurnAcknowledged{}}}); err == nil {
		t.Fatal("empty terminal acknowledgment accepted")
	}
}

func TestAgentSessionPendingOperationsAreBounded(t *testing.T) {
	session, _ := agentSessionFixture(t)
	guestDispatch(t, session, "turn", 1)
	for i := range 256 {
		envelope, reply := guestOperation(t, session, fmt.Sprintf("wait-%d", i), agentv1.Operation_METHOD_WAIT_ASK, `null`)
		if envelope == nil || reply != nil {
			t.Fatal("valid observation rejected")
		}
	}
	envelope, reply := guestOperation(t, session, "overflow", agentv1.Operation_METHOD_WAIT_ASK, `null`)
	requireGuestRejected(t, envelope, reply)
	if len(session.pending) != 256 {
		t.Fatal("pending bound exceeded")
	}
}

func (p *fakeAgentProcess) wait(ctx context.Context) error {
	if p.waitHook != nil {
		return p.waitHook(ctx)
	}
	<-ctx.Done()
	return ctx.Err()
}
func TestAgentSessionOversizedHostCommandCannotDestroyProcess(t *testing.T) {
	session, process := agentSessionFixture(t)
	before := len(process.commands)
	command := &agentv1.GuestCommand{Identity: session.grant.Identity, DeliveryId: "oversized", Command: &agentv1.GuestCommand_Dispatch{Dispatch: &agentv1.TurnDispatch{TurnId: "turn", Sequence: 1, InputJson: make([]byte, maxAgentFrameBytes)}}}
	if err := session.send(command); err == nil {
		t.Fatal("oversized host command admitted")
	}
	if session.activeTurn != "" || session.terminal || len(process.commands) != before {
		t.Fatal("oversized host frame mutated Session")
	}
}

func TestAgentSessionRequiresToolEnvelopeForSessionOperations(t *testing.T) {
	session, _ := agentSessionFixture(t)
	guestDispatch(t, session, "turn", 1)
	for _, method := range []agentv1.Operation_Method{agentv1.Operation_METHOD_START, agentv1.Operation_METHOD_SPAWN, agentv1.Operation_METHOD_ENQUEUE, agentv1.Operation_METHOD_SEND_MESSAGE, agentv1.Operation_METHOD_CONTROL_SESSION, agentv1.Operation_METHOD_WAIT_TURN, agentv1.Operation_METHOD_INSPECT_SESSION, agentv1.Operation_METHOD_LIST_SESSIONS} {
		envelope, reply := guestOperation(t, session, method.String(), method, `{}`)
		requireGuestRejected(t, envelope, reply)
	}
}
