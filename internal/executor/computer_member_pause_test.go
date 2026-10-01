package executor

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/frameio"
	programv0 "github.com/helmrdotdev/helmr/internal/proto/program/v0"
	"github.com/helmrdotdev/helmr/internal/wire"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

// memberCaptureTarget is a capture of count waiting members of one Instance.
func memberCaptureTarget(count int) workerapi.InstanceReconcileTarget {
	target := workerapi.InstanceReconcileTarget{ID: "instance", WorkerEpoch: 2, DesiredVersion: 5, Action: workerapi.InstanceReconcileCapture, Source: workerapi.InstanceSource{ComputerID: "01912345-6789-7abc-8def-0123456789ab", ComputerSpecID: "spec", WriterGeneration: 3}, Capture: &workerapi.InstanceCapture{CheckpointID: "checkpoint", MembershipRevision: 7, Runs: []workerapi.InstanceCaptureRun{}}}
	if count > 0 {
		target.Capture.ProgramDeploymentID = "program"
	}
	for i := range count {
		suffix := string(rune('a' + i))
		target.Capture.Runs = append(target.Capture.Runs, workerapi.InstanceCaptureRun{RunID: "run-" + suffix, AttemptNumber: 2, RunWaitID: "wait-" + suffix, RunLeaseID: "lease-" + suffix})
	}
	return target
}

// memberCaptureLease is member's local grant on target's Instance.
func memberCaptureLease(target workerapi.InstanceReconcileTarget, member workerapi.InstanceCaptureRun) workerapi.RunLeaseAssignment {
	lease := testRunLeaseAssignment(time.Now().Add(time.Minute))
	lease.ID, lease.RunID, lease.AttemptNumber = member.RunLeaseID, member.RunID, member.AttemptNumber
	lease.ComputerInstanceID, lease.WorkerEpoch = target.ID, target.WorkerEpoch
	lease.ComputerID, lease.WriterGeneration = target.Source.ComputerID, target.Source.WriterGeneration
	return lease
}

func TestComputerMemberPauseRejectsMismatchedReceipt(t *testing.T) {
	host, guest := net.Pipe()
	defer host.Close()
	defer guest.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	target := memberCaptureTarget(1)
	member := target.Capture.Runs[0]
	lease := memberCaptureLease(target, member)
	task := &guestRunLeaseTask{lease: lease, program: freshProgram{protocol: newProgramProtocol(host)}}
	defer task.Close()
	go func() {
		header, n, err := wire.ReadStreamFrameHeader(guest)
		if err != nil {
			return
		}
		_, err = wire.ReadCheckpointPauseRequest(header, guest, n)
		if err == nil {
			_ = wire.WriteCheckpointPauseReady(guest, "other-wait", target.Capture.CheckpointID)
		}
	}()
	err := task.pauseComputerMember(ctx, WaitRequest{RunWaitID: member.RunWaitID, ResumeAttachID: "attach", CorrelationID: "correlation"}, target, member)
	if err == nil || task.checkpointFrozen {
		t.Fatalf("err=%v frozen=%v", err, task.checkpointFrozen)
	}
}

func TestComputerMemberPauseJoinsReceiptAfterCaptureCancellation(t *testing.T) {
	host, guest := net.Pipe()
	defer guest.Close()
	target := memberCaptureTarget(1)
	member := target.Capture.Runs[0]
	lease := memberCaptureLease(target, member)
	lease.ExpiresAt = time.Now().Add(3 * time.Second)
	protocol := newProgramProtocol(host)
	defer protocol.Close()
	task := &guestRunLeaseTask{lease: lease, program: freshProgram{protocol: protocol}}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- task.pauseComputerMember(ctx, WaitRequest{RunWaitID: member.RunWaitID, ResumeAttachID: "attach", CorrelationID: "correlation"}, target, member)
	}()
	header, n, err := wire.ReadStreamFrameHeader(guest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wire.ReadCheckpointPauseRequest(header, guest, n); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case err := <-done:
		t.Fatalf("pause abandoned before receipt: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	if task.renewalGate.TryLock() {
		task.renewalGate.Unlock()
		t.Fatal("abort activation can overtake pending pause")
	}
	if err := wire.WriteCheckpointPauseReady(guest, member.RunWaitID, target.Capture.CheckpointID); err != nil {
		t.Fatalf("capture cancellation closed original stream: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !task.checkpointFrozen {
		t.Fatal("late receipt did not record frozen member")
	}
	// The physical frame has been fully consumed. Releasing the same reader
	// must permit subsequent protocol events on the original connection.
	protocol.resume <- struct{}{}
	writeDone := make(chan error, 1)
	go func() { writeDone <- frameio.WriteProtoFrame(guest, &programv0.RunEvent{}) }()
	readCtx, stop := context.WithTimeout(t.Context(), time.Second)
	defer stop()
	if err := task.program.readEvent(readCtx, new(programv0.RunEvent)); err != nil {
		t.Fatal(err)
	}
	if err := <-writeDone; err != nil {
		t.Fatal(err)
	}
}

func TestComputerMemberPauseStalledReceiptEndsAtGrantExpiry(t *testing.T) {
	host, guest := net.Pipe()
	defer guest.Close()
	target := memberCaptureTarget(1)
	member := target.Capture.Runs[0]
	lease := memberCaptureLease(target, member)
	lease.ExpiresAt = time.Now().Add(50 * time.Millisecond)
	protocol := newProgramProtocol(host)
	defer protocol.Close()
	task := &guestRunLeaseTask{lease: lease, program: freshProgram{protocol: protocol}}
	done := make(chan error, 1)
	go func() {
		done <- task.pauseComputerMember(t.Context(), WaitRequest{RunWaitID: member.RunWaitID, ResumeAttachID: "attach", CorrelationID: "correlation"}, target, member)
	}()
	select {
	case err := <-done:
		if err == nil || task.checkpointFrozen {
			t.Fatalf("err=%v frozen=%v", err, task.checkpointFrozen)
		}
	case <-time.After(time.Second):
		t.Fatal("grant expiry did not release stalled pause write")
	}
}
