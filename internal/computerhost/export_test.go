package computerhost

// Fixture hooks for the Run-side integration tests in package
// computerhost_test.
//
// Those tests drive the Run side only through executor's exported operations
// and this package's exported surface. The few assertions that need the real
// physical owner on the other side of that surface (restore installation
// versus the runner's resume authority, mount-bound source release, and
// capture dispatch ordering) reach its private setup only through the hooks
// in this file, which exist only in test builds.

import (
	"context"
	"time"

	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

// MountComputer serves machine as the Computer Instance the physical Server
// mounts after materialization: a pending restore is installed and activated
// through the Server's own restore path, then the mount is registered for Runs
// with the assignment's channel credential.
func MountComputer(ctx context.Context, mounts *Mounts, restore ComputerRestoreControl, machine vm.Machine, mount workerapi.ComputerInstanceAssignment) (func(), error) {
	server := Server{RestoreControl: restore, Mounts: mounts}
	instance := newInstanceMount(machine)
	if mount.RestoreCheckpointID != "" {
		if err := server.activateRestore(ctx, instance, mount); err != nil {
			return nil, err
		}
	}
	return mounts.register(mount, instance, server.channelCredential(mount)), nil
}

// MountReleaseResult reads, without releasing, whether the mounted Instance
// computerInstanceID was released as a checkpoint source and with what result.
func MountReleaseResult(ctx context.Context, mounts *Mounts, computerInstanceID string) (bool, error) {
	mounts.mu.RLock()
	entry := mounts.mounts[computerInstanceID]
	mounts.mu.RUnlock()
	if entry.instance == nil {
		return false, ErrMountNotFound
	}
	return entry.instance.CheckpointReleaseResult(ctx)
}

// CaptureComputer runs the physical owner's capture protocol for target.
func CaptureComputer(ctx context.Context, captures *CaptureRuns, target workerapi.RuntimeReconcileTarget, capture, exclude func(context.Context) error) error {
	return captures.capture(ctx, target, capture, exclude)
}

// AwaitCaptureDispatch returns once a capture has dispatched its member pause
// to runID's registered wait. Dispatch is committed under the registry lock
// before the pause is delivered, so the wait's owner has not necessarily
// received it yet.
func AwaitCaptureDispatch(ctx context.Context, captures *CaptureRuns, runID string) error {
	for {
		captures.mu.Lock()
		wait := captures.waits[runID]
		dispatched := wait != nil && wait.pause != nil
		captures.mu.Unlock()
		if dispatched {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Millisecond):
		}
	}
}
