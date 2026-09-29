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

func TestComputerCaptureJoinsTwoWaitsBeforePhysicalCapture(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	c, request, session, _ := newCaptureTest(t)
	registry := &ComputerCaptureRuns{}
	var frozen atomic.Int32
	results := make([]<-chan error, 0, 2)
	for _, member := range request.Target.Capture.Runs {
		host, guest := net.Pipe()
		t.Cleanup(func() { _ = host.Close(); _ = guest.Close() })
		_ = guest.SetDeadline(time.Now().Add(4 * time.Second))
		lease := testRunLeaseAssignment(time.Now().Add(time.Minute))
		lease.ID, lease.RunID, lease.AttemptNumber = member.RunLeaseID, member.RunID, member.AttemptNumber
		lease.ComputerInstanceID, lease.WorkerEpoch = request.Target.ID, request.Target.WorkerEpoch
		lease.ComputerID, lease.WriterGeneration = request.Target.Source.ComputerID, request.Target.Source.WriterGeneration
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
			if pause.RunId != member.RunID || pause.RunWaitId != member.RunWaitID || pause.RunLeaseId != member.RunLeaseID || pause.CheckpointId != request.Target.Capture.CheckpointID || pause.CheckpointRequestVersion != request.Target.DesiredVersion || pause.CorrelationId != wait.CorrelationID {
				t.Errorf("changed member pause: %+v", pause)
				return
			}
			frozen.Add(1)
			if err := wire.WriteCheckpointPauseReady(guest, pause.RunWaitId, pause.CheckpointId); err != nil {
				t.Error(err)
			}
		}()
	}
	excluded := false
	err := registry.Capture(ctx, request.Target, func(ctx context.Context) error {
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
		_, err := c.CreateCheckpoint(ctx, request)
		return err
	}, func(ctx context.Context) error {
		if ctx.Err() != nil {
			t.Fatal("source exclusion inherited cancellation")
		}
		err := c.ReleaseCheckpointSource(ctx)
		excluded = err == nil
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, result := range results {
		select {
		case err := <-result:
			if !errors.Is(err, ErrDetached) || !excluded {
				t.Fatalf("member result=%v excluded=%v", err, excluded)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	if len(session.snapshotRequests) != 1 || session.closeCount != 1 {
		t.Fatalf("physical snapshots=%d closes=%d", len(session.snapshotRequests), session.closeCount)
	}
}

func captureRegistryWait(t *testing.T, registry *ComputerCaptureRuns, target workerapi.RuntimeReconcileTarget, member workerapi.RuntimeCaptureRun) *computerCaptureWait {
	t.Helper()
	lease := testRunLeaseAssignment(time.Now().Add(time.Minute))
	lease.ID, lease.RunID, lease.AttemptNumber = member.RunLeaseID, member.RunID, member.AttemptNumber
	lease.ComputerInstanceID, lease.WorkerEpoch = target.ID, target.WorkerEpoch
	lease.ComputerID, lease.WriterGeneration = target.Source.ComputerID, target.Source.WriterGeneration
	entry, detach, err := registry.register(lease, member.RunWaitID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = detach() })
	return entry
}

func TestComputerCaptureFailureExcludesSourceBeforeReleasingMembers(t *testing.T) {
	for _, stage := range []string{"pause", "publication", "exclusion", "cancel"} {
		t.Run(stage, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			registry, target := &ComputerCaptureRuns{}, checkpointCaptureTarget(1)
			entry := captureRegistryWait(t, registry, target, target.Capture.Runs[0])
			failure := errors.New("failure at " + stage)
			excluded, permitExclusion := make(chan struct{}), make(chan struct{})
			result := make(chan error, 1)
			go func() {
				result <- registry.Capture(ctx, target, func(context.Context) error {
					if stage == "pause" || stage == "cancel" {
						t.Error("capture after failed pause")
					}
					if stage == "publication" {
						return failure
					}
					return nil
				}, func(cleanupCtx context.Context) error {
					if cleanupCtx.Err() != nil {
						t.Error("cancelled cleanup")
					}
					close(excluded)
					<-permitExclusion
					if stage == "exclusion" {
						return failure
					}
					return nil
				})
			}()
			pause := <-entry.requests
			if stage == "pause" {
				pause.ready <- failure
			} else if stage == "cancel" {
				cancel()
			} else {
				pause.ready <- nil
			}
			<-excluded
			select {
			case <-pause.finished:
				t.Fatal("member released before physical exclusion")
			default:
			}
			close(permitExclusion)
			err := <-result
			if stage == "cancel" {
				if !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
			} else if !errors.Is(err, failure) {
				t.Fatal(err)
			}
			<-pause.finished
			if pause.result == nil {
				t.Fatal("failed capture reported member success")
			}
			if _, _, err := registry.register(entry.lease, "new-wait"); err == nil {
				t.Fatal("failed source reopened admission")
			}
		})
	}
}

func TestComputerCaptureRejectsIncompleteOrChangedLocalMembership(t *testing.T) {
	for _, change := range []string{"missing", "extra", "lease", "attempt", "wait", "instance", "epoch", "computer", "writer"} {
		t.Run(change, func(t *testing.T) {
			registry, target := &ComputerCaptureRuns{}, checkpointCaptureTarget(1)
			if change != "missing" {
				captureRegistryWait(t, registry, target, target.Capture.Runs[0])
			}
			switch change {
			case "extra":
				target.Capture.Runs = nil
			case "lease":
				target.Capture.Runs[0].RunLeaseID = "other"
			case "attempt":
				target.Capture.Runs[0].AttemptNumber++
			case "wait":
				target.Capture.Runs[0].RunWaitID = "other"
			case "instance":
				target.ID = "other"
			case "epoch":
				target.WorkerEpoch++
			case "computer":
				target.Source.ComputerID = "other"
			case "writer":
				target.Source.WriterGeneration++
			}
			unexpected := func(context.Context) error { t.Fatal("invalid membership touched physical owner"); return nil }
			if err := registry.Capture(t.Context(), target, unexpected, unexpected); err == nil {
				t.Fatal("invalid capture accepted")
			}
		})
	}
}

func TestComputerCaptureEmptyInstanceAndDuplicateOwner(t *testing.T) {
	registry, target := &ComputerCaptureRuns{}, checkpointCaptureTarget(0)
	order := ""
	if err := registry.Capture(t.Context(), target, func(context.Context) error { order += "capture/"; return nil }, func(context.Context) error { order += "exclude"; return nil }); err != nil {
		t.Fatal(err)
	}
	if order != "capture/exclude" {
		t.Fatal(order)
	}
	unexpected := func(context.Context) error { t.Fatal("duplicate owner touched source"); return nil }
	if err := registry.Capture(t.Context(), target, unexpected, unexpected); err == nil {
		t.Fatal("duplicate capture accepted")
	}
}

func TestComputerMemberPauseRejectsMismatchedReceipt(t *testing.T) {
	host, guest := net.Pipe()
	defer host.Close()
	defer guest.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	target := checkpointCaptureTarget(1)
	member := target.Capture.Runs[0]
	lease := testRunLeaseAssignment(time.Now().Add(time.Minute))
	lease.ID, lease.RunID, lease.AttemptNumber = member.RunLeaseID, member.RunID, member.AttemptNumber
	lease.ComputerInstanceID, lease.WorkerEpoch = target.ID, target.WorkerEpoch
	lease.ComputerID, lease.WriterGeneration = target.Source.ComputerID, target.Source.WriterGeneration
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
	err := task.pauseComputerMember(ctx, WaitRequest{RunWaitID: member.RunWaitID, ResumeAttachID: "attach", CorrelationID: "correlation"}, &computerMemberPause{target: target, member: member})
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
			target := checkpointCaptureTarget(1)
			member := target.Capture.Runs[0]
			host, guest := net.Pipe()
			defer host.Close()
			defer guest.Close()
			_ = guest.SetDeadline(time.Now().Add(4 * time.Second))
			lease := testRunLeaseAssignment(time.Now().Add(time.Minute))
			lease.ID, lease.RunID, lease.AttemptNumber = member.RunLeaseID, member.RunID, member.AttemptNumber
			lease.ComputerInstanceID, lease.WorkerEpoch = target.ID, target.WorkerEpoch
			lease.ComputerID, lease.WriterGeneration = target.Source.ComputerID, target.Source.WriterGeneration
			registry := &ComputerCaptureRuns{}
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
				captured <- registry.Capture(ctx, target,
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
	target := checkpointCaptureTarget(1)
	member := target.Capture.Runs[0]
	host, guest := net.Pipe()
	defer host.Close()
	defer guest.Close()
	lease := testRunLeaseAssignment(time.Now().Add(time.Minute))
	lease.ID, lease.RunID, lease.AttemptNumber = member.RunLeaseID, member.RunID, member.AttemptNumber
	lease.ComputerInstanceID, lease.WorkerEpoch = target.ID, target.WorkerEpoch
	lease.ComputerID, lease.WriterGeneration = target.Source.ComputerID, target.Source.WriterGeneration
	registry := &ComputerCaptureRuns{}
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
	registry.mu.Lock()
	entry := registry.waits[member.RunID]
	registry.mu.Unlock()
	excluded, release := make(chan struct{}), make(chan struct{})
	captured := make(chan error, 1)
	go func() {
		captured <- registry.Capture(ctx, target, func(context.Context) error { t.Error("captured a member that left its wait"); return nil }, func(context.Context) error { close(excluded); <-release; return nil })
	}()
	// The hot-wait owner is blocked in the log event branch. Observe and replace
	// its queued request before letting that branch return through cancellation.
	var pause *computerMemberPause
	select {
	case pause = <-entry.requests:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	entry.requests <- pause
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
