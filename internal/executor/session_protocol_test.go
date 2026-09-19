package executor

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/frameio"
	programv0 "github.com/helmrdotdev/helmr/internal/proto/program/v0"
	"github.com/helmrdotdev/helmr/internal/wire"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"google.golang.org/protobuf/proto"
)

type sessionProtocolCP struct {
	*testRunLeaseControlPlane
	SessionExecutionControlPlane
	ready   func(workerapi.TurnExecutionRequest) workerapi.TurnCommandResponse
	control func(workerapi.SessionControlRequest) workerapi.SessionControlResponse
}

func (c *sessionProtocolCP) TurnMessagesReady(_ context.Context, r workerapi.TurnExecutionRequest) (workerapi.TurnCommandResponse, error) {
	return c.ready(r), nil
}
func (c *sessionProtocolCP) ReadSessionControl(_ context.Context, r workerapi.SessionControlRequest) (workerapi.SessionControlResponse, error) {
	return c.control(r), nil
}

func TestHotWaitServesTurnCommandsAndKeepsFollowingEvent(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	host, guest := net.Pipe()
	defer guest.Close()
	lease := testFreshProgramClaim(t).Lease
	execution := testTurnExecution(lease)
	cp := &sessionProtocolCP{testRunLeaseControlPlane: &testRunLeaseControlPlane{}, ready: func(r workerapi.TurnExecutionRequest) workerapi.TurnCommandResponse {
		if r.Lease != lease.Fence() || r.TurnID != execution.TurnId || r.RunGeneration != execution.Session.RunGeneration {
			t.Errorf("wrong scoped request: %+v", r)
		}
		return workerapi.TurnCommandResponse{CorrelationID: r.CorrelationID, Accepted: true}
	}}
	protocol := newProgramProtocol(host)
	defer protocol.Close()
	task := &guestRunLeaseTask{program: freshProgram{session: fakeGuestSession{stream: host}, protocol: protocol, execution: execution.Session}, lease: lease, controlPlane: cp}
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- task.runHotWait(ctx, WaitRequest{}, func(ctx context.Context, _ WaitRequest) error {
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	}()
	correlation := "019c10d5-a6f7-7af1-8f5f-000000000119"
	if err := frameio.WriteProtoFrame(guest, &programv0.RunEvent{Event: &programv0.RunEvent_TurnReadyRequested{TurnReadyRequested: &programv0.TurnReadyRequested{CorrelationId: correlation, Execution: execution}}}); err != nil {
		t.Fatal(err)
	}
	header, size, err := wire.ReadStreamFrameHeader(guest)
	if err != nil {
		t.Fatal(err)
	}
	decision, err := wire.ReadResumeDecision(header, guest, size)
	if err != nil {
		t.Fatal(err)
	}
	if decision.GetKind() != "completed" || decision.GetCorrelationId() != correlation {
		t.Fatalf("decision=%v", decision)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	next := &programv0.RunEvent{Event: &programv0.RunEvent_StdoutChunk{StdoutChunk: []byte("after wait")}}
	go func() { _ = frameio.WriteProtoFrame(guest, next) }()
	var observed programv0.RunEvent
	if err := task.program.readEvent(ctx, &observed); err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(next, &observed) {
		t.Fatalf("following event lost: %v", &observed)
	}
}

type protocolCheckpointer struct{ task *guestRunLeaseTask }

func (c protocolCheckpointer) ReleaseCheckpointSource(context.Context) error { return nil }
func (c protocolCheckpointer) CreateCheckpoint(ctx context.Context, r CheckpointRequest) (CheckpointResult, error) {
	if err := wire.WriteCheckpointPauseRequest(c.task.programStream(), &programv0.CheckpointPauseRequest{RunWaitId: r.RunWaitID, CheckpointId: r.CheckpointID}); err != nil {
		return CheckpointResult{}, err
	}
	if err := c.task.program.protocol.takePhysical(ctx, c.task.processCheckpointRunEvent); err != nil {
		return CheckpointResult{}, err
	}
	h, n, err := wire.ReadStreamFrameHeader(c.task.program.protocol.reader)
	if err != nil {
		return CheckpointResult{}, err
	}
	if h.Type != wire.StreamTypeCheckpointPauseReady || h.RunWaitID != r.RunWaitID || h.CheckpointID != r.CheckpointID || n != 0 {
		return CheckpointResult{}, errors.New("incorrect physical pause receipt")
	}
	return CheckpointResult{}, nil
}
func TestHotWaitTransfersOneReaderToPhysicalCheckpoint(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	host, guest := net.Pipe()
	defer guest.Close()
	protocol := newProgramProtocol(host)
	defer protocol.Close()
	task := &guestRunLeaseTask{program: freshProgram{session: fakeGuestSession{stream: host}, protocol: protocol}}
	task.checkpointer = protocolCheckpointer{task: task}
	done := make(chan error, 1)
	go func() {
		done <- task.runHotWait(ctx, WaitRequest{Checkpointer: task.checkpointer}, func(ctx context.Context, r WaitRequest) error {
			_, err := r.Checkpointer.CreateCheckpoint(ctx, CheckpointRequest{RunWaitID: "wait", CheckpointID: "checkpoint"})
			return err
		})
	}()
	h, n, err := wire.ReadStreamFrameHeader(guest)
	if err != nil {
		t.Fatal(err)
	}
	request, err := wire.ReadCheckpointPauseRequest(h, guest, n)
	if err != nil {
		t.Fatal(err)
	}
	if err := wire.WriteCheckpointPauseReady(guest, request.GetRunWaitId(), request.GetCheckpointId()); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestSessionStopPrecedesCancelledWaitAndKeepsFirstDeadline(t *testing.T) {
	host, guest := net.Pipe()
	defer host.Close()
	defer guest.Close()
	lease := testFreshProgramClaim(t).Lease
	lease.ExpiresAt = time.Now().Add(time.Minute)
	execution := testTurnExecution(lease)
	hold := "019c10d5-a6f7-7af1-8f5f-000000000118"
	reason := "interrupt_requested"
	reads := 0
	cp := &sessionProtocolCP{testRunLeaseControlPlane: &testRunLeaseControlPlane{}, control: func(r workerapi.SessionControlRequest) workerapi.SessionControlResponse {
		reads++
		return workerapi.SessionControlResponse{CorrelationID: r.CorrelationID, HoldID: &hold, TurnID: &execution.TurnId, Reason: &reason}
	}}
	task := &guestRunLeaseTask{program: freshProgram{session: fakeGuestSession{stream: host}, execution: execution.Session}, lease: lease, controlPlane: cp}
	done := make(chan error, 1)
	go func() {
		done <- task.beforeWaitResume(t.Context(), WaitResumeDecision{Kind: "cancelled", Data: json.RawMessage(`{"reason_code":"session_stopped"}`)})
	}()
	header, n, err := wire.ReadStreamFrameHeader(guest)
	if err != nil {
		t.Fatal(err)
	}
	stop, err := wire.ReadSessionStop(header, guest, n)
	if err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(stop.GetExecution(), execution.Session) || stop.GetHoldId() != hold || stop.GetTurnId() != execution.TurnId {
		t.Fatalf("stop=%v", stop)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	task.lease.ExpiresAt = lease.ExpiresAt.Add(time.Minute)
	deadline, err := task.deliverSessionStop(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !deadline.Equal(lease.ExpiresAt) || reads != 1 {
		t.Fatalf("stop was extended/reissued: %v reads=%d", deadline, reads)
	}
}

func TestTurnScopeRejectsGenerationAndNullTurnBeforeCP(t *testing.T) {
	lease := testFreshProgramClaim(t).Lease
	execution := testTurnExecution(lease)
	task := &guestRunLeaseTask{program: freshProgram{execution: execution.Session}, lease: lease}
	stale := proto.Clone(execution).(*programv0.TurnExecution)
	stale.Session.RunGeneration++
	for _, scope := range []*programv0.TurnExecution{nil, stale, {Session: execution.Session}} {
		if _, err := task.turnRequest("019c10d5-a6f7-7af1-8f5f-000000000117", scope); err == nil {
			t.Fatalf("accepted invalid scope %v", scope)
		}
	}
}

func TestRestoredActorScopeRequiresExactGenerationAndTurnShape(t *testing.T) {
	claim := testRestoredProgramClaim(t)
	restore := claim.Execution.Restore
	restore.EntrypointKind = "actor"
	restore.SessionID = "019c10d5-a6f7-7af1-8f5f-000000000111"
	restore.RunGeneration = 7
	restore.TurnID = new("019c10d5-a6f7-7af1-8f5f-000000000112")
	admission, err := validateResumedProgramClaim(&claim)
	if err != nil {
		t.Fatal(err)
	}
	if admission.execution.GetRunGeneration() != 7 || admission.execution.GetRunId() != claim.Lease.RunID || admission.turnID == nil || *admission.turnID != *restore.TurnID {
		t.Fatalf("restore scope=%+v", admission)
	}
	restore.RunGeneration = 0
	if _, err := validateResumedProgramClaim(&claim); err == nil {
		t.Fatal("missing generation accepted")
	}
	restore.RunGeneration = 7
	restore.TurnID = new("")
	if _, err := validateResumedProgramClaim(&claim); err == nil {
		t.Fatal("empty Turn accepted")
	}
	restore.TurnID = nil
	if _, err := validateResumedProgramClaim(&claim); err != nil {
		t.Fatalf("outside-Turn restore rejected: %v", err)
	}
	restore.EntrypointKind = "task"
	if _, err := validateResumedProgramClaim(&claim); err == nil {
		t.Fatal("Task accepted Actor scope")
	}
}

func TestSessionStopBlockedWriteUsesCapturedLeaseDeadline(t *testing.T) {
	host, guest := net.Pipe()
	defer host.Close()
	defer guest.Close()
	lease := testFreshProgramClaim(t).Lease
	lease.ExpiresAt = time.Now().Add(60 * time.Millisecond)
	execution := testTurnExecution(lease)
	cp := &sessionProtocolCP{testRunLeaseControlPlane: &testRunLeaseControlPlane{}, control: func(r workerapi.SessionControlRequest) workerapi.SessionControlResponse {
		return workerapi.SessionControlResponse{CorrelationID: r.CorrelationID, HoldID: new("hold"), Reason: new("interrupt_requested")}
	}}
	task := &guestRunLeaseTask{program: freshProgram{session: fakeGuestSession{stream: host}, execution: execution.Session}, lease: lease, controlPlane: cp}
	start := time.Now()
	if _, err := task.deliverSessionStop(t.Context()); err == nil {
		t.Fatal("blocked stop write succeeded")
	}
	if time.Since(start) > time.Second {
		t.Fatal("blocked stop write exceeded bounded grace")
	}
	var b [1]byte
	if _, err := guest.Read(b[:]); err == nil {
		t.Fatal("expired stop stream remained writable")
	}
}

func TestHotWaitKeepsNextOutcomeWhileResumeAcknowledgementIsPending(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	host, guest := net.Pipe()
	defer guest.Close()
	protocol := newProgramProtocol(host)
	defer protocol.Close()
	task := &guestRunLeaseTask{program: freshProgram{session: fakeGuestSession{stream: host}, protocol: protocol}}
	resumeStarted := make(chan struct{})
	ack := make(chan struct{})
	done := make(chan error, 1)
	request := WaitRequest{Resume: func(context.Context, WaitResumeDecision) error { close(resumeStarted); return nil }}
	go func() {
		done <- task.runHotWait(ctx, request, func(ctx context.Context, r WaitRequest) error {
			if err := r.Resume(ctx, WaitResumeDecision{Kind: "completed"}); err != nil {
				return err
			}
			select {
			case <-ack:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	}()
	<-resumeStarted
	next := &programv0.RunEvent{Event: &programv0.RunEvent_ActorOutcome{ActorOutcome: &programv0.ActorOutcome{RunGeneration: 1, Outcome: &programv0.ActorOutcome_Succeeded{Succeeded: &programv0.ActorSucceeded{}}}}}
	if err := frameio.WriteProtoFrame(guest, next); err != nil {
		t.Fatal(err)
	}
	close(ack)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	var observed programv0.RunEvent
	if err := task.program.readEvent(ctx, &observed); err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(next, &observed) {
		t.Fatalf("following outcome lost: %v", &observed)
	}
}

func TestProtocolCloseUnblocksIdleActorReader(t *testing.T) {
	host, guest := net.Pipe()
	defer guest.Close()
	protocol := newProgramProtocol(host)
	result := make(chan error, 1)
	go func() { _, err := protocol.next(t.Context()); result <- err }()
	if err := protocol.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("closed stream read succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("forced cleanup left reader blocked")
	}
}
