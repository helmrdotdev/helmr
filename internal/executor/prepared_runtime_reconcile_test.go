package executor

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/capacity"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/vm"
)

type typedRuntimeClient struct {
	targets      []api.WorkerRuntimeReconcileResponse
	closed       []api.WorkerRuntimeInstanceStateRequest
	failed       []api.WorkerRuntimeInstanceStateRequest
	failedErrors []error
}

type cleanupRuntimeConnector struct {
	cleaned []string
	err     error
}

type closeTrackingRuntimeSession struct {
	closed int
	err    error
}

type stuckPreparedRuntimeSession struct {
	waitStarted chan struct{}
	releaseWait chan struct{}
}

type unavailableRuntimeCAS struct{}

func (unavailableRuntimeCAS) Put(context.Context, string, io.Reader) (cas.Object, error) {
	return cas.Object{}, errors.New("not used")
}
func (unavailableRuntimeCAS) Stage(context.Context, string) (cas.Stage, error) {
	return nil, errors.New("not used")
}
func (unavailableRuntimeCAS) Stat(context.Context, string) (cas.Object, error) {
	return cas.Object{}, errors.New("not used")
}
func (unavailableRuntimeCAS) Get(context.Context, string) (io.ReadCloser, error) {
	return nil, errors.New("not used")
}
func (unavailableRuntimeCAS) Delete(context.Context, string) error { return errors.New("not used") }

func (c *cleanupRuntimeConnector) Connect(context.Context, vm.ConnectRequest) (vm.Session, error) {
	return nil, errors.New("not used")
}

func (c *cleanupRuntimeConnector) Cleanup(_ context.Context, owner vm.Owner) error {
	c.cleaned = append(c.cleaned, owner.ID)
	return c.err
}

func (*closeTrackingRuntimeSession) Stream() vm.Stream { return nil }
func (*closeTrackingRuntimeSession) OpenStream(context.Context) (vm.Stream, error) {
	return nil, nil
}
func (*closeTrackingRuntimeSession) Wait(context.Context) error { return nil }
func (s *closeTrackingRuntimeSession) Close(context.Context) error {
	s.closed++
	return s.err
}

func (*stuckPreparedRuntimeSession) Stream() vm.Stream { return nil }
func (*stuckPreparedRuntimeSession) OpenStream(context.Context) (vm.Stream, error) {
	return nil, nil
}
func (s *stuckPreparedRuntimeSession) Wait(context.Context) error {
	close(s.waitStarted)
	<-s.releaseWait
	return nil
}
func (*stuckPreparedRuntimeSession) Close(context.Context) error { return nil }

func TestPreparedRuntimePoolCloseHonorsDeadlineWhileMonitorIsStuck(t *testing.T) {
	session := &stuckPreparedRuntimeSession{waitStarted: make(chan struct{}), releaseWait: make(chan struct{})}
	pool := NewPreparedRuntimePool(nil, nil, 1, nil)
	pool.RuntimeInstances = &typedRuntimeClient{}
	entry := preparedRuntimeEntry{
		session: session, poolKey: "runtime-key", runtimeInstanceID: "runtime-1", runtimeEpoch: 7,
		target: api.WorkerRuntimeReconcileTarget{ID: "runtime-1", WorkerEpoch: 7, DesiredVersion: 1, ObservedVersion: 0},
		exit:   newPreparedRuntimeSignal(), ready: newPreparedRuntimeSignal(),
	}
	pool.mu.Lock()
	pool.entries[entry.poolKey] = []preparedRuntimeEntry{entry}
	pool.monitorReadyEntryLocked(entry.poolKey, entry)
	pool.mu.Unlock()
	<-session.waitStarted
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	started := time.Now()
	err := pool.Close(ctx)
	if err == nil || !strings.Contains(err.Error(), "background tasks") || time.Since(started) > time.Second {
		t.Fatalf("Close() error = %v, elapsed = %s", err, time.Since(started))
	}
	close(session.releaseWait)
	retryCtx, retryCancel := context.WithTimeout(context.Background(), time.Second)
	defer retryCancel()
	if err := pool.Close(retryCtx); err != nil {
		t.Fatalf("retry Close() = %v", err)
	}
}

func (c *typedRuntimeClient) NextRuntimeReconcileTarget(ctx context.Context) (api.WorkerRuntimeReconcileResponse, error) {
	if len(c.targets) == 0 {
		<-ctx.Done()
		return api.WorkerRuntimeReconcileResponse{}, ctx.Err()
	}
	target := c.targets[0]
	c.targets = c.targets[1:]
	return target, nil
}
func (c *typedRuntimeClient) MarkRuntimeInstanceReady(context.Context, api.WorkerRuntimeInstanceStateRequest) (api.WorkerRuntimeInstance, error) {
	return api.WorkerRuntimeInstance{}, nil
}
func (c *typedRuntimeClient) MarkRuntimeInstanceClosed(_ context.Context, request api.WorkerRuntimeInstanceStateRequest) (api.WorkerRuntimeInstance, error) {
	c.closed = append(c.closed, request)
	return api.WorkerRuntimeInstance{ID: request.ID}, nil
}
func (c *typedRuntimeClient) MarkRuntimeInstanceFailed(_ context.Context, request api.WorkerRuntimeInstanceStateRequest) (api.WorkerRuntimeInstance, error) {
	c.failed = append(c.failed, request)
	if len(c.failedErrors) > 0 {
		err := c.failedErrors[0]
		c.failedErrors = c.failedErrors[1:]
		return api.WorkerRuntimeInstance{}, err
	}
	return api.WorkerRuntimeInstance{ID: request.ID}, nil
}

func TestStopRuntimeTargetRequiresExclusiveMatchingLocalEpoch(t *testing.T) {
	session := &closeTrackingRuntimeSession{}
	pool := NewPreparedRuntimePool(nil, nil, 1, nil)
	pool.entries["runtime-key"] = []preparedRuntimeEntry{{session: session, runtimeInstanceID: "runtime-1", runtimeEpoch: 7}}
	client := &typedRuntimeClient{}
	target := api.WorkerRuntimeReconcileTarget{ID: "runtime-1", WorkerEpoch: 7, DesiredVersion: 2, ObservedVersion: 1}
	if err := pool.StopRuntimeTarget(context.Background(), client, target); err != nil {
		t.Fatal(err)
	}
	if len(client.closed) != 1 || client.closed[0].ID != "runtime-1" || client.closed[0].WorkerEpoch != 7 {
		t.Fatalf("closed = %+v", client.closed)
	}
	if proof := client.closed[0].CleanupProof; proof == nil || proof.Method != api.WorkerRuntimeCleanupSessionClosed || proof.CompletedAt.IsZero() {
		t.Fatalf("cleanup proof = %+v, want closed session", proof)
	}
	if session.closed != 1 {
		t.Fatalf("session close count = %d, want 1", session.closed)
	}
	if err := pool.StopRuntimeTarget(context.Background(), client, target); err == nil {
		t.Fatal("second controller teardown unexpectedly acquired the same runtime")
	}
}

func TestStopRuntimeTargetDefersToCheckedOutWorkspaceRuntime(t *testing.T) {
	pool := NewPreparedRuntimePool(nil, nil, 1, nil)
	pool.mu.Lock()
	pool.markRuntimeCheckedOutLocked("runtime-1", 7)
	pool.mu.Unlock()
	client := &typedRuntimeClient{}
	target := api.WorkerRuntimeReconcileTarget{ID: "runtime-1", WorkerEpoch: 7, DesiredVersion: 2, ObservedVersion: 1}
	if err := pool.StopRuntimeTarget(context.Background(), client, target); err != nil {
		t.Fatal(err)
	}
	if len(client.closed) != 0 {
		t.Fatalf("checked-out runtime was closed by the pool reconciler: %+v", client.closed)
	}
	if err := pool.ReleaseCheckout(target.ID, target.WorkerEpoch); err != nil {
		t.Fatal(err)
	}
	if err := pool.StopRuntimeTarget(context.Background(), client, target); err == nil {
		t.Fatal("untracked runtime teardown unexpectedly succeeded")
	}
}

func TestStopRuntimeTargetReconcilesMissingLocalRuntimeExactly(t *testing.T) {
	cleaner := &cleanupRuntimeConnector{}
	pool := NewPreparedRuntimePool(cleaner, nil, 1, nil)
	client := &typedRuntimeClient{}
	target := api.WorkerRuntimeReconcileTarget{ID: "runtime-1", WorkerEpoch: 7, DesiredVersion: 2, ObservedVersion: 1}

	if err := pool.StopRuntimeTarget(context.Background(), client, target); err != nil {
		t.Fatal(err)
	}
	if len(cleaner.cleaned) != 1 || cleaner.cleaned[0] != target.ID {
		t.Fatalf("cleaned = %v, want [%s]", cleaner.cleaned, target.ID)
	}
	if len(client.closed) != 1 {
		t.Fatalf("closed = %+v, want one transition", client.closed)
	}
	proof := client.closed[0].CleanupProof
	if proof == nil || proof.Method != api.WorkerRuntimeCleanupHostReconciled || proof.CompletedAt.IsZero() {
		t.Fatalf("cleanup proof = %+v, want host reconciliation", proof)
	}
}

func TestStopRuntimeTargetDoesNotCloseWhenExactCleanupFails(t *testing.T) {
	cleaner := &cleanupRuntimeConnector{err: errors.New("cleanup failed")}
	pool := NewPreparedRuntimePool(cleaner, nil, 1, nil)
	client := &typedRuntimeClient{}
	target := api.WorkerRuntimeReconcileTarget{ID: "runtime-1", WorkerEpoch: 7, DesiredVersion: 2, ObservedVersion: 1}

	if err := pool.StopRuntimeTarget(context.Background(), client, target); err == nil {
		t.Fatal("cleanup failure unexpectedly closed runtime")
	}
	if len(client.closed) != 0 {
		t.Fatalf("closed = %+v, want no transition", client.closed)
	}
}

func TestReconcileDesiredRuntimesStopsCleanly(t *testing.T) {
	pool := NewPreparedRuntimePool(nil, nil, 1, nil)
	client := &typedRuntimeClient{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- pool.ReconcileDesiredRuntimes(ctx, client) }()
	cancel()
	select {
	case err := <-done:
		if err != context.Canceled {
			t.Fatalf("error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("typed runtime reconciler did not stop")
	}
}

func TestWarmRuntimeTargetHonorsHardAdmissionBeforeMaterialization(t *testing.T) {
	pool := NewPreparedRuntimePool(nil, nil, 1, nil)
	admissionErr := errors.New("disk_floor")
	pool.AdmitRuntimeStart = func(context.Context) error { return admissionErr }
	err := pool.WarmRuntimeTarget(context.Background(), &typedRuntimeClient{}, api.WorkerRuntimeReconcileTarget{})
	if !errors.Is(err, admissionErr) {
		t.Fatalf("error = %v, want hard admission error", err)
	}
}

func TestWarmRuntimeTargetRetriesForegroundBackpressureWithoutDurableFailure(t *testing.T) {
	gate := NewBackgroundWorkGate()
	endForeground := gate.BeginForeground()
	pool := NewPreparedRuntimePool(&cleanupRuntimeConnector{}, unavailableRuntimeCAS{}, 1, nil)
	pool.BackgroundGate = gate
	client := &typedRuntimeClient{}
	target := retryableWarmTarget()

	err := pool.WarmRuntimeTarget(context.Background(), client, target)
	assertRuntimeBackpressure(t, err, PreparedRuntimeBackpressureForeground)
	if len(client.failed) != 0 {
		t.Fatalf("foreground backpressure mutated durable runtime: %+v", client.failed)
	}

	endForeground()
	backgroundCtx, finish, ok := pool.beginBackground(context.Background())
	if !ok || backgroundCtx == nil {
		t.Fatal("foreground release did not make retry eligible")
	}
	finish()
}

func TestWarmRuntimeTargetRetriesCapacityBackpressureWithoutDurableFailure(t *testing.T) {
	pool := NewPreparedRuntimePool(&cleanupRuntimeConnector{}, unavailableRuntimeCAS{}, 1, nil)
	pool.entries["occupied"] = []preparedRuntimeEntry{{runtimeInstanceID: "occupied", runtimeEpoch: 7}}
	client := &typedRuntimeClient{}

	err := pool.WarmRuntimeTarget(context.Background(), client, retryableWarmTarget())
	assertRuntimeBackpressure(t, err, PreparedRuntimeBackpressureCapacity)
	if len(client.failed) != 0 {
		t.Fatalf("capacity backpressure mutated durable runtime: %+v", client.failed)
	}

	pool.mu.Lock()
	delete(pool.entries, "occupied")
	retryEligible := pool.reservedCountLocked() < pool.Size
	pool.mu.Unlock()
	if !retryEligible {
		t.Fatal("released local capacity did not make retry eligible")
	}
}

func TestPreparedRuntimeCapacityReservationLivesThroughCheckout(t *testing.T) {
	target := runtimeCapacityTarget("00000000-0000-0000-0000-000000000510", 7)
	pool := NewPreparedRuntimePool(nil, nil, 1, nil)
	pool.Capacity = newPreparedRuntimeCapacity(t, 1)
	pool.RuntimeScratchBytes = 256 << 20
	if err := pool.reserveRuntimeCapacity(target); err != nil {
		t.Fatal(err)
	}
	wantKey := runtimeCapacityKey(target.ID, target.WorkerEpoch)
	wantVector := capacity.Vector{
		CPUMillis: 1000, MemoryBytes: 512 << 20, WorkloadDiskBytes: 1024 << 20,
		ScratchBytes: 256 << 20, VMSlots: 1,
	}
	if got := pool.Capacity.Snapshot().Reservations[wantKey]; got != wantVector {
		t.Fatalf("reservation = %+v, want %+v", got, wantVector)
	}

	mount := preparedRuntimeWorkspaceMountFromSource(target.Source)
	mount.RuntimeInstanceID = target.ID
	mount.RuntimeEpoch = target.WorkerEpoch
	key := runtimeInstanceIDFromWorkspaceMount(mount)
	ready := newPreparedRuntimeSignal()
	ready.finish(nil)
	pool.entries[key] = []preparedRuntimeEntry{{
		session: &closeTrackingRuntimeSession{}, poolKey: key,
		runtimeInstanceID: target.ID, runtimeEpoch: target.WorkerEpoch,
		target: target, exit: newPreparedRuntimeSignal(), ready: ready,
	}}

	if _, _, ok := pool.Checkout(context.Background(), mount); !ok {
		t.Fatal("reserved runtime was not checked out")
	}
	if got := len(pool.Capacity.Snapshot().Reservations); got != 1 {
		t.Fatalf("reservations after checkout = %d, want 1", got)
	}
	if err := pool.ReleaseCheckout(target.ID, target.WorkerEpoch); err != nil {
		t.Fatal(err)
	}
	if got := len(pool.Capacity.Snapshot().Reservations); got != 0 {
		t.Fatalf("reservations after successful checkout cleanup = %d, want 0", got)
	}
}

func TestPreparedRuntimeCapacityExhaustionIsRetryableBackpressure(t *testing.T) {
	pool := NewPreparedRuntimePool(nil, nil, 2, nil)
	pool.Capacity = newPreparedRuntimeCapacity(t, 1)
	pool.RuntimeScratchBytes = 256 << 20
	first := runtimeCapacityTarget("00000000-0000-0000-0000-000000000511", 7)
	if err := pool.reserveRuntimeCapacity(first); err != nil {
		t.Fatal(err)
	}
	assertRuntimeBackpressure(t, pool.reserveRuntimeCapacity(first), PreparedRuntimeBackpressureCapacity)

	err := pool.reserveRuntimeCapacity(runtimeCapacityTarget("00000000-0000-0000-0000-000000000512", 7))
	assertRuntimeBackpressure(t, err, PreparedRuntimeBackpressureCapacity)
	if got := len(pool.Capacity.Snapshot().Reservations); got != 1 {
		t.Fatalf("reservations = %d, want 1", got)
	}
}

func TestPreparedRuntimeCapacityRejectsNegativeScratch(t *testing.T) {
	pool := NewPreparedRuntimePool(nil, nil, 1, nil)
	pool.Capacity = newPreparedRuntimeCapacity(t, 1)
	pool.RuntimeScratchBytes = -1

	if err := pool.reserveRuntimeCapacity(runtimeCapacityTarget("00000000-0000-0000-0000-000000000514", 7)); err == nil {
		t.Fatal("negative runtime scratch capacity unexpectedly reserved")
	}
	if got := len(pool.Capacity.Snapshot().Reservations); got != 0 {
		t.Fatalf("reservations = %d, want 0", got)
	}
}

func TestPreparedRuntimeCloseFailureRetainsCapacityUntilReclaim(t *testing.T) {
	target := runtimeCapacityTarget("00000000-0000-0000-0000-000000000513", 7)
	connector := &cleanupRuntimeConnector{}
	pool := NewPreparedRuntimePool(connector, nil, 1, nil)
	pool.Capacity = newPreparedRuntimeCapacity(t, 1)
	pool.RuntimeScratchBytes = 256 << 20
	if err := pool.reserveRuntimeCapacity(target); err != nil {
		t.Fatal(err)
	}
	session := &closeTrackingRuntimeSession{err: errors.New("close failed")}
	pool.entries["runtime-key"] = []preparedRuntimeEntry{{
		session: session, poolKey: "runtime-key",
		runtimeInstanceID: target.ID, runtimeEpoch: target.WorkerEpoch, target: target,
	}}
	client := &typedRuntimeClient{}

	if err := pool.StopRuntimeTarget(context.Background(), client, target); err == nil {
		t.Fatal("close failure unexpectedly succeeded")
	}
	if got := len(pool.Capacity.Snapshot().Reservations); got != 1 {
		t.Fatalf("reservations after close failure = %d, want 1", got)
	}
	if err := pool.ReclaimFailedRuntimeTarget(context.Background(), client, target); err != nil {
		t.Fatal(err)
	}
	if got := len(pool.Capacity.Snapshot().Reservations); got != 0 {
		t.Fatalf("reservations after exact reclaim = %d, want 0", got)
	}
}

func newPreparedRuntimeCapacity(t *testing.T, vmSlots int64) *capacity.Ledger {
	t.Helper()
	ledger, err := capacity.New(capacity.Vector{
		CPUMillis: 1000, MemoryBytes: 512 << 20, WorkloadDiskBytes: 1024 << 20,
		ScratchBytes: 256 << 20, VMSlots: vmSlots,
	})
	if err != nil {
		t.Fatal(err)
	}
	return ledger
}

func runtimeCapacityTarget(id string, epoch int64) api.WorkerRuntimeReconcileTarget {
	return api.WorkerRuntimeReconcileTarget{
		ID: id, WorkerEpoch: epoch,
		Source: api.WorkerRuntimeSource{
			DeploymentDefinitionID: "00000000-0000-0000-0000-000000000703",
			ReservedCpuMillis:      1000, ReservedMemoryMiB: 512, ReservedDiskMiB: 1024,
			ReservedExecutionSlots: 5,
		},
	}
}

func retryableWarmTarget() api.WorkerRuntimeReconcileTarget {
	return api.WorkerRuntimeReconcileTarget{
		ID: "00000000-0000-0000-0000-000000000503", WorkerEpoch: 7,
		Source: api.WorkerRuntimeSource{DeploymentDefinitionID: "00000000-0000-0000-0000-000000000703"},
	}
}

func assertRuntimeBackpressure(t *testing.T, err error, want PreparedRuntimeBackpressureKind) {
	t.Helper()
	var backpressure *PreparedRuntimeBackpressureError
	if !errors.As(err, &backpressure) || backpressure.Kind != want || !backpressure.Retryable() {
		t.Fatalf("error = %v, want retryable %s backpressure", err, want)
	}
}

func TestReclaimFailedRuntimeTargetPersistsProofOnlyAfterExactHostCleanup(t *testing.T) {
	connector := &cleanupRuntimeConnector{}
	pool := NewPreparedRuntimePool(connector, nil, 1, nil)
	client := &typedRuntimeClient{}
	target := api.WorkerRuntimeReconcileTarget{
		ID: "00000000-0000-0000-0000-000000000501", WorkerEpoch: 7,
		NetworkSlotID: "00000000-0000-0000-0000-000000000601", NetworkSlotGeneration: 3,
		DesiredVersion: 2, ObservedVersion: 4, Action: api.WorkerRuntimeReconcileReclaim,
	}
	if err := pool.ReclaimFailedRuntimeTarget(context.Background(), client, target); err != nil {
		t.Fatal(err)
	}
	if len(connector.cleaned) != 1 || connector.cleaned[0] != target.ID {
		t.Fatalf("cleaned = %v", connector.cleaned)
	}
	if len(client.failed) != 1 || client.failed[0].CleanupProof == nil || client.failed[0].CleanupProof.Method != api.WorkerRuntimeCleanupHostReconciled {
		t.Fatalf("failed transition = %+v", client.failed)
	}
}

func TestReclaimFailedRuntimeTargetKeepsQuarantineWhenCleanupIsAmbiguous(t *testing.T) {
	connector := &cleanupRuntimeConnector{err: errors.New("process still alive")}
	pool := NewPreparedRuntimePool(connector, nil, 1, nil)
	client := &typedRuntimeClient{}
	target := api.WorkerRuntimeReconcileTarget{ID: "00000000-0000-0000-0000-000000000502", WorkerEpoch: 7}
	if err := pool.ReclaimFailedRuntimeTarget(context.Background(), client, target); err == nil {
		t.Fatal("ambiguous cleanup unexpectedly succeeded")
	}
	if len(client.failed) != 0 {
		t.Fatalf("cleanup proof persisted after failure: %+v", client.failed)
	}
}

func TestReclaimFailedCheckedOutRuntimeClearsExactCheckoutAfterPhysicalCleanup(t *testing.T) {
	target := runtimeCapacityTarget("00000000-0000-0000-0000-000000000515", 7)
	target.NetworkSlotID = "00000000-0000-0000-0000-000000000615"
	target.NetworkSlotGeneration = 3
	target.DesiredVersion = 2
	target.ObservedVersion = 4
	connector := &cleanupRuntimeConnector{}
	pool := NewPreparedRuntimePool(connector, nil, 1, nil)
	pool.Capacity = newPreparedRuntimeCapacity(t, 1)
	pool.RuntimeScratchBytes = 256 << 20
	if err := pool.reserveRuntimeCapacity(target); err != nil {
		t.Fatal(err)
	}
	pool.mu.Lock()
	pool.markRuntimeCheckedOutLocked(target.ID, target.WorkerEpoch)
	pool.markRuntimeCheckedOutLocked("00000000-0000-0000-0000-000000000516", target.WorkerEpoch)
	pool.mu.Unlock()
	client := &typedRuntimeClient{}

	if err := pool.ReclaimFailedRuntimeTarget(context.Background(), client, target); err != nil {
		t.Fatal(err)
	}
	if pool.runtimeCheckedOut(target.ID, target.WorkerEpoch) {
		t.Fatal("reclaimed runtime remains checked out")
	}
	if !pool.runtimeCheckedOut("00000000-0000-0000-0000-000000000516", target.WorkerEpoch) {
		t.Fatal("reclaim cleared a different checkout")
	}
	if got := len(pool.Capacity.Snapshot().Reservations); got != 0 {
		t.Fatalf("reservations after reclaim = %d, want 0", got)
	}
	if len(connector.cleaned) != 1 || connector.cleaned[0] != target.ID {
		t.Fatalf("cleaned = %v, want exact runtime", connector.cleaned)
	}
	if len(client.failed) != 1 || client.failed[0].CleanupProof == nil ||
		client.failed[0].CleanupProof.Method != api.WorkerRuntimeCleanupHostReconciled {
		t.Fatalf("cleanup proof = %+v, want host reconciliation", client.failed)
	}
}

func TestReclaimFailedCheckedOutRuntimeRetainsCheckoutWhenPhysicalCleanupFails(t *testing.T) {
	target := runtimeCapacityTarget("00000000-0000-0000-0000-000000000517", 7)
	connector := &cleanupRuntimeConnector{err: errors.New("runtime still exists")}
	pool := NewPreparedRuntimePool(connector, nil, 1, nil)
	pool.Capacity = newPreparedRuntimeCapacity(t, 1)
	pool.RuntimeScratchBytes = 256 << 20
	if err := pool.reserveRuntimeCapacity(target); err != nil {
		t.Fatal(err)
	}
	pool.mu.Lock()
	pool.markRuntimeCheckedOutLocked(target.ID, target.WorkerEpoch)
	pool.mu.Unlock()
	client := &typedRuntimeClient{}

	if err := pool.ReclaimFailedRuntimeTarget(context.Background(), client, target); err == nil {
		t.Fatal("cleanup failure unexpectedly reclaimed runtime")
	}
	if !pool.runtimeCheckedOut(target.ID, target.WorkerEpoch) {
		t.Fatal("cleanup failure cleared checkout")
	}
	if got := len(pool.Capacity.Snapshot().Reservations); got != 1 {
		t.Fatalf("reservations after cleanup failure = %d, want 1", got)
	}
	if len(client.failed) != 0 {
		t.Fatalf("cleanup failure persisted proof: %+v", client.failed)
	}
}

func TestReclaimFailedCheckedOutRuntimeRetriesProofAfterLocalRelease(t *testing.T) {
	target := runtimeCapacityTarget("00000000-0000-0000-0000-000000000518", 7)
	target.NetworkSlotID = "00000000-0000-0000-0000-000000000618"
	target.NetworkSlotGeneration = 3
	target.DesiredVersion = 2
	target.ObservedVersion = 4
	connector := &cleanupRuntimeConnector{}
	pool := NewPreparedRuntimePool(connector, nil, 1, nil)
	pool.Capacity = newPreparedRuntimeCapacity(t, 1)
	pool.RuntimeScratchBytes = 256 << 20
	if err := pool.reserveRuntimeCapacity(target); err != nil {
		t.Fatal(err)
	}
	pool.mu.Lock()
	pool.markRuntimeCheckedOutLocked(target.ID, target.WorkerEpoch)
	pool.mu.Unlock()
	client := &typedRuntimeClient{failedErrors: []error{errors.New("proof response lost")}}

	if err := pool.ReclaimFailedRuntimeTarget(context.Background(), client, target); err == nil {
		t.Fatal("proof persistence failure unexpectedly succeeded")
	}
	if pool.runtimeCheckedOut(target.ID, target.WorkerEpoch) {
		t.Fatal("physical cleanup success retained checkout after proof failure")
	}
	if got := len(pool.Capacity.Snapshot().Reservations); got != 0 {
		t.Fatalf("reservations after physical cleanup = %d, want 0", got)
	}
	if err := pool.ReclaimFailedRuntimeTarget(context.Background(), client, target); err != nil {
		t.Fatal(err)
	}
	if len(connector.cleaned) != 2 {
		t.Fatalf("physical cleanup attempts = %d, want 2 idempotent attempts", len(connector.cleaned))
	}
	if len(client.failed) != 2 {
		t.Fatalf("proof attempts = %d, want 2", len(client.failed))
	}
	for _, request := range client.failed {
		if request.CleanupProof == nil || request.CleanupProof.Method != api.WorkerRuntimeCleanupHostReconciled {
			t.Fatalf("proof attempt = %+v, want host reconciliation", request)
		}
	}
}
