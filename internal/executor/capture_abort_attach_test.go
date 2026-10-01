package executor

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/frameio"
	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"
	programv0 "github.com/helmrdotdev/helmr/internal/proto/program/v0"
	"github.com/helmrdotdev/helmr/internal/wire"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func TestCaptureAbortRebindRetriesLostReadyAfterTransportReset(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	target := memberCaptureTarget(1)
	member := target.Capture.Runs[0]
	lease := memberCaptureLease(target, member)
	lease.BaseComputerDiskVersionID = "version"
	oldHost, oldGuest := net.Pipe()
	defer oldGuest.Close()
	protocol := newProgramProtocol(oldHost)
	defer protocol.Close()
	mounts := newTestMounts()
	authority := freshComputerAuthority(&workerapi.RunLeaseClaimResponse{Lease: lease}, "token", testComputerMount(lease))
	task := &guestRunLeaseTask{lease: lease, authority: authority, mounts: mounts, program: freshProgram{protocol: protocol}}
	paused := make(chan error, 1)
	go func() {
		paused <- task.pauseComputerMember(ctx, WaitRequest{RunWaitID: member.RunWaitID, CorrelationID: "correlation", ResumeAttachID: "attach"}, target, member)
	}()
	h, n, err := wire.ReadStreamFrameHeader(oldGuest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wire.ReadCheckpointPauseRequest(h, oldGuest, n); err != nil {
		t.Fatal(err)
	}
	if err := wire.WriteCheckpointPauseReady(oldGuest, member.RunWaitID, target.Capture.CheckpointID); err != nil {
		t.Fatal(err)
	}
	if err := <-paused; err != nil {
		t.Fatal(err)
	}
	oldGuest.Close()
	grant := workerapi.CaptureAbortMember{RunID: member.RunID, RunWaitID: member.RunWaitID, AttemptNumber: member.AttemptNumber, Lease: lease.Fence(), BaseComputerDiskVersionID: lease.BaseComputerDiskVersionID, ExpiresAt: lease.ExpiresAt}
	var surviving net.Conn
	for sequence := uint64(1); sequence <= 2; sequence++ {
		host, guest := net.Pipe()
		defer host.Close()
		defer guest.Close()
		mounts.add(testComputerMount(lease), fakeGuestSession{stream: host}, "token")
		served := make(chan error, 1)
		go func() {
			header, _, err := wire.ReadStreamFrameHeader(guest)
			if err != nil {
				served <- err
				return
			}
			if header.Type != wire.StreamTypeComputerCaptureAbortAttach {
				t.Errorf("attachment type: %s", header.Type)
			}
			var q computerv0.ComputerCaptureAbortAttachRequest
			if err := frameio.ReadProtoFrame(guest, &q); err != nil {
				served <- err
				return
			}
			if q.AttachSequence != sequence || q.Member.RunLeaseId != lease.ID {
				t.Error("attachment fence changed")
			}
			if sequence == 1 {
				guest.Close()
				served <- nil
				return
			}
			served <- frameio.WriteProtoFrame(guest, &computerv0.ComputerCaptureAbortAttachResponse{CheckpointId: q.CheckpointId, AbortDesiredVersion: q.AbortDesiredVersion, Member: q.Member, AttachSequence: q.AttachSequence})
		}()
		_, seq, err := task.resumeCapturedMember(ctx, target, grant, true)
		if sequence == 1 && err == nil {
			t.Fatal("lost ready accepted")
		}
		if sequence == 2 && (err != nil || seq != 2) {
			t.Fatalf("retry sequence=%d err=%v", seq, err)
		}
		if err := <-served; err != nil {
			t.Fatal(err)
		}
		surviving = guest
	}
	_, seq, err := task.resumeCapturedMember(ctx, target, grant, true)
	if err != nil || seq != 2 {
		t.Fatalf("lost activation replay: %d %v", seq, err)
	}
	wrote := make(chan error, 1)
	go func() {
		wrote <- frameio.WriteProtoFrame(surviving, &programv0.RunEvent{Event: &programv0.RunEvent_RunWaitRequested{RunWaitRequested: &programv0.RunWaitRequested{RunWaitId: "next-wait"}}})
	}()
	var event programv0.RunEvent
	if err := task.program.readEvent(ctx, &event); err != nil {
		t.Fatal(err)
	}
	if event.GetRunWaitRequested().GetRunWaitId() != "next-wait" {
		t.Fatal("new event lost")
	}
	if err := <-wrote; err != nil {
		t.Fatal(err)
	}
}

func TestSessionStopWaitsForCaptureTransportAndDurableAbort(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	oldHost, oldGuest := net.Pipe()
	defer oldGuest.Close()
	protocol := newProgramProtocol(oldHost)
	defer protocol.Close()
	lease := testFreshProgramClaim(t).Lease
	execution := testTurnExecution(lease)
	cp := &sessionProtocolCP{testRunLeaseControlPlane: &testRunLeaseControlPlane{}, control: func(r workerapi.SessionControlRequest) workerapi.SessionControlResponse {
		return workerapi.SessionControlResponse{CorrelationID: r.CorrelationID, HoldID: new("hold"), Reason: new("interrupt_requested")}
	}}
	task := &guestRunLeaseTask{program: freshProgram{protocol: protocol, execution: execution.Session}, lease: lease, controlPlane: testControlPlane(t, cp), capturePaused: true}
	write := make(chan error, 1)
	go func() { write <- wire.WriteCheckpointPauseReady(oldGuest, "wait", "checkpoint") }()
	if err := protocol.takePhysical(ctx, func(context.Context, *programv0.RunEvent) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if _, _, err := wire.ReadStreamFrameHeader(protocol.reader); err != nil {
		t.Fatal(err)
	}
	if err := <-write; err != nil {
		t.Fatal(err)
	}
	oldGuest.Close()
	if deadline, err := task.deliverSessionStopWithCaptureBarrier(ctx, true); err != nil || !deadline.IsZero() {
		t.Fatalf("stop delivered during capture: %v %v", deadline, err)
	}
	newHost, newGuest := net.Pipe()
	defer newGuest.Close()
	if err := protocol.replacePausedStream(ctx, newHost); err != nil {
		t.Fatal(err)
	}
	if deadline, err := task.deliverSessionStopWithCaptureBarrier(ctx, true); err != nil || !deadline.IsZero() {
		t.Fatalf("stop delivered before durable abort: %v %v", deadline, err)
	}
	task.mu.Lock()
	task.capturePaused = false
	task.mu.Unlock()
	done := make(chan error, 1)
	go func() { _, err := task.deliverSessionStop(ctx); done <- err }()
	header, size, err := wire.ReadStreamFrameHeader(newGuest)
	if err != nil {
		t.Fatal(err)
	}
	stop, err := wire.ReadSessionStop(header, newGuest, size)
	if err != nil {
		t.Fatal(err)
	}
	if stop.HoldId != "hold" {
		t.Fatal("wrong stop hold")
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	// Cancelling the completed write's context must not close the replacement.
	go func() { write <- frameio.WriteProtoFrame(newGuest, &programv0.RunEvent{}) }()
	if _, err := protocol.next(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-write; err != nil {
		t.Fatal(err)
	}
}

// A failed non-consuming event must not abandon an already dispatched pause.
type captureLogRetryCP struct {
	*testRunLeaseControlPlane
	calls    int
	sequence uint64
}

func (cp *captureLogRetryCP) AppendRunLog(_ context.Context, _ workerapi.RunLeaseAssignment, _ workerapi.LogStream, sequence uint64, _ []byte) error {
	cp.calls++
	if cp.calls == 1 {
		cp.sequence = sequence
		return errors.New("temporary upload failure")
	}
	if sequence != cp.sequence {
		return errors.New("log sequence changed on retry")
	}
	return nil
}
func TestComputerMemberPauseRetriesDrainedLogBeforeProof(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	target := memberCaptureTarget(1)
	member := target.Capture.Runs[0]
	lease := memberCaptureLease(target, member)
	host, guest := net.Pipe()
	defer guest.Close()
	protocol := newProgramProtocol(host)
	defer protocol.Close()
	cp := &captureLogRetryCP{testRunLeaseControlPlane: &testRunLeaseControlPlane{}}
	task := &guestRunLeaseTask{lease: lease, program: freshProgram{protocol: protocol}, controlPlane: testControlPlane(t, cp)}
	done := make(chan error, 1)
	go func() {
		done <- task.pauseComputerMember(ctx, WaitRequest{RunWaitID: member.RunWaitID, CorrelationID: "correlation", ResumeAttachID: "attach"}, target, member)
	}()
	h, n, err := wire.ReadStreamFrameHeader(guest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wire.ReadCheckpointPauseRequest(h, guest, n); err != nil {
		t.Fatal(err)
	}
	if err := frameio.WriteProtoFrame(guest, &programv0.RunEvent{Event: &programv0.RunEvent_StdoutChunk{StdoutChunk: []byte("before freeze")}}); err != nil {
		t.Fatal(err)
	}
	if err := wire.WriteCheckpointPauseReady(guest, member.RunWaitID, target.Capture.CheckpointID); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if cp.calls != 2 || !task.checkpointFrozen {
		t.Fatalf("calls=%d frozen=%v", cp.calls, task.checkpointFrozen)
	}
}

// The pause owner must finish an in-flight Session response while holding the
// capture gate, even when the independent stop poller is waiting on that gate.
func TestComputerMemberPauseDrainsSessionStopBeforeProof(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	lease := testFreshProgramClaim(t).Lease
	lease.ExpiresAt = time.Now().Add(3 * time.Second)
	execution := testTurnExecution(lease)
	target := memberCaptureTarget(1)
	target.ID, target.WorkerEpoch = lease.ComputerInstanceID, lease.WorkerEpoch
	target.Source.ComputerID, target.Source.WriterGeneration = lease.ComputerID, lease.WriterGeneration
	member := &target.Capture.Runs[0]
	member.RunID, member.RunLeaseID, member.AttemptNumber = lease.RunID, lease.ID, lease.AttemptNumber
	host, guest := net.Pipe()
	defer guest.Close()
	_ = guest.SetDeadline(time.Now().Add(3 * time.Second))
	protocol := newProgramProtocol(host)
	defer protocol.Close()
	polled := make(chan struct{}, 1)
	cp := &sessionProtocolCP{testRunLeaseControlPlane: &testRunLeaseControlPlane{},
		control: func(r workerapi.SessionControlRequest) workerapi.SessionControlResponse {
			select {
			case polled <- struct{}{}:
			default:
			}
			return workerapi.SessionControlResponse{CorrelationID: r.CorrelationID, HoldID: new("hold"), TurnID: &execution.TurnId, Reason: new("interrupt_requested")}
		},
		ready: func(r workerapi.TurnExecutionRequest) workerapi.TurnCommandResponse {
			return workerapi.TurnCommandResponse{CorrelationID: r.CorrelationID, Failed: &workerapi.RuntimeOperationFailure{Code: "turn_stopping", Message: "Turn interruption has been accepted"}}
		},
	}
	task := &guestRunLeaseTask{lease: lease, program: freshProgram{protocol: protocol, execution: execution.Session}, controlPlane: testControlPlane(t, cp)}
	paused := make(chan error, 1)
	go func() {
		paused <- task.pauseComputerMember(ctx, WaitRequest{RunWaitID: member.RunWaitID, CorrelationID: "pause", ResumeAttachID: "attach"}, target, *member)
	}()
	h, n, err := wire.ReadStreamFrameHeader(guest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wire.ReadCheckpointPauseRequest(h, guest, n); err != nil {
		t.Fatal(err)
	}
	stopped := make(chan error, 1)
	go func() { _, err := task.deliverSessionStopWithCaptureBarrier(ctx, true); stopped <- err }()
	select {
	case <-polled:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	correlation := "019c10d5-a6f7-7af1-8f5f-000000000117"
	if err := frameio.WriteProtoFrame(guest, &programv0.RunEvent{Event: &programv0.RunEvent_TurnReadyRequested{TurnReadyRequested: &programv0.TurnReadyRequested{CorrelationId: correlation, Execution: execution}}}); err != nil {
		t.Fatal(err)
	}
	h, n, err = wire.ReadStreamFrameHeader(guest)
	if err != nil {
		t.Fatal(err)
	}
	stop, err := wire.ReadSessionStop(h, guest, n)
	if err != nil || stop.GetHoldId() != "hold" {
		t.Fatalf("stop before rejection: %v %v", stop, err)
	}
	h, n, err = wire.ReadStreamFrameHeader(guest)
	if err != nil {
		t.Fatal(err)
	}
	decision, err := wire.ReadResumeDecision(h, guest, n)
	if err != nil || decision.GetKind() != "failed" || decision.GetCorrelationId() != correlation {
		t.Fatalf("rejection: %v %v", decision, err)
	}
	if err := wire.WriteCheckpointPauseReady(guest, member.RunWaitID, target.Capture.CheckpointID); err != nil {
		t.Fatal(err)
	}
	if err := <-paused; err != nil {
		t.Fatal(err)
	}
	if err := <-stopped; err != nil {
		t.Fatal(err)
	}
	if !task.checkpointFrozen {
		t.Fatal("pause did not finish")
	}
}
