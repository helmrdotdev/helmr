package executor

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

// barrierCloseMachine blocks every Close until the test releases it and
// returns the error queued for that attempt.
type barrierCloseMachine struct {
	entered chan struct{}
	release chan error
	mu      sync.Mutex
	closes  int
}

func newBarrierCloseMachine() *barrierCloseMachine {
	return &barrierCloseMachine{entered: make(chan struct{}, 1), release: make(chan error)}
}

func (*barrierCloseMachine) Stream() vm.Stream { return nil }
func (*barrierCloseMachine) OpenStream(context.Context) (vm.Stream, error) {
	return nil, errors.New("barrier machine has no streams")
}
func (*barrierCloseMachine) Wait(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}

func (m *barrierCloseMachine) Close(ctx context.Context) error {
	m.mu.Lock()
	m.closes++
	m.mu.Unlock()
	m.entered <- struct{}{}
	select {
	case err := <-m.release:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *barrierCloseMachine) closeCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.closes
}

type countingCloseComputerDevice struct {
	vm.ComputerDevice
	mu     sync.Mutex
	err    error
	closes int
}

func (d *countingCloseComputerDevice) Close(context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.closes++
	return d.err
}

func (d *countingCloseComputerDevice) closeCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.closes
}

// reportOrderRuntimeClient runs beforeReport ahead of recording each runtime
// state report, so a test can observe local cleanup at report time.
type reportOrderRuntimeClient struct {
	typedRuntimeClient
	beforeReport func()
}

func (c *reportOrderRuntimeClient) MarkComputerInstanceClosed(ctx context.Context, request workerapi.ComputerInstanceStateRequest) (workerapi.ComputerInstance, error) {
	c.beforeReport()
	return c.typedRuntimeClient.MarkComputerInstanceClosed(ctx, request)
}

func (c *reportOrderRuntimeClient) MarkComputerInstanceFailed(ctx context.Context, request workerapi.ComputerInstanceStateRequest) (workerapi.ComputerInstance, error) {
	c.beforeReport()
	return c.typedRuntimeClient.MarkComputerInstanceFailed(ctx, request)
}

func computerDeviceTracked(pool *PreparedRuntimePool, ref preparedRuntimeRef) bool {
	pool.mu.Lock()
	defer pool.mu.Unlock()
	return pool.computerDevices[ref] != nil
}

func TestFailedCheckoutCloseDefersToOwnerThenReconcileCleansUpOnce(t *testing.T) {
	for _, retry := range []string{"stop", "reclaim"} {
		t.Run(retry, func(t *testing.T) {
			_, mount := testComputerMountArtifacts(t)
			mount.ComputerID = "computer"
			mount.DesiredVersion = 2
			mount.ObservedVersion = 1
			// The prepared runtime has no restore provenance, so the mount is
			// rejected after checkout and the materializer must close it.
			mount.RestoreCheckpointID = "checkpoint-not-prepared"
			machine := newBarrierCloseMachine()
			pool := computerPreparedRuntimePool(t, mount, machine)
			ref := preparedRuntimeRef{id: mount.ComputerInstanceID, epoch: mount.RuntimeEpoch}
			device := &countingCloseComputerDevice{}
			pool.computerDevices = map[preparedRuntimeRef]vm.ComputerDevice{ref: device}
			backend := &cleanupRuntimeBackend{}
			pool.Backend = backend
			target := runtimeReservationTarget(ref.id, ref.epoch)
			target.DesiredVersion = 2
			target.ObservedVersion = 1

			materializer := ComputerMaterializer{RuntimePool: pool, FailureTimeout: 5 * time.Second}
			result := make(chan error, 1)
			go func() {
				_, _, err := materializer.materializeSession(t.Context(), &mount)
				result <- err
			}()
			select {
			case <-machine.entered:
			case <-time.After(5 * time.Second):
				t.Fatal("materializer did not close the rejected checkout")
			}

			// While the materializer still owns the checkout, reconciliation defers.
			early := &typedRuntimeClient{}
			if err := pool.StopRuntimeTarget(t.Context(), early, target); err != nil {
				t.Fatal(err)
			}
			if len(backend.cleaned) != 0 || len(early.closed) != 0 || len(early.failed) != 0 || device.closeCount() != 0 {
				t.Fatalf("reconcile acted on a held checkout: cleaned=%v closed=%d failed=%d device=%d",
					backend.cleaned, len(early.closed), len(early.failed), device.closeCount())
			}
			if !pool.runtimeCheckedOut(ref.id, ref.epoch) {
				t.Fatal("checkout released while its owner is closing it")
			}

			closeErr := errors.New("physical exclusion not established")
			machine.release <- closeErr
			var err error
			select {
			case err = <-result:
			case <-time.After(5 * time.Second):
				t.Fatal("materializer did not finish")
			}
			if !errors.Is(err, closeErr) || !strings.Contains(err.Error(), "close prepared computer runtime") {
				t.Fatalf("materialize error = %v, want close failure", err)
			}
			if pool.runtimeCheckedOut(ref.id, ref.epoch) {
				t.Fatal("failed close must hand the checkout to reconciliation")
			}
			if got := len(pool.Reservations.Snapshot().Reservations); got != 1 {
				t.Fatalf("reservations after failed close = %d, want 1", got)
			}
			if !computerDeviceTracked(pool, ref) || device.closeCount() != 0 {
				t.Fatal("failed close must retain the Computer device")
			}

			reports := 0
			control := &reportOrderRuntimeClient{beforeReport: func() {
				reports++
				if got := len(pool.Reservations.Snapshot().Reservations); got != 0 || computerDeviceTracked(pool, ref) || device.closeCount() != 1 {
					t.Errorf("report before local release: reservations=%d device tracked=%t device closes=%d",
						got, computerDeviceTracked(pool, ref), device.closeCount())
				}
			}}
			switch retry {
			case "stop":
				if err := pool.StopRuntimeTarget(t.Context(), control, target); err != nil {
					t.Fatal(err)
				}
				if len(control.closed) != 1 || control.closed[0].CleanupProof == nil ||
					control.closed[0].CleanupProof.Method != workerapi.RuntimeCleanupHostReconciled {
					t.Fatalf("closed = %+v, want host reconciled proof", control.closed)
				}
			case "reclaim":
				if err := pool.ReclaimFailedRuntimeTarget(t.Context(), control, target); err != nil {
					t.Fatal(err)
				}
				if len(control.failed) != 1 || control.failed[0].CleanupProof == nil ||
					control.failed[0].CleanupProof.Method != workerapi.RuntimeCleanupHostReconciled {
					t.Fatalf("failed = %+v, want host reconciled proof", control.failed)
				}
			}
			if reports != 1 {
				t.Fatalf("reports = %d, want 1", reports)
			}
			if len(backend.cleaned) != 1 || backend.cleaned[0] != ref.id {
				t.Fatalf("backend cleanups = %v, want exactly one", backend.cleaned)
			}
			if device.closeCount() != 1 || machine.closeCount() != 1 {
				t.Fatalf("device closes = %d, machine closes = %d, want 1 each", device.closeCount(), machine.closeCount())
			}
			if got := len(pool.Reservations.Snapshot().Reservations); got != 0 {
				t.Fatalf("reservations after reconcile = %d, want 0", got)
			}
			if computerDeviceTracked(pool, ref) {
				t.Fatal("reconcile did not release the Computer device")
			}
		})
	}
}

// The injected failure is the Computer device cleanup that Release performs
// before returning reservations; the checkout must still be handed back.
func TestReleaseCheckoutRelinquishesWhenDeviceCleanupFails(t *testing.T) {
	_, mount := testComputerMountArtifacts(t)
	mount.ComputerID = "computer"
	pool := computerPreparedRuntimePool(t, mount, &closeTrackingRuntimeSession{})
	checkout, _, ok := pool.Checkout(t.Context(), mount)
	if !ok {
		t.Fatal("checkout failed")
	}
	ref := preparedRuntimeRef{id: mount.ComputerInstanceID, epoch: mount.RuntimeEpoch}
	device := &countingCloseComputerDevice{err: errors.New("device cleanup failed")}
	pool.computerDevices = map[preparedRuntimeRef]vm.ComputerDevice{ref: device}

	if err := checkout.Release(); !errors.Is(err, device.err) {
		t.Fatalf("release error = %v, want device failure", err)
	}
	if pool.runtimeCheckedOut(ref.id, ref.epoch) {
		t.Fatal("release must relinquish the checkout even when capacity release fails")
	}
	if got := len(pool.Reservations.Snapshot().Reservations); got != 1 || !computerDeviceTracked(pool, ref) {
		t.Fatalf("reservations = %d, device retained = %t; want leftovers retained for reconcile", got, computerDeviceTracked(pool, ref))
	}
	if err := checkout.Release(); err != nil {
		t.Fatalf("repeated release = %v, want nil", err)
	}
	if device.closeCount() != 1 {
		t.Fatalf("device closes after repeated release = %d, want 1", device.closeCount())
	}

	device.mu.Lock()
	device.err = nil
	device.mu.Unlock()
	backend := &cleanupRuntimeBackend{}
	pool.Backend = backend
	control := &reportOrderRuntimeClient{beforeReport: func() {
		if got := len(pool.Reservations.Snapshot().Reservations); got != 0 || computerDeviceTracked(pool, ref) || device.closeCount() != 2 {
			t.Errorf("report before local release: reservations=%d device tracked=%t device closes=%d",
				got, computerDeviceTracked(pool, ref), device.closeCount())
		}
	}}
	if err := pool.StopRuntimeTarget(t.Context(), control, runtimeReservationTarget(ref.id, ref.epoch)); err != nil {
		t.Fatal(err)
	}
	if len(backend.cleaned) != 1 || len(control.closed) != 1 || device.closeCount() != 2 {
		t.Fatalf("cleaned=%v closed=%d device closes=%d", backend.cleaned, len(control.closed), device.closeCount())
	}
	if got := len(pool.Reservations.Snapshot().Reservations); got != 0 {
		t.Fatalf("reservations after reconcile = %d, want 0", got)
	}
}
