package executor

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/vm"
)

type typedRuntimeClient struct {
	targets []api.WorkerRuntimeReconcileResponse
	closed  []api.WorkerRuntimeInstanceStateRequest
	failed  []api.WorkerRuntimeInstanceStateRequest
}

type cleanupRuntimeConnector struct {
	cleaned []string
	err     error
}

type closeTrackingRuntimeSession struct {
	closed int
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

func (c *cleanupRuntimeConnector) CleanupRuntime(_ context.Context, id string) error {
	c.cleaned = append(c.cleaned, id)
	return c.err
}

func (*closeTrackingRuntimeSession) Stream() io.ReadWriteCloser { return nil }
func (*closeTrackingRuntimeSession) OpenStream(context.Context) (io.ReadWriteCloser, error) {
	return nil, nil
}
func (*closeTrackingRuntimeSession) Wait(context.Context) error { return nil }
func (s *closeTrackingRuntimeSession) Close(context.Context) error {
	s.closed++
	return nil
}

func (*stuckPreparedRuntimeSession) Stream() io.ReadWriteCloser { return nil }
func (*stuckPreparedRuntimeSession) OpenStream(context.Context) (io.ReadWriteCloser, error) {
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
	pool.ReleaseCheckout(target.ID, target.WorkerEpoch)
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

func retryableWarmTarget() api.WorkerRuntimeReconcileTarget {
	return api.WorkerRuntimeReconcileTarget{
		ID: "00000000-0000-0000-0000-000000000503", WorkerEpoch: 7,
		Source: api.WorkerPreparedRuntimeSource{DeploymentSandboxID: "00000000-0000-0000-0000-000000000703"},
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
