package worker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
)

type testControl struct {
	authenticated  atomic.Bool
	recovered      atomic.Bool
	activated      atomic.Bool
	renewed        atomic.Int32
	completed      atomic.Int32
	status         atomic.Value
	activateStatus atomic.Value
	observeStatus  atomic.Value
	renewStatus    atomic.Value
	completeErr    error
}

func (c *testControl) AuthenticateWorker(context.Context) error {
	c.authenticated.Store(true)
	return nil
}
func (c *testControl) ActivateWorker(_ context.Context, capabilities api.WorkerCapabilities) (api.WorkerStatusResponse, error) {
	if !c.authenticated.Load() {
		return api.WorkerStatusResponse{}, errors.New("activation before authentication")
	}
	if !c.recovered.Load() {
		return api.WorkerStatusResponse{}, errors.New("activation before startup recovery proof")
	}
	if len(capabilities.Observation.HealthDetails) == 0 {
		return api.WorkerStatusResponse{}, errors.New("recovery evidence missing")
	}
	c.activated.Store(true)
	if status, ok := c.activateStatus.Load().(api.WorkerStatusResponse); ok {
		return status, nil
	}
	return api.WorkerStatusResponse{Status: api.WorkerStatusActive}, nil
}

func (c *testControl) ReportWorkerStartupRecovery(_ context.Context, request api.WorkerStartupRecoveryRequest) error {
	if !request.InventoryComplete || request.InventoryScope != "worker_runtime_state_roots_v0" || request.ObservedAt.IsZero() {
		return errors.New("incomplete startup recovery proof")
	}
	c.recovered.Store(true)
	return nil
}
func (c *testControl) CompleteWorkerDrain(_ context.Context, request api.WorkerDrainCompletionRequest) (api.WorkerStatusResponse, error) {
	if !request.InventoryComplete || request.InventoryScope != "worker_runtime_state_roots_v0" || request.ObservedAt.IsZero() || len(request.Inventory) != 0 || len(request.Quarantined) != 0 || len(request.Errors) != 0 {
		return api.WorkerStatusResponse{}, errors.New("incomplete worker drain proof")
	}
	c.completed.Add(1)
	if c.completeErr != nil {
		return api.WorkerStatusResponse{}, c.completeErr
	}
	return api.WorkerStatusResponse{Status: api.WorkerStatusDisabled}, nil
}
func (c *testControl) returnedStatus() api.WorkerStatusResponse {
	if status, ok := c.status.Load().(api.WorkerStatusResponse); ok {
		return status
	}
	return api.WorkerStatusResponse{Status: api.WorkerStatusActive}
}
func (c *testControl) ObserveWorker(_ context.Context, observation api.WorkerObservation) (api.WorkerStatusResponse, error) {
	if observation.RunPausedReason == string(StateDraining) {
		return c.returnedStatus(), nil
	}
	if status, ok := c.observeStatus.Load().(api.WorkerStatusResponse); ok {
		return status, nil
	}
	return c.returnedStatus(), nil
}
func (c *testControl) RenewWorkerCertification(context.Context, api.WorkerCapabilities) (api.WorkerStatusResponse, error) {
	c.renewed.Add(1)
	if status, ok := c.renewStatus.Load().(api.WorkerStatusResponse); ok {
		return status, nil
	}
	return c.returnedStatus(), nil
}

type queuedConsumer struct {
	mu      sync.Mutex
	work    []Work
	claimed int
}

type blockingClaimConsumer struct {
	entered  chan struct{}
	canceled chan struct{}
}

type shutdownClaimConsumer struct {
	entered     chan struct{}
	allowReturn chan struct{}
	workStarted chan struct{}
	releaseWork chan struct{}
}

type enabledConsumer struct {
	enabled atomic.Bool
	inner   *queuedConsumer
}

func (c *enabledConsumer) Claim(ctx context.Context) (Work, bool, error) {
	if !c.enabled.Load() {
		return nil, false, nil
	}
	return c.inner.Claim(ctx)
}

func (c *shutdownClaimConsumer) Claim(ctx context.Context) (Work, bool, error) {
	close(c.entered)
	<-ctx.Done()
	<-c.allowReturn
	return func(context.Context) error {
		close(c.workStarted)
		<-c.releaseWork
		return nil
	}, true, nil
}

func (c *blockingClaimConsumer) Claim(ctx context.Context) (Work, bool, error) {
	close(c.entered)
	<-ctx.Done()
	close(c.canceled)
	return nil, false, ctx.Err()
}

func (c *queuedConsumer) Claim(context.Context) (Work, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.work) == 0 {
		return nil, false, nil
	}
	work := c.work[0]
	c.work = c.work[1:]
	c.claimed++
	return work, true, nil
}

func TestSupervisorRunsConcurrentWorkAndDrainsLocally(t *testing.T) {
	control := &testControl{}
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	consumer := &queuedConsumer{work: []Work{
		func(context.Context) error { started <- struct{}{}; <-release; return nil },
		func(context.Context) error { started <- struct{}{}; <-release; return nil },
	}}
	s, err := New(Config{
		Control: control, Capabilities: api.WorkerCapabilities{}, PollEvery: time.Millisecond,
		DrainTimeout: time.Second, Consumers: []ConsumerSpec{{Name: "run", Concurrency: 2, Consumer: consumer}},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	for range 2 {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("concurrent work did not start")
		}
	}
	cancel()
	deadline := time.Now().Add(time.Second)
	for s.state.Load().(State) != StateDraining {
		if time.Now().After(deadline) {
			t.Fatal("supervisor did not enter local draining state")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case err := <-done:
		t.Fatalf("returned before active work completed: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("run error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("supervisor did not stop")
	}
	if got := control.completed.Load(); got != 0 {
		t.Fatalf("ordinary process shutdown completed durable drain %d times", got)
	}
}

func TestSupervisorDrainTimeoutBoundsHungWork(t *testing.T) {
	control := &testControl{}
	started := make(chan struct{})
	release := make(chan struct{})
	consumer := &queuedConsumer{work: []Work{func(context.Context) error { close(started); <-release; return nil }}}
	s, err := New(Config{Control: control, PollEvery: time.Millisecond, DrainTimeout: 30 * time.Millisecond, Consumers: []ConsumerSpec{{Name: "run", Concurrency: 1, Consumer: consumer}}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	<-started
	cancel()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "timed out") {
			t.Fatalf("error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("drain timeout did not bound shutdown")
	}
	close(release)
}

func TestSupervisorShutdownCancelsOutstandingClaims(t *testing.T) {
	control := &testControl{}
	consumer := &blockingClaimConsumer{entered: make(chan struct{}), canceled: make(chan struct{})}
	s, err := New(Config{
		Control: control, PollEvery: time.Millisecond,
		Consumers: []ConsumerSpec{{Name: "run", Concurrency: 1, Consumer: consumer}},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	select {
	case <-consumer.entered:
	case <-time.After(time.Second):
		t.Fatal("claim did not start")
	}
	cancel()
	select {
	case <-consumer.canceled:
	case <-time.After(time.Second):
		t.Fatal("outstanding claim was not canceled")
	}
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("run error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("supervisor did not stop")
	}
}

func TestSupervisorShutdownWaitsForClaimThatReturnsCommittedWork(t *testing.T) {
	control := &testControl{}
	consumer := &shutdownClaimConsumer{
		entered: make(chan struct{}), allowReturn: make(chan struct{}),
		workStarted: make(chan struct{}), releaseWork: make(chan struct{}),
	}
	s, err := New(Config{
		Control: control, PollEvery: time.Millisecond, DrainTimeout: time.Second,
		Consumers: []ConsumerSpec{{Name: "run", Concurrency: 1, Consumer: consumer}},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	select {
	case <-consumer.entered:
	case <-time.After(time.Second):
		t.Fatal("claim did not start")
	}
	cancel()
	deadline := time.Now().Add(time.Second)
	for s.state.Load().(State) != StateDraining {
		if time.Now().After(deadline) {
			t.Fatal("supervisor did not enter draining state")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case err := <-done:
		t.Fatalf("returned while claim was still resolving: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(consumer.allowReturn)
	select {
	case <-consumer.workStarted:
	case <-time.After(time.Second):
		t.Fatal("committed work did not start")
	}
	select {
	case err := <-done:
		t.Fatalf("returned before committed work completed: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(consumer.releaseWork)
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("run error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("supervisor did not stop")
	}
}

func TestSupervisorHardAdmissionPausesClaimsButNotShutdown(t *testing.T) {
	control := &testControl{}
	now := time.Now()
	probe := &staticHealthProbe{health: healthyHost(now)}
	probe.health.AvailableDiskBytes = 1
	evaluator, err := NewHardAdmission(HardAdmissionConfig{
		Probe: probe, DiskFloorBytes: 2, FDHeadroom: 1, RuntimeSlotCount: 1, Now: time.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	consumer := &queuedConsumer{work: []Work{func(context.Context) error { return nil }}}
	s, err := New(Config{
		Control: control, PollEvery: time.Millisecond, CertificationTTL: time.Hour,
		AdmissionEvaluator: evaluator,
		Consumers:          []ConsumerSpec{{Name: "run", Concurrency: 1, Consumer: consumer}},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	time.Sleep(20 * time.Millisecond)
	consumer.mu.Lock()
	claimed := consumer.claimed
	consumer.mu.Unlock()
	if claimed != 0 {
		t.Fatalf("claimed %d jobs while disk hard admission was paused", claimed)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("run error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("paused supervisor did not drain")
	}
}

func TestSupervisorRenewsCertificationBeforeExpiry(t *testing.T) {
	control := &testControl{}
	s, err := New(Config{Control: control, ObservationEvery: time.Millisecond, CertificationTTL: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	deadline := time.After(time.Second)
	for control.renewed.Load() == 0 {
		select {
		case <-deadline:
			t.Fatal("certification was not renewed")
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	<-done
}

func TestServerDirectedDrainStopsExecutionAndCompletesAfterCleanup(t *testing.T) {
	control := &testControl{}
	control.status.Store(api.WorkerStatusResponse{Status: api.WorkerStatusActive})
	runStarted := make(chan struct{})
	runRelease := make(chan struct{})
	unexpectedRun := make(chan struct{}, 1)
	runs := &queuedConsumer{work: []Work{
		func(context.Context) error { close(runStarted); <-runRelease; return nil },
		func(context.Context) error { unexpectedRun <- struct{}{}; return nil },
	}}
	cleanupStarted := make(chan struct{})
	cleanupRelease := make(chan struct{})
	cleanup := &enabledConsumer{inner: &queuedConsumer{work: []Work{func(context.Context) error {
		close(cleanupStarted)
		<-cleanupRelease
		return nil
	}}}}
	finalized := make(chan struct{})
	s, err := New(Config{
		Control: control, PollEvery: time.Millisecond, ObservationEvery: time.Millisecond, CertificationTTL: time.Hour, DrainTimeout: time.Second,
		Consumers: []ConsumerSpec{
			{Name: "run", Concurrency: 1, Consumer: runs},
			{Name: "workspace-cleanup", Concurrency: 1, DrainEligible: true, Consumer: cleanup},
		},
		FinalizeDrain: func(context.Context) (RecoveryEvidence, error) {
			close(finalized)
			return RecoveryEvidence{ObservedAt: time.Now().UTC()}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	select {
	case <-runStarted:
	case <-time.After(time.Second):
		t.Fatal("run did not start")
	}
	control.status.Store(api.WorkerStatusResponse{Status: api.WorkerStatusDraining, ActiveExecutions: 1})
	deadline := time.Now().Add(time.Second)
	for s.state.Load().(State) != StateDraining {
		if time.Now().After(deadline) {
			t.Fatal("supervisor did not enter server-directed drain")
		}
		time.Sleep(time.Millisecond)
	}
	cleanup.enabled.Store(true)
	select {
	case <-cleanupStarted:
	case <-time.After(time.Second):
		t.Fatal("drain-eligible cleanup did not continue")
	}
	close(runRelease)
	close(cleanupRelease)
	select {
	case <-unexpectedRun:
		t.Fatal("execution consumer claimed new work after server-directed drain")
	case <-time.After(20 * time.Millisecond):
	}
	control.status.Store(api.WorkerStatusResponse{Status: api.WorkerStatusDraining, ActiveExecutions: 0})
	select {
	case <-finalized:
	case <-time.After(time.Second):
		t.Fatal("local drain was not finalized")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("server-directed drain error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("server-directed drain did not stop supervisor")
	}
	if got := control.completed.Load(); got != 1 {
		t.Fatalf("drain completion calls = %d, want 1", got)
	}
}

func TestActivationCanResumePreviouslyRequestedDrain(t *testing.T) {
	control := &testControl{}
	control.activateStatus.Store(api.WorkerStatusResponse{Status: api.WorkerStatusDraining})
	control.status.Store(api.WorkerStatusResponse{Status: api.WorkerStatusDraining})
	s, err := New(Config{
		Control: control, PollEvery: time.Millisecond, DrainTimeout: time.Second,
		FinalizeDrain: func(context.Context) (RecoveryEvidence, error) {
			return RecoveryEvidence{ObservedAt: time.Now().UTC()}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := control.completed.Load(); got != 1 {
		t.Fatalf("drain completion calls = %d, want 1", got)
	}
}

func TestDurableDrainLatchWinsWhenShutdownIsAlsoReady(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	control := &testControl{}
	control.status.Store(api.WorkerStatusResponse{Status: api.WorkerStatusDraining})
	s, err := New(Config{
		Control: control, PollEvery: time.Millisecond, ObservationEvery: time.Hour, CertificationTTL: 24 * time.Hour, DrainTimeout: time.Second,
		FinalizeDrain: func(context.Context) (RecoveryEvidence, error) {
			return RecoveryEvidence{ObservedAt: time.Now().UTC()}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	deadline := time.Now().Add(time.Second)
	for !control.activated.Load() || s.state.Load().(State) != StateActive {
		if time.Now().After(deadline) {
			t.Fatal("supervisor did not reach active select")
		}
		time.Sleep(time.Millisecond)
	}
	// Model the observation callback's ordering: it stores the durable latch
	// before publishing its wakeup. Cancellation is already ready when Run
	// resumes its select branch.
	s.state.Store(StateDraining)
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("latched durable drain = %v", err)
	}
	if got := control.completed.Load(); got != 1 {
		t.Fatalf("drain completion calls = %d, want 1", got)
	}
}

func TestSignalDuringDurableDrainDoesNotCancelCompletion(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	control := &testControl{}
	control.activateStatus.Store(api.WorkerStatusResponse{Status: api.WorkerStatusDraining})
	control.status.Store(api.WorkerStatusResponse{Status: api.WorkerStatusDraining})
	finalizeStarted := make(chan struct{})
	releaseFinalize := make(chan struct{})
	s, err := New(Config{
		Control: control, PollEvery: time.Millisecond, DrainTimeout: time.Second,
		FinalizeDrain: func(finalizeCtx context.Context) (RecoveryEvidence, error) {
			close(finalizeStarted)
			select {
			case <-releaseFinalize:
				return RecoveryEvidence{ObservedAt: time.Now().UTC()}, nil
			case <-finalizeCtx.Done():
				return RecoveryEvidence{}, finalizeCtx.Err()
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	<-finalizeStarted
	cancel()
	select {
	case err := <-done:
		t.Fatalf("signal canceled latched drain before finalization release: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(releaseFinalize)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("latched drain completion = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("latched drain did not complete")
	}
	if got := control.completed.Load(); got != 1 {
		t.Fatalf("drain completion calls = %d, want 1", got)
	}
}

func TestObservationAndRenewalResponsesTriggerDurableDrain(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*testControl)
	}{
		{
			name: "observation",
			setup: func(control *testControl) {
				control.observeStatus.Store(api.WorkerStatusResponse{Status: api.WorkerStatusDraining})
			},
		},
		{
			name: "certification renewal",
			setup: func(control *testControl) {
				control.observeStatus.Store(api.WorkerStatusResponse{Status: api.WorkerStatusActive})
				control.renewStatus.Store(api.WorkerStatusResponse{Status: api.WorkerStatusDraining})
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			control := &testControl{}
			control.status.Store(api.WorkerStatusResponse{Status: api.WorkerStatusDraining})
			tt.setup(control)
			s, err := New(Config{
				Control: control, PollEvery: time.Millisecond, ObservationEvery: time.Millisecond, CertificationTTL: 2 * time.Millisecond, DrainTimeout: time.Second,
				FinalizeDrain: func(context.Context) (RecoveryEvidence, error) {
					return RecoveryEvidence{ObservedAt: time.Now().UTC()}, nil
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := s.Run(context.Background()); err != nil {
				t.Fatal(err)
			}
			if got := control.completed.Load(); got != 1 {
				t.Fatalf("drain completion calls = %d, want 1", got)
			}
		})
	}
}

func TestServerDirectedDrainDoesNotCompleteOnTimeoutOrDirtyInventory(t *testing.T) {
	tests := []struct {
		name        string
		status      api.WorkerStatusResponse
		finalize    func(context.Context) (RecoveryEvidence, error)
		completeErr error
		wantError   string
	}{
		{
			name: "server authority timeout", status: api.WorkerStatusResponse{Status: api.WorkerStatusDraining, ActiveExecutions: 1},
			finalize: func(context.Context) (RecoveryEvidence, error) {
				return RecoveryEvidence{ObservedAt: time.Now().UTC()}, nil
			},
			wantError: "timed out",
		},
		{
			name: "quarantined local inventory", status: api.WorkerStatusResponse{Status: api.WorkerStatusDraining},
			finalize: func(context.Context) (RecoveryEvidence, error) {
				return RecoveryEvidence{ObservedAt: time.Now().UTC(), Quarantined: []string{"runtime"}, QuarantineErrors: []string{"busy"}}, nil
			},
			wantError: "inventory is not clean",
		},
		{
			name: "cleanup failure", status: api.WorkerStatusResponse{Status: api.WorkerStatusDraining},
			finalize: func(context.Context) (RecoveryEvidence, error) {
				return RecoveryEvidence{}, errors.New("cleanup failed")
			},
			wantError: "cleanup failed",
		},
		{
			name: "completion response loss", status: api.WorkerStatusResponse{Status: api.WorkerStatusDraining},
			finalize: func(context.Context) (RecoveryEvidence, error) {
				return RecoveryEvidence{ObservedAt: time.Now().UTC()}, nil
			},
			completeErr: errors.New("response lost; retry identical proof"),
			wantError:   "response lost; retry identical proof",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			control := &testControl{}
			control.completeErr = tt.completeErr
			control.activateStatus.Store(tt.status)
			control.status.Store(tt.status)
			s, err := New(Config{Control: control, PollEvery: time.Millisecond, DrainTimeout: 20 * time.Millisecond, FinalizeDrain: tt.finalize})
			if err != nil {
				t.Fatal(err)
			}
			err = s.Run(context.Background())
			if err == nil || !strings.Contains(err.Error(), tt.wantError) {
				t.Fatalf("error = %v, want %q", err, tt.wantError)
			}
			wantCompleted := int32(0)
			if tt.completeErr != nil {
				wantCompleted = 1
			}
			if got := control.completed.Load(); got != wantCompleted {
				t.Fatalf("drain completion calls = %d, want %d", got, wantCompleted)
			}
		})
	}
}

func TestSingletonRejectsSecondOwner(t *testing.T) {
	dir := t.TempDir()
	first, err := Acquire(dir, ProcessIdentity{ServiceID: "one", Roles: []string{"run"}})
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if _, err := Acquire(dir, ProcessIdentity{ServiceID: "two", Roles: []string{"run"}}); err == nil {
		t.Fatal("second singleton acquisition succeeded")
	}
	identity, err := ReadProcessIdentity(dir)
	if err != nil || identity.ServiceID != "one" {
		t.Fatalf("identity = %+v, err = %v", identity, err)
	}
}

func TestQuarantineStaleRuntimeState(t *testing.T) {
	dir := t.TempDir()
	stale := filepath.Join(dir, "vms", "guest", "runtime-1")
	if err := os.MkdirAll(stale, 0o700); err != nil {
		t.Fatal(err)
	}
	var stopped []int
	var deleted []string
	evidence, err := recoverLocalRuntimeState(context.Background(), dir, filepath.Join(dir, "jailer"), runtimeRecoveryOps{
		matchingPIDs: func(string) ([]int, error) { return []int{42}, nil },
		stopPID:      func(_ context.Context, pid int) error { stopped = append(stopped, pid); return nil },
		netnsExists:  func(context.Context, string) (bool, error) { return true, nil },
		deleteNetns:  func(_ context.Context, id string) error { deleted = append(deleted, id); return nil },
		removeAll:    os.RemoveAll,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(evidence.Reclaimed) != 1 || evidence.Reclaimed[0] != "runtime-1" || len(evidence.Quarantined) != 0 {
		t.Fatalf("evidence = %+v", evidence)
	}
	if len(stopped) != 1 || stopped[0] != 42 || len(deleted) != 1 {
		t.Fatalf("stopped=%v deleted=%v", stopped, deleted)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("live stale state still exists: %v", err)
	}
}

func TestRecoveryLeavesUnreclaimedStateInPlace(t *testing.T) {
	dir := t.TempDir()
	stale := filepath.Join(dir, "vms", "guest", "runtime-1")
	if err := os.MkdirAll(stale, 0o700); err != nil {
		t.Fatal(err)
	}
	evidence, err := recoverLocalRuntimeState(context.Background(), dir, "", runtimeRecoveryOps{
		matchingPIDs: func(string) ([]int, error) { return []int{42}, nil },
		stopPID:      func(context.Context, int) error { return errors.New("still running") },
		netnsExists:  func(context.Context, string) (bool, error) { return false, nil },
		deleteNetns:  func(context.Context, string) error { return nil },
		removeAll:    os.RemoveAll,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(evidence.Quarantined) != 1 {
		t.Fatalf("evidence = %+v", evidence)
	}
	if _, err := os.Stat(stale); err != nil {
		t.Fatalf("quarantined state was hidden or removed: %v", err)
	}
}
