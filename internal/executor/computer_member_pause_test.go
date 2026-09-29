package executor

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/frameio"
	programv0 "github.com/helmrdotdev/helmr/internal/proto/program/v0"
	"github.com/helmrdotdev/helmr/internal/wire"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

// memberCaptureTarget is a capture of count waiting members of one Instance.
func memberCaptureTarget(count int) workerapi.RuntimeReconcileTarget {
	target := workerapi.RuntimeReconcileTarget{ID: "instance", WorkerEpoch: 2, DesiredVersion: 5, Action: workerapi.RuntimeReconcileCapture, Source: workerapi.RuntimeSource{ComputerID: "01912345-6789-7abc-8def-0123456789ab", ComputerSpecID: "spec", WriterGeneration: 3}, Capture: &workerapi.RuntimeCapture{CheckpointID: "checkpoint", MembershipRevision: 7, Runs: []workerapi.RuntimeCaptureRun{}}}
	if count > 0 {
		target.Capture.ProgramDeploymentID = "program"
	}
	for i := range count {
		suffix := string(rune('a' + i))
		target.Capture.Runs = append(target.Capture.Runs, workerapi.RuntimeCaptureRun{RunID: "run-" + suffix, AttemptNumber: 2, RunWaitID: "wait-" + suffix, RunLeaseID: "lease-" + suffix})
	}
	return target
}

// memberCaptureLease is member's local grant on target's Instance.
func memberCaptureLease(target workerapi.RuntimeReconcileTarget, member workerapi.RuntimeCaptureRun) workerapi.RunLeaseAssignment {
	lease := testRunLeaseAssignment(time.Now().Add(time.Minute))
	lease.ID, lease.RunID, lease.AttemptNumber = member.RunLeaseID, member.RunID, member.AttemptNumber
	lease.ComputerInstanceID, lease.WorkerEpoch = target.ID, target.WorkerEpoch
	lease.ComputerID, lease.WriterGeneration = target.Source.ComputerID, target.Source.WriterGeneration
	return lease
}

func TestComputerCaptureJoinsTwoWaitsBeforePhysicalCapture(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	target := memberCaptureTarget(2)
	registry := &CaptureRuns{}
	var frozen atomic.Int32
	results := make([]<-chan error, 0, 2)
	for _, member := range target.Capture.Runs {
		host, guest := net.Pipe()
		t.Cleanup(func() { _ = host.Close(); _ = guest.Close() })
		_ = guest.SetDeadline(time.Now().Add(4 * time.Second))
		lease := memberCaptureLease(target, member)
		task := &guestRunLeaseTask{captures: registry, lease: lease, program: freshProgram{channel: fakeGuestSession{stream: host}, protocol: newProgramProtocol(host)}}
		t.Cleanup(task.Close)
		wait := WaitRequest{RunWaitID: member.RunWaitID, ResumeAttachID: "attach-" + member.RunID, CorrelationID: "correlation-" + member.RunID}
		opened, result := make(chan struct{}), make(chan error, 1)
		results = append(results, result)
		go func() {
			result <- task.runHotWait(ctx, wait, func(ctx context.Context, _ WaitRequest) error { close(opened); <-ctx.Done(); return ctx.Err() })
		}()
		<-opened
		go func() {
			header, size, err := wire.ReadStreamFrameHeader(guest)
			if err != nil {
				t.Error(err)
				return
			}
			pause, err := wire.ReadCheckpointPauseRequest(header, guest, size)
			if err != nil {
				t.Error(err)
				return
			}
			if pause.RunId != member.RunID || pause.RunWaitId != member.RunWaitID || pause.RunLeaseId != member.RunLeaseID || pause.CheckpointId != target.Capture.CheckpointID || pause.CheckpointRequestVersion != target.DesiredVersion || pause.CorrelationId != wait.CorrelationID {
				t.Errorf("changed member pause: %+v", pause)
				return
			}
			frozen.Add(1)
			if err := wire.WriteCheckpointPauseReady(guest, pause.RunWaitId, pause.CheckpointId); err != nil {
				t.Error(err)
			}
		}()
	}
	var captures, exclusions atomic.Int32
	err := captureComputer(ctx, registry, target, func(ctx context.Context) error {
		if frozen.Load() != 2 {
			t.Fatal("physical capture preceded all member receipts")
		}
		for _, result := range results {
			select {
			case err := <-result:
				t.Fatalf("member detached before publication: %v", err)
			default:
			}
		}
		captures.Add(1)
		return nil
	}, func(ctx context.Context) error {
		if ctx.Err() != nil {
			t.Fatal("source exclusion inherited cancellation")
		}
		exclusions.Add(1)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, result := range results {
		select {
		case err := <-result:
			if !errors.Is(err, ErrDetached) || exclusions.Load() != 1 {
				t.Fatalf("member result=%v exclusions=%d", err, exclusions.Load())
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	if captures.Load() != 1 || exclusions.Load() != 1 {
		t.Fatalf("physical captures=%d exclusions=%d", captures.Load(), exclusions.Load())
	}
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

func TestHotWaitCaptureFailureWaitsForPhysicalExclusion(t *testing.T) {
	for _, stage := range []string{"receipt", "cancellation"} {
		t.Run(stage, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			memberCtx, cancelMember := context.WithCancel(ctx)
			defer cancelMember()
			target := memberCaptureTarget(1)
			member := target.Capture.Runs[0]
			host, guest := net.Pipe()
			defer host.Close()
			defer guest.Close()
			_ = guest.SetDeadline(time.Now().Add(4 * time.Second))
			lease := memberCaptureLease(target, member)
			registry := &CaptureRuns{}
			task := &guestRunLeaseTask{captures: registry, lease: lease, program: freshProgram{channel: fakeGuestSession{stream: host}, protocol: newProgramProtocol(host)}}
			defer task.Close()
			opened, waited := make(chan struct{}), make(chan error, 1)
			go func() {
				waited <- task.runHotWait(memberCtx, WaitRequest{RunWaitID: member.RunWaitID, ResumeAttachID: "attach", CorrelationID: "correlation"}, func(ctx context.Context, _ WaitRequest) error { close(opened); <-ctx.Done(); return ctx.Err() })
			}()
			<-opened
			go func() {
				header, n, err := wire.ReadStreamFrameHeader(guest)
				if err != nil {
					return
				}
				_, err = wire.ReadCheckpointPauseRequest(header, guest, n)
				if err != nil {
					return
				}
				if stage == "cancellation" {
					cancelMember()
					return
				}
				_ = wire.WriteCheckpointPauseReady(guest, "wrong-wait", target.Capture.CheckpointID)
			}()
			excluded, release := make(chan struct{}), make(chan struct{})
			captured := make(chan error, 1)
			go func() {
				captured <- captureComputer(ctx, registry, target,
					func(context.Context) error { t.Error("capture after invalid member proof"); return nil },
					func(context.Context) error { close(excluded); <-release; return nil })
			}()
			select {
			case <-excluded:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			select {
			case err := <-waited:
				t.Fatalf("logical member escaped physical exclusion: %v", err)
			default:
			}
			close(release)
			if err := <-captured; err == nil {
				t.Fatal("invalid capture succeeded")
			}
			select {
			case err := <-waited:
				if err == nil || errors.Is(err, ErrDetached) {
					t.Fatalf("failed member became successful: %v", err)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
		})
	}
}

type blockedCaptureLogClient struct {
	RunLeaseControlPlane
	entered chan struct{}
}

func (c *blockedCaptureLogClient) AppendRunLog(ctx context.Context, _ workerapi.RunLeaseAssignment, _ workerapi.LogStream, _ uint64, _ []byte) error {
	close(c.entered)
	<-ctx.Done()
	return ctx.Err()
}

func TestHotWaitQueuedCaptureJoinsExclusionWhenEventBranchWins(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	memberCtx, cancelMember := context.WithCancel(ctx)
	defer cancelMember()
	target := memberCaptureTarget(1)
	member := target.Capture.Runs[0]
	host, guest := net.Pipe()
	defer host.Close()
	defer guest.Close()
	lease := memberCaptureLease(target, member)
	registry := &CaptureRuns{}
	logs := &blockedCaptureLogClient{entered: make(chan struct{})}
	task := &guestRunLeaseTask{captures: registry, lease: lease, controlPlane: testControlPlane(t, logs), program: freshProgram{channel: fakeGuestSession{stream: host}, protocol: newProgramProtocol(host)}}
	defer task.Close()
	opened, waited := make(chan struct{}), make(chan error, 1)
	go func() {
		waited <- task.runHotWait(memberCtx, WaitRequest{RunWaitID: member.RunWaitID, ResumeAttachID: "attach", CorrelationID: "correlation"}, func(ctx context.Context, _ WaitRequest) error { close(opened); <-ctx.Done(); return ctx.Err() })
	}()
	<-opened
	if err := frameio.WriteProtoFrame(guest, &programv0.RunEvent{Event: &programv0.RunEvent_StdoutChunk{StdoutChunk: []byte("pending log")}}); err != nil {
		t.Fatal(err)
	}
	<-logs.entered
	excluded, release := make(chan struct{}), make(chan struct{})
	captured := make(chan error, 1)
	go func() {
		captured <- captureComputer(ctx, registry, target, func(context.Context) error { t.Error("captured a member that left its wait"); return nil }, func(context.Context) error { close(excluded); <-release; return nil })
	}()
	// The hot-wait owner is blocked in the log event branch. Wait until the
	// capture has dispatched its pause to that wait before letting the branch
	// return through cancellation.
	if err := awaitCaptureDispatch(ctx, registry, member.RunID); err != nil {
		t.Fatal(err)
	}
	cancelMember()
	select {
	case <-excluded:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	select {
	case err := <-waited:
		t.Fatalf("queued capture escaped exclusion: %v", err)
	default:
	}
	close(release)
	if err := <-captured; err == nil {
		t.Fatal("abandoned member was captured")
	}
	select {
	case err := <-waited:
		if err == nil || errors.Is(err, ErrDetached) {
			t.Fatalf("invalid member result: %v", err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}
