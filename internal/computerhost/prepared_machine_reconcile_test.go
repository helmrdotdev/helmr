package computerhost

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/artifact"
	"github.com/helmrdotdev/helmr/internal/disk"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/reservation"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

type typedRuntimeClient struct {
	targets      []workerapi.RuntimeReconcileResponse
	closed       []workerapi.ComputerInstanceStateRequest
	failed       []workerapi.ComputerInstanceStateRequest
	failedErrors []error
}

type batchRuntimeClient struct {
	response workerapi.RuntimeReconcileResponse
	polled   chan struct{}
	calls    atomic.Int32
	repeat   bool
}

type blockingMaterializingBackend struct {
	unsupportedMachineStarts
	started  chan string
	canceled chan string
	failID   string
}

type blockingCloseRuntimeSession struct {
	unusedCheckpoint
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

type countingRuntimeBackend struct {
	unsupportedMachineStarts
	calls atomic.Int32
}

type cleanupRuntimeBackend struct {
	unsupportedMachineStarts
	cleaned []string
	err     error
}

type closeTrackingRuntimeSession struct {
	unusedCheckpoint
	closed int
	err    error
}

type stuckPreparedMachineSession struct {
	unusedCheckpoint
	waitStarted chan struct{}
	releaseWait chan struct{}
}

type unavailableRuntimeCAS struct{}

type fatalRuntimeInfrastructureError struct{ secret string }

func (err fatalRuntimeInfrastructureError) Error() string { return err.secret }
func (fatalRuntimeInfrastructureError) FatalWorker() bool { return true }

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

func TestRuntimeTargetFailureScrubsFatalWorkerDiagnostic(t *testing.T) {
	const sentinel = "signed-url-secret-sentinel"
	request := runtimeTargetStatusRequest(
		workerapi.RuntimeReconcileTarget{ID: "runtime", WorkerEpoch: 1, DesiredVersion: 2},
		fatalRuntimeInfrastructureError{secret: sentinel},
	)
	if request.ReasonCode != workerapi.RuntimeFailureWorkerInvalid ||
		strings.Contains(string(request.Error), sentinel) ||
		!strings.Contains(string(request.Error), "worker runtime infrastructure failed") {
		t.Fatalf("fatal runtime request = reason:%q error:%s", request.ReasonCode, request.Error)
	}
}

func TestFatalRuntimeFailureWaitsForControlAcknowledgement(t *testing.T) {
	client := &typedRuntimeClient{failedErrors: []error{errors.New("control unavailable")}}
	machines := &PreparedMachines{}
	target := workerapi.RuntimeReconcileTarget{ID: "runtime", WorkerEpoch: 1, DesiredVersion: 2}
	err := machines.reportRuntimeTargetFailedWithProof(
		t.Context(), client, target,
		fatalRuntimeInfrastructureError{secret: "local-secret-sentinel"},
		workerapi.RuntimeCleanupNotMaterialized,
	)
	if err == nil || !strings.Contains(err.Error(), "control unavailable") ||
		strings.Contains(err.Error(), "local-secret-sentinel") {
		t.Fatalf("failure-report error = %v", err)
	}
	var fatal interface{ FatalWorker() bool }
	if errors.As(err, &fatal) && fatal.FatalWorker() {
		t.Fatal("unacknowledged failure report terminated the Worker epoch")
	}
	if err := machines.reportRuntimeTargetFailedWithProof(
		t.Context(), client, target,
		fatalRuntimeInfrastructureError{secret: "local-secret-sentinel"},
		workerapi.RuntimeCleanupNotMaterialized,
	); err != nil {
		t.Fatalf("acknowledged failure report = %v", err)
	}
}

func (c *cleanupRuntimeBackend) Cleanup(_ context.Context, owner vm.Owner) error {
	c.cleaned = append(c.cleaned, owner.ID)
	return c.err
}

func (*closeTrackingRuntimeSession) Stream() vm.Stream { return nil }
func (*closeTrackingRuntimeSession) OpenStream(context.Context) (vm.Stream, error) {
	return nil, nil
}
func (*closeTrackingRuntimeSession) Wait(context.Context) error { return nil }
func (*closeTrackingRuntimeSession) CaptureComputer(context.Context) (*vm.ComputerSnapshot, error) {
	return nil, errTestLiveCapture
}
func (s *closeTrackingRuntimeSession) Close(context.Context) error {
	s.closed++
	return s.err
}

func (*blockingCloseRuntimeSession) Stream() vm.Stream { return nil }
func (*blockingCloseRuntimeSession) OpenStream(context.Context) (vm.Stream, error) {
	return nil, nil
}
func (*blockingCloseRuntimeSession) Wait(context.Context) error { return nil }
func (*blockingCloseRuntimeSession) CaptureComputer(context.Context) (*vm.ComputerSnapshot, error) {
	return nil, errTestLiveCapture
}
func (s *blockingCloseRuntimeSession) Close(ctx context.Context) error {
	s.once.Do(func() { close(s.started) })
	select {
	case <-s.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *countingRuntimeBackend) Cleanup(context.Context, vm.Owner) error {
	c.calls.Add(1)
	return nil
}

func (c *blockingMaterializingBackend) Cleanup(context.Context, vm.Owner) error { return nil }
func (c *blockingMaterializingBackend) Materialize(ctx context.Context, request vm.MaterializeRequest) (vm.CheckpointableMachine, error) {
	request.RecordPhase(vm.RuntimePhase{Name: "test_materialize", DurationMs: 1})
	c.started <- request.ID
	if request.ID == c.failID {
		return nil, errors.New("materialize failed")
	}
	<-ctx.Done()
	if c.canceled != nil {
		c.canceled <- request.ID
	}
	return nil, ctx.Err()
}

func (*stuckPreparedMachineSession) Stream() vm.Stream { return nil }
func (*stuckPreparedMachineSession) OpenStream(context.Context) (vm.Stream, error) {
	return nil, nil
}
func (s *stuckPreparedMachineSession) Wait(context.Context) error {
	close(s.waitStarted)
	<-s.releaseWait
	return nil
}
func (*stuckPreparedMachineSession) Close(context.Context) error { return nil }
func (*stuckPreparedMachineSession) CaptureComputer(context.Context) (*vm.ComputerSnapshot, error) {
	return nil, errTestLiveCapture
}

func TestPreparedMachinesCloseHonorsDeadlineWhileMonitorIsStuck(t *testing.T) {
	session := &stuckPreparedMachineSession{waitStarted: make(chan struct{}), releaseWait: make(chan struct{})}
	machines := NewPreparedMachines(nil, nil, 1, nil)
	machines.ComputerInstances = &typedRuntimeClient{}
	entry := preparedMachineEntry{
		session: session, machineKey: "runtime-key", computerInstanceID: "runtime-1", runtimeEpoch: 7,
		target: workerapi.RuntimeReconcileTarget{ID: "runtime-1", WorkerEpoch: 7, DesiredVersion: 1, ObservedVersion: 0},
		exit:   newPreparedMachineSignal(), ready: newPreparedMachineSignal(),
	}
	machines.mu.Lock()
	machines.entries[entry.machineKey] = []preparedMachineEntry{entry}
	machines.monitorReadyEntryLocked(entry.machineKey, entry)
	machines.mu.Unlock()
	<-session.waitStarted
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	started := time.Now()
	err := machines.Close(ctx)
	if err == nil || !strings.Contains(err.Error(), "background tasks") || time.Since(started) > time.Second {
		t.Fatalf("Close() error = %v, elapsed = %s", err, time.Since(started))
	}
	close(session.releaseWait)
	retryCtx, retryCancel := context.WithTimeout(context.Background(), time.Second)
	defer retryCancel()
	if err := machines.Close(retryCtx); err != nil {
		t.Fatalf("retry Close() = %v", err)
	}
}

func (c *typedRuntimeClient) ListRuntimeReconcileTargets(ctx context.Context) (workerapi.RuntimeReconcileResponse, error) {
	if len(c.targets) == 0 {
		<-ctx.Done()
		return workerapi.RuntimeReconcileResponse{}, ctx.Err()
	}
	target := c.targets[0]
	c.targets = c.targets[1:]
	return target, nil
}

func (c *batchRuntimeClient) ListRuntimeReconcileTargets(context.Context) (workerapi.RuntimeReconcileResponse, error) {
	calls := c.calls.Add(1)
	if c.polled != nil {
		select {
		case c.polled <- struct{}{}:
		default:
		}
	}
	if !c.repeat && calls > 1 {
		return workerapi.RuntimeReconcileResponse{Items: []workerapi.RuntimeReconcileTarget{}}, nil
	}
	return c.response, nil
}

func (*batchRuntimeClient) MarkComputerInstanceReady(context.Context, workerapi.ComputerInstanceStateRequest) (workerapi.ComputerInstance, error) {
	return workerapi.ComputerInstance{}, nil
}

func (*batchRuntimeClient) MarkComputerInstanceClosed(_ context.Context, request workerapi.ComputerInstanceStateRequest) (workerapi.ComputerInstance, error) {
	return workerapi.ComputerInstance{ID: request.ID}, nil
}

func (*batchRuntimeClient) MarkComputerInstanceFailed(_ context.Context, request workerapi.ComputerInstanceStateRequest) (workerapi.ComputerInstance, error) {
	return workerapi.ComputerInstance{ID: request.ID}, nil
}
func (c *typedRuntimeClient) MarkComputerInstanceReady(context.Context, workerapi.ComputerInstanceStateRequest) (workerapi.ComputerInstance, error) {
	return workerapi.ComputerInstance{}, nil
}
func (c *typedRuntimeClient) MarkComputerInstanceClosed(_ context.Context, request workerapi.ComputerInstanceStateRequest) (workerapi.ComputerInstance, error) {
	c.closed = append(c.closed, request)
	return workerapi.ComputerInstance{ID: request.ID}, nil
}
func (c *typedRuntimeClient) MarkComputerInstanceFailed(_ context.Context, request workerapi.ComputerInstanceStateRequest) (workerapi.ComputerInstance, error) {
	c.failed = append(c.failed, request)
	if len(c.failedErrors) > 0 {
		err := c.failedErrors[0]
		c.failedErrors = c.failedErrors[1:]
		return workerapi.ComputerInstance{}, err
	}
	return workerapi.ComputerInstance{ID: request.ID}, nil
}

func TestStopRuntimeTargetRequiresExclusiveMatchingLocalEpoch(t *testing.T) {
	session := &closeTrackingRuntimeSession{}
	machines := NewPreparedMachines(nil, nil, 1, nil)
	machines.entries["runtime-key"] = []preparedMachineEntry{{session: session, computerInstanceID: "runtime-1", runtimeEpoch: 7}}
	client := &typedRuntimeClient{}
	target := workerapi.RuntimeReconcileTarget{ID: "runtime-1", WorkerEpoch: 7, DesiredVersion: 2, ObservedVersion: 1}
	if err := machines.stopRuntimeTarget(context.Background(), client, target); err != nil {
		t.Fatal(err)
	}
	if len(client.closed) != 1 || client.closed[0].ID != "runtime-1" || client.closed[0].WorkerEpoch != 7 {
		t.Fatalf("closed = %+v", client.closed)
	}
	if proof := client.closed[0].CleanupProof; proof == nil || proof.Method != workerapi.RuntimeCleanupSessionClosed || proof.CompletedAt.IsZero() {
		t.Fatalf("cleanup proof = %+v, want closed session", proof)
	}
	if session.closed != 1 {
		t.Fatalf("session close count = %d, want 1", session.closed)
	}
	if err := machines.stopRuntimeTarget(context.Background(), client, target); err == nil {
		t.Fatal("second controller teardown unexpectedly acquired the same runtime")
	}
}

func TestStopRuntimeTargetDefersToCheckedOutComputerRuntime(t *testing.T) {
	machines := NewPreparedMachines(nil, nil, 1, nil)
	ref := preparedMachineRef{id: "runtime-1", epoch: 7}
	machines.mu.Lock()
	claim := machines.claimLocked(ref, serverClaim, preparedMachineEntry{})
	machines.mu.Unlock()
	checkout := &machineCheckout{machines: machines, ref: ref, gen: claim.gen}
	client := &typedRuntimeClient{}
	target := workerapi.RuntimeReconcileTarget{ID: "runtime-1", WorkerEpoch: 7, DesiredVersion: 2, ObservedVersion: 1}
	if err := machines.stopRuntimeTarget(context.Background(), client, target); err != nil {
		t.Fatal(err)
	}
	if len(client.closed) != 0 {
		t.Fatalf("checked-out runtime was closed by the machines reconciler: %+v", client.closed)
	}
	if err := checkout.Release(); err != nil {
		t.Fatal(err)
	}
	if err := machines.stopRuntimeTarget(context.Background(), client, target); err == nil {
		t.Fatal("untracked runtime teardown unexpectedly succeeded")
	}
}

func TestStopRuntimeTargetReconcilesMissingLocalRuntimeExactly(t *testing.T) {
	cleaner := &cleanupRuntimeBackend{}
	machines := NewPreparedMachines(cleaner, nil, 1, nil)
	client := &typedRuntimeClient{}
	target := workerapi.RuntimeReconcileTarget{ID: "runtime-1", WorkerEpoch: 7, DesiredVersion: 2, ObservedVersion: 1}

	if err := machines.stopRuntimeTarget(context.Background(), client, target); err != nil {
		t.Fatal(err)
	}
	if len(cleaner.cleaned) != 1 || cleaner.cleaned[0] != target.ID {
		t.Fatalf("cleaned = %v, want [%s]", cleaner.cleaned, target.ID)
	}
	if len(client.closed) != 1 {
		t.Fatalf("closed = %+v, want one transition", client.closed)
	}
	proof := client.closed[0].CleanupProof
	if proof == nil || proof.Method != workerapi.RuntimeCleanupHostReconciled || proof.CompletedAt.IsZero() {
		t.Fatalf("cleanup proof = %+v, want host reconciliation", proof)
	}
}

func TestStopRuntimeTargetDoesNotCloseWhenExactCleanupFails(t *testing.T) {
	cleaner := &cleanupRuntimeBackend{err: errors.New("cleanup failed")}
	machines := NewPreparedMachines(cleaner, nil, 1, nil)
	client := &typedRuntimeClient{}
	target := workerapi.RuntimeReconcileTarget{ID: "runtime-1", WorkerEpoch: 7, DesiredVersion: 2, ObservedVersion: 1}

	if err := machines.stopRuntimeTarget(context.Background(), client, target); err == nil {
		t.Fatal("cleanup failure unexpectedly closed runtime")
	}
	if len(client.closed) != 0 {
		t.Fatalf("closed = %+v, want no transition", client.closed)
	}
}

func TestReconcileDesiredRuntimesStopsCleanly(t *testing.T) {
	machines := NewPreparedMachines(nil, nil, 1, nil)
	client := &typedRuntimeClient{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- machines.ReconcileDesiredRuntimes(ctx, client) }()
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

func TestReconcileDesiredRuntimesSkipsActiveRedelivery(t *testing.T) {
	connector := &countingRuntimeBackend{}
	machines := NewPreparedMachines(connector, nil, 2, nil)
	session := &blockingCloseRuntimeSession{started: make(chan struct{}), release: make(chan struct{})}
	target := workerapi.RuntimeReconcileTarget{ID: "runtime-1", WorkerEpoch: 7, Action: workerapi.RuntimeReconcileClose}
	machines.entries[target.ID] = []preparedMachineEntry{{session: session, computerInstanceID: target.ID, runtimeEpoch: 7}}
	client := &batchRuntimeClient{
		response: workerapi.RuntimeReconcileResponse{Items: []workerapi.RuntimeReconcileTarget{target}},
		polled:   make(chan struct{}, 4),
		repeat:   true,
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- machines.ReconcileDesiredRuntimes(ctx, client) }()
	for range 2 {
		select {
		case <-client.polled:
		case <-time.After(time.Second):
			t.Fatal("reconciler did not repoll")
		}
	}
	if connector.calls.Load() != 0 {
		t.Fatal("active runtime redelivery started duplicate cleanup")
	}
	cancel()
	if err := <-done; err != context.Canceled {
		t.Fatalf("error = %v", err)
	}
}

func TestReconcileDesiredRuntimesBacksOffWhenCapacityIsFull(t *testing.T) {
	store, mount := testComputerMountArtifacts(t)
	connector := &blockingMaterializingBackend{started: make(chan string, 1)}
	machines := NewPreparedMachines(connector, store, 2, nil)
	machines.TempDir = t.TempDir()
	machines.RuntimeArchitecture = definition.RuntimeArchitecture("x86_64")
	machines.Reservations = newPreparedMachineReservations(t, 1)
	machines.ComputerInstances = &batchRuntimeClient{}
	if err := machines.reserveRuntimeCapacity(runtimeReservationTarget("occupied", 7)); err != nil {
		t.Fatal(err)
	}
	target := runtimePreparationTarget(mount, uuid.NewV7().String(), 7)
	client := &batchRuntimeClient{
		response: workerapi.RuntimeReconcileResponse{Items: []workerapi.RuntimeReconcileTarget{target}},
		polled:   make(chan struct{}, 4),
	}
	machines.ComputerInstances = client
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- machines.ReconcileDesiredRuntimes(ctx, client) }()
	select {
	case <-client.polled:
	case <-time.After(time.Second):
		t.Fatal("reconciler did not poll")
	}
	time.Sleep(250 * time.Millisecond)
	if calls := client.calls.Load(); calls != 1 {
		t.Fatalf("capacity-full poll calls = %d, want 1", calls)
	}
	select {
	case id := <-connector.started:
		t.Fatalf("capacity-rejected runtime %q reached materialization", id)
	default:
	}
	cancel()
	if err := <-done; err != context.Canceled {
		t.Fatalf("error = %v", err)
	}
}

func TestWarmRuntimeTargetRejectsMissingComputerBeforeAdmission(t *testing.T) {
	machines := NewPreparedMachines(nil, nil, 1, nil)
	admissionCalls := 0
	machines.AdmitRuntimeStart = func(context.Context) error {
		admissionCalls++
		return errors.New("disk_floor")
	}
	target := retryableWarmTarget()
	target.Source.Computer = nil
	err := machines.warmRuntimeTarget(context.Background(), &typedRuntimeClient{}, target, func() {})
	if err == nil || !strings.Contains(err.Error(), "runtime computer source is required") {
		t.Fatalf("error = %v, want missing Computer source", err)
	}
	if admissionCalls != 0 {
		t.Fatalf("runtime admission calls = %d, want 0", admissionCalls)
	}
}

func TestWarmRuntimeTargetHonorsHardAdmissionBeforeMaterialization(t *testing.T) {
	machines := NewPreparedMachines(nil, nil, 1, nil)
	admissionErr := errors.New("disk_floor")
	machines.AdmitRuntimeStart = func(context.Context) error { return admissionErr }
	target := retryableWarmTarget()
	err := machines.warmRuntimeTarget(context.Background(), &typedRuntimeClient{}, target, func() {})
	if !errors.Is(err, admissionErr) {
		t.Fatalf("error = %v, want hard admission error", err)
	}
}

func TestWarmRuntimeTargetStartsWhileUnrelatedRunIsBorrowed(t *testing.T) {
	registry := NewMounts()
	unregister := registry.register(
		workerapi.ComputerInstanceAssignment{ComputerInstanceID: "unrelated-instance"},
		newInstanceMount(&closeTrackingRuntimeSession{}),
		"channel-token",
	)
	defer unregister()
	borrowed, err := registry.OpenChannel(context.Background(), "unrelated-instance")
	if err != nil {
		t.Fatal(err)
	}
	defer borrowed.Channel.Close(context.Background())

	machines := NewPreparedMachines(&cleanupRuntimeBackend{}, unavailableRuntimeCAS{}, 1, nil)
	client := &typedRuntimeClient{}
	machines.ComputerInstances = client
	if err := machines.warmRuntimeTarget(context.Background(), client, retryableWarmTarget(), func() {}); err != nil {
		t.Fatal(err)
	}
	if len(client.failed) != 1 {
		t.Fatalf("runtime preparation attempts = %d, want 1", len(client.failed))
	}
}

func TestWarmRuntimeTargetRetriesCapacityBackpressureWithoutDurableFailure(t *testing.T) {
	machines := NewPreparedMachines(&cleanupRuntimeBackend{}, unavailableRuntimeCAS{}, 1, nil)
	machines.entries["occupied"] = []preparedMachineEntry{{computerInstanceID: "occupied", runtimeEpoch: 7}}
	client := &typedRuntimeClient{}

	err := machines.warmRuntimeTarget(context.Background(), client, retryableWarmTarget(), func() {})
	assertRuntimeCapacityBackpressure(t, err)
	if len(client.failed) != 0 {
		t.Fatalf("capacity backpressure mutated durable runtime: %+v", client.failed)
	}

	machines.mu.Lock()
	delete(machines.entries, "occupied")
	retryEligible := machines.reservedCountLocked() < machines.Size
	machines.mu.Unlock()
	if !retryEligible {
		t.Fatal("released local capacity did not make retry eligible")
	}
}

func TestPreparedMachineCapacityReservationLivesThroughCheckout(t *testing.T) {
	target := runtimeReservationTarget("019c10d5-a6f7-7af1-8f5f-000000000510", 7)
	machines := NewPreparedMachines(nil, nil, 1, nil)
	machines.Reservations = newPreparedMachineReservations(t, 1)
	if err := machines.reserveRuntimeCapacity(target); err != nil {
		t.Fatal(err)
	}
	wantKey := runtimeReservationKey(target.ID, target.WorkerEpoch)
	wantVector := reservation.Vector{
		CPUMillis: 1000, MemoryBytes: 512 << 20, GuestEphemeralDiskBytes: 1024 << 20,
		VMSlots: 1,
	}
	if got := machines.Reservations.Snapshot().Reservations[wantKey]; got != wantVector {
		t.Fatalf("reservation = %+v, want %+v", got, wantVector)
	}

	mount := preparedMachineComputerMountFromSource(target.Source)
	mount.ComputerInstanceID = target.ID
	mount.RuntimeEpoch = target.WorkerEpoch
	key := computerInstanceIDFromComputerMount(mount)
	ready := newPreparedMachineSignal()
	ready.finish(nil)
	machines.entries[key] = []preparedMachineEntry{{
		session: &closeTrackingRuntimeSession{}, machineKey: key,
		computerInstanceID: target.ID, runtimeEpoch: target.WorkerEpoch,
		target: target, exit: newPreparedMachineSignal(), ready: ready,
	}}

	checkout, _, ok := machines.checkout(context.Background(), mount)
	if !ok {
		t.Fatal("reserved runtime was not checked out")
	}
	if got := len(machines.Reservations.Snapshot().Reservations); got != 1 {
		t.Fatalf("reservations after checkout = %d, want 1", got)
	}
	if err := checkout.Release(); err != nil {
		t.Fatal(err)
	}
	if got := len(machines.Reservations.Snapshot().Reservations); got != 0 {
		t.Fatalf("reservations after successful checkout cleanup = %d, want 0", got)
	}
}

func TestPreparedMachineSourcePreservesComputerReservationAuthority(t *testing.T) {
	source := workerapi.RuntimeSource{
		ComputerID:     "019c10d5-a6f7-7af1-8f5f-000000000701",
		ComputerSpecID: "019c10d5-a6f7-7af1-8f5f-000000000702",
		Computer:       &workerapi.RuntimeComputerSource{VersionID: "019c10d5-a6f7-7af1-8f5f-000000000703"},
	}
	mount := preparedMachineComputerMountFromSource(source)
	if mount.ComputerID != source.ComputerID || mount.Target.BaseComputerDiskVersionID != source.Computer.VersionID {
		t.Fatalf("mount = %#v, want reserved Computer version without a tree artifact", mount)
	}
}

func TestPreparedMachineRejectsComputerArchitectureOutsideWorkerCertification(t *testing.T) {
	machines := NewPreparedMachines(nil, nil, 1, nil)
	machines.RuntimeArchitecture = definition.ArchitectureX8664
	_, closeProgram, err := machines.prepareProgram(
		context.Background(),
		t.TempDir(),
		workerapi.RuntimeReconcileTarget{Source: workerapi.RuntimeSource{
			ComputerArchitecture: "aarch64",
		}},
	)
	if closeErr := closeProgram(); closeErr != nil {
		t.Fatal(closeErr)
	}
	if err == nil || !strings.Contains(err.Error(), "does not match computer architecture") {
		t.Fatalf("error = %v", err)
	}
}

func TestPreparedMachineBindsProgramIndexToDeploymentReceipt(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	index := artifact.ProgramIndex{
		Architecture:       definition.ArchitectureX8664,
		ConfigResultDigest: digest,
		Declarations: []artifact.ProgramIndexDeclaration{{
			Kind:       definition.KindTask,
			DeclaredID: "task",
			Task: &definition.TaskManifest{
				Payload: definition.SchemaManifest{Kind: definition.SchemaKindNone},
				Run: definition.RunManifest{
					Queue:         "task/task",
					MaxDurationMs: 900000,
					Retry:         definition.RetryManifest{Enabled: false},
				},
			},
			Locator: &artifact.ProgramLocator{
				ExportName: "task",
				ModulePath: "helmr/app/entry-0.mjs",
				Slot:       artifact.DeclarationSlotHandler,
			},
		}},
		Queues: []definition.QueueInput{{
			Name: "task/task",
		}},
		RuntimeContract: definition.RuntimeContract,
		RuntimeDigest:   "sha256:" + strings.Repeat("f", 64),
	}
	canonical, err := artifact.CanonicalProgramIndex(index)
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyProgramIndexDigest(index, sha256sum.DigestBytes(canonical)); err != nil {
		t.Fatal(err)
	}
	if err := verifyProgramIndexDigest(
		index,
		"sha256:"+strings.Repeat("b", 64),
	); err == nil {
		t.Fatal("mismatched Program index digest was accepted")
	}
}

func TestPreparedMachineCapacityExhaustionIsRetryableBackpressure(t *testing.T) {
	machines := NewPreparedMachines(nil, nil, 2, nil)
	machines.Reservations = newPreparedMachineReservations(t, 1)
	first := runtimeReservationTarget("019c10d5-a6f7-7af1-8f5f-000000000511", 7)
	if err := machines.reserveRuntimeCapacity(first); err != nil {
		t.Fatal(err)
	}
	assertRuntimeCapacityBackpressure(t, machines.reserveRuntimeCapacity(first))

	err := machines.reserveRuntimeCapacity(runtimeReservationTarget("019c10d5-a6f7-7af1-8f5f-000000000512", 7))
	assertRuntimeCapacityBackpressure(t, err)
	if got := len(machines.Reservations.Snapshot().Reservations); got != 1 {
		t.Fatalf("reservations = %d, want 1", got)
	}
}

func TestPreparedMachineCapacityRejectsNegativeGuestDisk(t *testing.T) {
	machines := NewPreparedMachines(nil, nil, 1, nil)
	machines.Reservations = newPreparedMachineReservations(t, 1)
	target := runtimeReservationTarget("019c10d5-a6f7-7af1-8f5f-000000000514", 7)
	target.Source.ReservedDiskMiB = -1
	if err := machines.reserveRuntimeCapacity(target); err == nil {
		t.Fatal("negative guest disk capacity unexpectedly reserved")
	}
	if got := len(machines.Reservations.Snapshot().Reservations); got != 0 {
		t.Fatalf("reservations = %d, want 0", got)
	}
}

func TestPreparedMachineCloseFailureRetainsCapacityUntilReclaim(t *testing.T) {
	target := runtimeReservationTarget("019c10d5-a6f7-7af1-8f5f-000000000513", 7)
	connector := &cleanupRuntimeBackend{}
	machines := NewPreparedMachines(connector, nil, 1, nil)
	machines.Reservations = newPreparedMachineReservations(t, 1)
	if err := machines.reserveRuntimeCapacity(target); err != nil {
		t.Fatal(err)
	}
	session := &closeTrackingRuntimeSession{err: errors.New("close failed")}
	machines.entries["runtime-key"] = []preparedMachineEntry{{
		session: session, machineKey: "runtime-key",
		computerInstanceID: target.ID, runtimeEpoch: target.WorkerEpoch, target: target,
	}}
	client := &typedRuntimeClient{}

	if err := machines.stopRuntimeTarget(context.Background(), client, target); err == nil {
		t.Fatal("close failure unexpectedly succeeded")
	}
	if got := len(machines.Reservations.Snapshot().Reservations); got != 1 {
		t.Fatalf("reservations after close failure = %d, want 1", got)
	}
	if err := machines.reclaimFailedRuntimeTarget(context.Background(), client, target); err != nil {
		t.Fatal(err)
	}
	if got := len(machines.Reservations.Snapshot().Reservations); got != 0 {
		t.Fatalf("reservations after exact reclaim = %d, want 0", got)
	}
}

func newPreparedMachineReservations(t *testing.T, vmSlots int64) *reservation.Ledger {
	t.Helper()
	ledger, err := reservation.New(reservation.Vector{
		CPUMillis: 1000 * vmSlots, MemoryBytes: vmSlots * 512 << 20, GuestEphemeralDiskBytes: vmSlots * 1024 << 20,
		VMSlots: vmSlots,
	})
	if err != nil {
		t.Fatal(err)
	}
	return ledger
}

func runtimePreparationTarget(mount workerapi.ComputerInstanceAssignment, id string, epoch int64) workerapi.RuntimeReconcileTarget {
	target := retryableWarmTarget()
	target.ID = id
	target.WorkerEpoch = epoch
	target.Source.VMPlatformID = mount.VMPlatformID
	target.Source.RootfsDigest = mount.RootfsDigest
	return target
}

func runtimeReservationTarget(id string, epoch int64) workerapi.RuntimeReconcileTarget {
	return workerapi.RuntimeReconcileTarget{
		ID: id, WorkerEpoch: epoch,
		Source: workerapi.RuntimeSource{
			ComputerSpecID:    "019c10d5-a6f7-7af1-8f5f-000000000703",
			Computer:          &workerapi.RuntimeComputerSource{VersionID: "019c10d5-a6f7-7af1-8f5f-000000000704"},
			ReservedCPUMillis: 1000, ReservedMemoryMiB: 512, ReservedDiskMiB: 1024,
			ReservedExecutionSlots: 5,
		},
	}
}

func retryableWarmTarget() workerapi.RuntimeReconcileTarget {
	return workerapi.RuntimeReconcileTarget{
		ID: "019c10d5-a6f7-7af1-8f5f-000000000503", WorkerEpoch: 7, DesiredVersion: 1, Action: workerapi.RuntimeReconcilePrepare, PreparationExpiresAt: time.Now().Add(time.Minute),
		Source: workerapi.RuntimeSource{
			ComputerID:           "019c10d5-a6f7-7af1-8f5f-000000000702",
			ComputerSpecID:       "019c10d5-a6f7-7af1-8f5f-000000000703",
			ComputerArchitecture: "x86_64", ReservedCPUMillis: 1000, ReservedMemoryMiB: 512, ReservedDiskMiB: disk.SeedCapacity / mebibyte, ReservedExecutionSlots: 1,
			Computer: &workerapi.RuntimeComputerSource{VersionID: "019c10d5-a6f7-7af1-8f5f-000000000704", LogicalBytes: disk.SeedCapacity, Root: ptrGenerationRoot(disk.SeedCapacity)},
		},
	}
}

func assertRuntimeCapacityBackpressure(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, errPreparedMachineCapacityBusy) {
		t.Fatalf("error = %v, want capacity backpressure", err)
	}
}

func TestReclaimFailedRuntimeTargetPersistsProofOnlyAfterExactHostCleanup(t *testing.T) {
	connector := &cleanupRuntimeBackend{}
	machines := NewPreparedMachines(connector, nil, 1, nil)
	client := &typedRuntimeClient{}
	target := workerapi.RuntimeReconcileTarget{
		ID: "019c10d5-a6f7-7af1-8f5f-000000000501", WorkerEpoch: 7,
		DesiredVersion: 2, ObservedVersion: 4, Action: workerapi.RuntimeReconcileReclaim,
	}
	if err := machines.reclaimFailedRuntimeTarget(context.Background(), client, target); err != nil {
		t.Fatal(err)
	}
	if len(connector.cleaned) != 1 || connector.cleaned[0] != target.ID {
		t.Fatalf("cleaned = %v", connector.cleaned)
	}
	if len(client.failed) != 1 || client.failed[0].CleanupProof == nil || client.failed[0].CleanupProof.Method != workerapi.RuntimeCleanupHostReconciled {
		t.Fatalf("failed transition = %+v", client.failed)
	}
}

func TestReclaimFailedRuntimeTargetKeepsQuarantineWhenCleanupIsAmbiguous(t *testing.T) {
	connector := &cleanupRuntimeBackend{err: errors.New("process still alive")}
	machines := NewPreparedMachines(connector, nil, 1, nil)
	client := &typedRuntimeClient{}
	target := workerapi.RuntimeReconcileTarget{ID: "019c10d5-a6f7-7af1-8f5f-000000000502", WorkerEpoch: 7}
	if err := machines.reclaimFailedRuntimeTarget(context.Background(), client, target); err == nil {
		t.Fatal("ambiguous cleanup unexpectedly succeeded")
	}
	if len(client.failed) != 0 {
		t.Fatalf("cleanup proof persisted after failure: %+v", client.failed)
	}
}

func TestReclaimFailedCheckedOutRuntimeClearsExactCheckoutAfterPhysicalCleanup(t *testing.T) {
	target := runtimeReservationTarget("019c10d5-a6f7-7af1-8f5f-000000000515", 7)
	target.DesiredVersion = 2
	target.ObservedVersion = 4
	connector := &cleanupRuntimeBackend{}
	machines := NewPreparedMachines(connector, nil, 1, nil)
	machines.Reservations = newPreparedMachineReservations(t, 1)
	if err := machines.reserveRuntimeCapacity(target); err != nil {
		t.Fatal(err)
	}
	machines.mu.Lock()
	machines.claimLocked(preparedMachineRef{id: target.ID, epoch: target.WorkerEpoch}, serverClaim, preparedMachineEntry{})
	machines.claimLocked(preparedMachineRef{id: "019c10d5-a6f7-7af1-8f5f-000000000516", epoch: target.WorkerEpoch}, serverClaim, preparedMachineEntry{})
	machines.mu.Unlock()
	client := &typedRuntimeClient{}

	if err := machines.reclaimFailedRuntimeTarget(context.Background(), client, target); err != nil {
		t.Fatal(err)
	}
	if machines.runtimeCheckedOut(target.ID, target.WorkerEpoch) {
		t.Fatal("reclaimed runtime remains checked out")
	}
	if !machines.runtimeCheckedOut("019c10d5-a6f7-7af1-8f5f-000000000516", target.WorkerEpoch) {
		t.Fatal("reclaim cleared a different checkout")
	}
	if got := len(machines.Reservations.Snapshot().Reservations); got != 0 {
		t.Fatalf("reservations after reclaim = %d, want 0", got)
	}
	if len(connector.cleaned) != 1 || connector.cleaned[0] != target.ID {
		t.Fatalf("cleaned = %v, want exact runtime", connector.cleaned)
	}
	if len(client.failed) != 1 || client.failed[0].CleanupProof == nil ||
		client.failed[0].CleanupProof.Method != workerapi.RuntimeCleanupHostReconciled {
		t.Fatalf("cleanup proof = %+v, want host reconciliation", client.failed)
	}
}

func TestReclaimFailedCheckedOutRuntimeRetainsCheckoutWhenPhysicalCleanupFails(t *testing.T) {
	target := runtimeReservationTarget("019c10d5-a6f7-7af1-8f5f-000000000517", 7)
	connector := &cleanupRuntimeBackend{err: errors.New("runtime still exists")}
	machines := NewPreparedMachines(connector, nil, 1, nil)
	machines.Reservations = newPreparedMachineReservations(t, 1)
	if err := machines.reserveRuntimeCapacity(target); err != nil {
		t.Fatal(err)
	}
	machines.mu.Lock()
	machines.claimLocked(preparedMachineRef{id: target.ID, epoch: target.WorkerEpoch}, serverClaim, preparedMachineEntry{})
	machines.mu.Unlock()
	client := &typedRuntimeClient{}

	if err := machines.reclaimFailedRuntimeTarget(context.Background(), client, target); err == nil {
		t.Fatal("cleanup failure unexpectedly reclaimed runtime")
	}
	if !machines.runtimeCheckedOut(target.ID, target.WorkerEpoch) {
		t.Fatal("cleanup failure cleared checkout")
	}
	if got := len(machines.Reservations.Snapshot().Reservations); got != 1 {
		t.Fatalf("reservations after cleanup failure = %d, want 1", got)
	}
	if len(client.failed) != 0 {
		t.Fatalf("cleanup failure persisted proof: %+v", client.failed)
	}
}

func TestReclaimFailedCheckedOutRuntimeRetriesProofAfterLocalRelease(t *testing.T) {
	target := runtimeReservationTarget("019c10d5-a6f7-7af1-8f5f-000000000518", 7)
	target.DesiredVersion = 2
	target.ObservedVersion = 4
	connector := &cleanupRuntimeBackend{}
	machines := NewPreparedMachines(connector, nil, 1, nil)
	machines.Reservations = newPreparedMachineReservations(t, 1)
	if err := machines.reserveRuntimeCapacity(target); err != nil {
		t.Fatal(err)
	}
	machines.mu.Lock()
	machines.claimLocked(preparedMachineRef{id: target.ID, epoch: target.WorkerEpoch}, serverClaim, preparedMachineEntry{})
	machines.mu.Unlock()
	client := &typedRuntimeClient{failedErrors: []error{errors.New("proof response lost")}}

	if err := machines.reclaimFailedRuntimeTarget(context.Background(), client, target); err == nil {
		t.Fatal("proof persistence failure unexpectedly succeeded")
	}
	if machines.runtimeCheckedOut(target.ID, target.WorkerEpoch) {
		t.Fatal("physical cleanup success retained checkout after proof failure")
	}
	if got := len(machines.Reservations.Snapshot().Reservations); got != 0 {
		t.Fatalf("reservations after physical cleanup = %d, want 0", got)
	}
	if err := machines.reclaimFailedRuntimeTarget(context.Background(), client, target); err != nil {
		t.Fatal(err)
	}
	if len(connector.cleaned) != 2 {
		t.Fatalf("physical cleanup attempts = %d, want 2 idempotent attempts", len(connector.cleaned))
	}
	if len(client.failed) != 2 {
		t.Fatalf("proof attempts = %d, want 2", len(client.failed))
	}
	for _, request := range client.failed {
		if request.CleanupProof == nil || request.CleanupProof.Method != workerapi.RuntimeCleanupHostReconciled {
			t.Fatalf("proof attempt = %+v, want host reconciliation", request)
		}
	}
}
