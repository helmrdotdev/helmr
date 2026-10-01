package computerhost_test

import (
	"bufio"
	"context"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/computerhost"
	"github.com/helmrdotdev/helmr/internal/executor"
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

// memberCaptureMount is target's restored Instance.
func memberCaptureMount(target workerapi.InstanceReconcileTarget) workerapi.ComputerInstanceAssignment {
	return workerapi.ComputerInstanceAssignment{ComputerInstanceID: target.ID, ComputerID: target.Source.ComputerID, WriterGeneration: target.Source.WriterGeneration, RestoreCheckpointID: "restored-checkpoint", DesiredVersion: 4, WorkerEpoch: target.WorkerEpoch, VMPlatformID: "platform", GuestChannelCredential: "channel", Target: workerapi.ComputerMountTarget{BaseComputerDiskVersionID: "disk"}}
}

// memberCaptureClaim is member's restored claim on target's Instance, whose
// restored wait is the one the capture pauses.
func memberCaptureClaim(target workerapi.InstanceReconcileTarget, member workerapi.InstanceCaptureRun) *workerapi.RunLeaseClaimResponse {
	lease := workerapi.RunLeaseAssignment{ID: member.RunLeaseID, RunID: member.RunID, AttemptNumber: member.AttemptNumber, LeaseSequence: 2, ComputerInstanceID: target.ID, ComputerID: target.Source.ComputerID, WriterGeneration: target.Source.WriterGeneration, WorkerEpoch: target.WorkerEpoch, WorkerHostID: "worker", VMPlatformID: "platform", BaseComputerDiskVersionID: "disk", ExpiresAt: time.Now().Add(time.Minute)}
	return restoredClaim(memberCaptureMount(target), lease, member.RunWaitID, "task")
}

// memberCaptureRun mounts target's restored Instance on h and starts each
// member's Run in its reattached hot wait. Each result channel receives that
// Run's Wait result under ctx.
func memberCaptureRun(ctx context.Context, t *testing.T, h *restoredProgramHarness, target workerapi.InstanceReconcileTarget, registry *computerhost.CaptureRuns, claims []*workerapi.RunLeaseClaimResponse, fakes ...any) []<-chan error {
	t.Helper()
	mounts := computerhost.NewMounts()
	unregister, err := computerhost.MountComputer(ctx, mounts, h, h, memberCaptureMount(target))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(unregister)
	runner := executor.ProgramRunner{ControlPlane: testControlPlane(t, append(fakes, h)...), Mounts: mounts, CAS: unusedCAS{}, ComputerCaptures: registry}
	results := make([]<-chan error, 0, len(claims))
	for _, claim := range claims {
		task, err := runner.StartRunLeaseTask(ctx, claim)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(task.Close)
		result := make(chan error, 1)
		results = append(results, result)
		go func() {
			_, err := task.Wait(ctx)
			result <- err
		}()
		if err := h.awaitPolled(ctx, claim.Lease.RunID); err != nil {
			t.Fatal(err)
		}
	}
	return results
}

func TestComputerCaptureJoinsTwoWaitsBeforePhysicalCapture(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	target := memberCaptureTarget(2)
	registry := &computerhost.CaptureRuns{}
	claims := []*workerapi.RunLeaseClaimResponse{memberCaptureClaim(target, target.Capture.Runs[0]), memberCaptureClaim(target, target.Capture.Runs[1])}
	h := newRestoredProgramHarness(memberCaptureMount(target), claims...)
	var frozen atomic.Int32
	h.program = func(guest net.Conn, reader *bufio.Reader, attach *programv0.ResumeAttach) error {
		_ = guest.SetDeadline(time.Now().Add(4 * time.Second))
		header, size, err := wire.ReadStreamFrameHeader(reader)
		if err != nil {
			return err
		}
		pause, err := wire.ReadCheckpointPauseRequest(header, reader, size)
		if err != nil {
			return err
		}
		var member workerapi.InstanceCaptureRun
		for _, run := range target.Capture.Runs {
			if run.RunID == attach.RunId {
				member = run
			}
		}
		if pause.RunId != member.RunID || pause.RunWaitId != member.RunWaitID || pause.RunLeaseId != member.RunLeaseID || pause.CheckpointId != target.Capture.CheckpointID || pause.CheckpointRequestVersion != target.DesiredVersion || pause.CorrelationId != attach.CorrelationId || pause.ResumeAttachId != attach.ResumeAttachId {
			return errors.New("changed member pause")
		}
		frozen.Add(1)
		return wire.WriteCheckpointPauseReady(guest, pause.RunWaitId, pause.CheckpointId)
	}
	results := memberCaptureRun(ctx, t, h, target, registry, claims)
	var captures, exclusions atomic.Int32
	err := computerhost.CaptureComputer(ctx, registry, target, func(ctx context.Context) error {
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
			if !errors.Is(err, executor.ErrDetached) || exclusions.Load() != 1 {
				t.Fatalf("member result=%v exclusions=%d", err, exclusions.Load())
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	if captures.Load() != 1 || exclusions.Load() != 1 {
		t.Fatalf("physical captures=%d exclusions=%d", captures.Load(), exclusions.Load())
	}
	// The restore installation and activation, and each member's grant and
	// Program stream, were served cleanly.
	for range 2 + 2*len(claims) {
		select {
		case err := <-h.err:
			if err != nil {
				t.Fatal(err)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
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
			claim := memberCaptureClaim(target, target.Capture.Runs[0])
			registry := &computerhost.CaptureRuns{}
			h := newRestoredProgramHarness(memberCaptureMount(target), claim)
			h.program = func(guest net.Conn, reader *bufio.Reader, _ *programv0.ResumeAttach) error {
				_ = guest.SetDeadline(time.Now().Add(4 * time.Second))
				header, n, err := wire.ReadStreamFrameHeader(reader)
				if err != nil {
					return nil
				}
				if _, err := wire.ReadCheckpointPauseRequest(header, reader, n); err != nil {
					return nil
				}
				if stage == "cancellation" {
					cancelMember()
					return nil
				}
				_ = wire.WriteCheckpointPauseReady(guest, "wrong-wait", target.Capture.CheckpointID)
				return nil
			}
			mounted := computerhost.NewMounts()
			unregister, err := computerhost.MountComputer(ctx, mounted, h, h, memberCaptureMount(target))
			if err != nil {
				t.Fatal(err)
			}
			defer unregister()
			task, err := (executor.ProgramRunner{ControlPlane: testControlPlane(t, h), Mounts: mounted, CAS: unusedCAS{}, ComputerCaptures: registry}).StartRunLeaseTask(ctx, claim)
			if err != nil {
				t.Fatal(err)
			}
			defer task.Close()
			waited := make(chan error, 1)
			go func() {
				_, err := task.Wait(memberCtx)
				waited <- err
			}()
			if err := h.awaitPolled(ctx, claim.Lease.RunID); err != nil {
				t.Fatal(err)
			}
			excluded, release := make(chan struct{}), make(chan struct{})
			captured := make(chan error, 1)
			go func() {
				captured <- computerhost.CaptureComputer(ctx, registry, target,
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
				if err == nil || errors.Is(err, executor.ErrDetached) {
					t.Fatalf("failed member became successful: %v", err)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
		})
	}
}

type blockedCaptureLogClient struct {
	executor.RunLeaseControlPlane
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
	claim := memberCaptureClaim(target, member)
	registry := &computerhost.CaptureRuns{}
	logs := &blockedCaptureLogClient{entered: make(chan struct{})}
	h := newRestoredProgramHarness(memberCaptureMount(target), claim)
	// The Program logs during its hot wait and then only drains its stream.
	logged := make(chan struct{})
	h.program = func(guest net.Conn, reader *bufio.Reader, _ *programv0.ResumeAttach) error {
		<-logged
		if err := frameio.WriteProtoFrame(guest, &programv0.RunEvent{Event: &programv0.RunEvent_StdoutChunk{StdoutChunk: []byte("pending log")}}); err != nil {
			return err
		}
		_, _ = reader.WriteTo(discard{})
		return nil
	}
	mounted := computerhost.NewMounts()
	unregister, err := computerhost.MountComputer(ctx, mounted, h, h, memberCaptureMount(target))
	if err != nil {
		t.Fatal(err)
	}
	defer unregister()
	task, err := (executor.ProgramRunner{ControlPlane: testControlPlane(t, logs, h), Mounts: mounted, CAS: unusedCAS{}, ComputerCaptures: registry}).StartRunLeaseTask(ctx, claim)
	if err != nil {
		t.Fatal(err)
	}
	defer task.Close()
	waited := make(chan error, 1)
	go func() {
		_, err := task.Wait(memberCtx)
		waited <- err
	}()
	if err := h.awaitPolled(ctx, member.RunID); err != nil {
		t.Fatal(err)
	}
	close(logged)
	<-logs.entered
	excluded, release := make(chan struct{}), make(chan struct{})
	captured := make(chan error, 1)
	go func() {
		captured <- computerhost.CaptureComputer(ctx, registry, target, func(context.Context) error { t.Error("captured a member that left its wait"); return nil }, func(context.Context) error { close(excluded); <-release; return nil })
	}()
	// The hot-wait owner is blocked in the log event branch. Wait until the
	// capture has dispatched its pause to that wait before letting the branch
	// return through cancellation.
	if err := computerhost.AwaitCaptureDispatch(ctx, registry, member.RunID); err != nil {
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
		if err == nil || errors.Is(err, executor.ErrDetached) {
			t.Fatalf("invalid member result: %v", err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }
