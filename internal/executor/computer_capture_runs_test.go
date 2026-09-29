package executor

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func captureRegistryWait(t *testing.T, registry *CaptureRuns, target workerapi.RuntimeReconcileTarget, member workerapi.RuntimeCaptureRun) *CaptureWait {
	t.Helper()
	lease := workerapi.RunLeaseAssignment{LeaseSequence: 1, ExpiresAt: time.Now().Add(time.Minute).UTC()}
	lease.ID, lease.RunID, lease.AttemptNumber = member.RunLeaseID, member.RunID, member.AttemptNumber
	lease.ComputerInstanceID, lease.WorkerEpoch = target.ID, target.WorkerEpoch
	lease.ComputerID, lease.WriterGeneration = target.Source.ComputerID, target.Source.WriterGeneration
	entry, err := registry.Register(lease, member.RunWaitID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = entry.Detach() })
	return entry
}

// Two waiting members are paused before the real checkpointer snapshots the
// source once; they are released as detached only after the source has been
// closed exactly once.
func TestComputerCaptureSnapshotsOnceAndReleasesMembersAfterSourceRelease(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	c, request, session, _ := newCaptureTest(t)
	registry := &CaptureRuns{}
	var paused atomic.Int32
	results := make([]chan error, 0, len(request.Target.Capture.Runs))
	for _, member := range request.Target.Capture.Runs {
		wait := captureRegistryWait(t, registry, request.Target, member)
		result := make(chan error, 1)
		results = append(results, result)
		go func() {
			select {
			case pause := <-wait.Pauses():
				paused.Add(1)
				result <- pause.Settle(nil)
			case <-ctx.Done():
				result <- ctx.Err()
			}
		}()
	}
	released := false
	err := registry.capture(ctx, request.Target, func(ctx context.Context) error {
		if paused.Load() != 2 {
			t.Fatal("physical capture preceded all member pauses")
		}
		for _, result := range results {
			select {
			case err := <-result:
				t.Fatalf("member released before publication: %v", err)
			default:
			}
		}
		_, err := c.CreateCheckpoint(ctx, request)
		return err
	}, func(ctx context.Context) error {
		if ctx.Err() != nil {
			t.Fatal("source exclusion inherited cancellation")
		}
		for _, result := range results {
			select {
			case err := <-result:
				t.Fatalf("member released before source release: %v", err)
			default:
			}
		}
		err := c.ReleaseCheckpointSource(ctx)
		released = err == nil
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, result := range results {
		select {
		case err := <-result:
			if err != nil || !released {
				t.Fatalf("member result=%v released=%v", err, released)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	if len(session.snapshotRequests) != 1 || session.closeCount != 1 {
		t.Fatalf("physical snapshots=%d closes=%d", len(session.snapshotRequests), session.closeCount)
	}
}

func TestComputerCaptureFailureExcludesSourceBeforeReleasingMembers(t *testing.T) {
	for _, stage := range []string{"pause", "publication", "exclusion", "cancel"} {
		t.Run(stage, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			registry, target := &CaptureRuns{}, checkpointCaptureTarget(1)
			entry := captureRegistryWait(t, registry, target, target.Capture.Runs[0])
			failure := errors.New("failure at " + stage)
			excluded, permitExclusion := make(chan struct{}), make(chan struct{})
			result := make(chan error, 1)
			go func() {
				result <- registry.capture(ctx, target, func(context.Context) error {
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
			if _, err := registry.Register(entry.lease, "new-wait"); err == nil {
				t.Fatal("failed source reopened admission")
			}
		})
	}
}

func TestComputerCaptureRejectsIncompleteOrChangedLocalMembership(t *testing.T) {
	for _, change := range []string{"missing", "extra", "lease", "attempt", "wait", "instance", "epoch", "computer", "writer"} {
		t.Run(change, func(t *testing.T) {
			registry, target := &CaptureRuns{}, checkpointCaptureTarget(1)
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
			if err := registry.capture(t.Context(), target, unexpected, unexpected); err == nil {
				t.Fatal("invalid capture accepted")
			}
		})
	}
}

func TestComputerCaptureEmptyInstanceAndDuplicateOwner(t *testing.T) {
	registry, target := &CaptureRuns{}, checkpointCaptureTarget(0)
	order := ""
	if err := registry.capture(t.Context(), target, func(context.Context) error { order += "capture/"; return nil }, func(context.Context) error { order += "exclude"; return nil }); err != nil {
		t.Fatal(err)
	}
	if order != "capture/exclude" {
		t.Fatal(order)
	}
	unexpected := func(context.Context) error { t.Fatal("duplicate owner touched source"); return nil }
	if err := registry.capture(t.Context(), target, unexpected, unexpected); err == nil {
		t.Fatal("duplicate capture accepted")
	}
}
