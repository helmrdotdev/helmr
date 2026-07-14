package fleet

import (
	"context"
	"errors"
	"maps"
	"reflect"
	"sync"
	"testing"
	"time"
)

func TestControllerLeaderRacePerformsNoReadOrMutation(t *testing.T) {
	source := &fakeSource{snapshot: launchSnapshot()}
	provider := newFakeProvider(0)
	controller := newTestController(t, "run", source, &fakeLeaders{acquired: false}, provider, nil)
	if err := controller.Cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if source.snapshotCalls != 0 || provider.callCount() != 0 {
		t.Fatalf("lost leader race performed work: snapshot=%d provider=%d", source.snapshotCalls, provider.callCount())
	}
}

func TestControllerSnapshotFailureDoesNotMutateProvider(t *testing.T) {
	source := &fakeSource{snapshotErr: errors.New("database unavailable")}
	provider := newFakeProvider(0)
	controller := newTestController(t, "run", source, acquiredLeaders(), provider, nil)
	if err := controller.Cycle(context.Background()); err == nil {
		t.Fatal("Cycle succeeded, want snapshot failure")
	}
	if provider.callCount() != 0 {
		t.Fatalf("snapshot failure made %d provider calls", provider.callCount())
	}
}

func TestControllerProviderReadFailureDoesNotMutateProvider(t *testing.T) {
	source := &fakeSource{snapshot: launchSnapshot()}
	provider := newFakeProvider(0)
	provider.describeErr = errors.New("provider unavailable")
	controller := newTestController(t, "run", source, acquiredLeaders(), provider, nil)
	if err := controller.Cycle(context.Background()); err == nil {
		t.Fatal("Cycle succeeded, want provider read failure")
	}
	if provider.mutationCount() != 0 {
		t.Fatalf("provider read failure made %d provider mutations", provider.mutationCount())
	}
}

func TestControllerMetricsFailureNeverChangesScaling(t *testing.T) {
	source := &fakeSource{snapshot: launchSnapshot()}
	provider := newFakeProvider(0)
	metrics := &fakeMetrics{err: errors.New("metrics denied")}
	controller := newTestController(t, "run", source, acquiredLeaders(), provider, metrics)
	if err := controller.Cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if provider.desired() != 1 || len(source.confirmed) != 1 {
		t.Fatalf("metrics denial altered scaling: desired=%d confirmed=%#v", provider.desired(), source.confirmed)
	}
	if len(metrics.values) != 2 || metrics.values[0].Outcome != OutcomePlanned || metrics.values[1].Outcome != OutcomeConfirmed {
		t.Fatalf("metric values = %#v, want planned then confirmed", metrics.values)
	}
}

func TestControllerAmbiguousLaunchResponseReconcilesExactTarget(t *testing.T) {
	source := &fakeSource{snapshot: launchSnapshot()}
	provider := newFakeProvider(0)
	provider.ambiguousDesired = true
	controller := newTestController(t, "run", source, acquiredLeaders(), provider, nil)
	if err := controller.Cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := provider.desiredTargets; !reflect.DeepEqual(got, []int{1}) {
		t.Fatalf("desired targets = %v, want exact target [1]", got)
	}
	if len(source.confirmed) != 1 || source.confirmed[0].Desired != 1 || source.confirmed[0].Action != ActionLaunch {
		t.Fatalf("confirmed = %#v, want reconciled launch", source.confirmed)
	}
}

func TestControllerBeginDrainUsesExactPlannerVictim(t *testing.T) {
	snapshot := GroupSnapshot{Inputs: Inputs{Workers: []Worker{
		{ID: "busy", State: WorkerActive, AuthorityCount: 4, ActivatedAt: testNow.Add(-2 * time.Hour)},
		{ID: "idle", State: WorkerActive, ActivatedAt: testNow.Add(-time.Hour)},
	}}, ResourceIDs: map[string]string{"busy": "i-busy", "idle": "i-idle"}}
	source := &fakeSource{snapshot: snapshot, proofs: map[string]TerminationProof{
		"idle": {WorkerID: "idle", State: WorkerActive},
	}}
	provider := newFakeProvider(2)
	provider.state.Instances["i-busy"] = ProviderInstance{ID: "i-busy"}
	provider.state.Instances["i-idle"] = ProviderInstance{ID: "i-idle"}
	controller := newTestController(t, "run", source, acquiredLeaders(), provider, nil)
	if err := controller.Cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(source.drained, []string{"idle"}) {
		t.Fatalf("drained = %v, want exact victim idle", source.drained)
	}
}

func TestControllerRefusesTerminationWhenFreshProofIsUnsafe(t *testing.T) {
	source := &fakeSource{
		snapshot: func() GroupSnapshot {
			snapshot := terminationSnapshot("worker-a")
			snapshot.OldestDrainStartedAt = testNow.Add(-time.Hour)
			return snapshot
		}(),
		proofs: map[string]TerminationProof{"worker-a": {
			WorkerID: "worker-a", ResourceID: "i-a", State: WorkerDraining,
			LocalCleanupComplete: true,
		}},
	}
	provider := newFakeProvider(1)
	provider.state.Instances["i-a"] = ProviderInstance{ID: "i-a", ProtectedFromScaleIn: true}
	controller := newTestController(t, "run", source, acquiredLeaders(), provider, nil)
	err := controller.Cycle(context.Background())
	if !errors.Is(err, ErrUnsafeTermination) {
		t.Fatalf("Cycle error = %v, want unsafe termination refusal", err)
	}
	if provider.mutationCount() != 0 {
		t.Fatalf("unsafe proof made %d provider mutations", provider.mutationCount())
	}
}

func TestControllerAmbiguousUnprotectAndTerminateReconcile(t *testing.T) {
	source := &fakeSource{
		snapshot: terminationSnapshot("worker-a"),
		proofs: map[string]TerminationProof{"worker-a": {
			WorkerID: "worker-a", ResourceID: "i-a", State: WorkerDisabled,
			LocalCleanupComplete: true,
		}},
	}
	provider := newFakeProvider(1)
	provider.state.Instances["i-a"] = ProviderInstance{ID: "i-a", ProtectedFromScaleIn: true}
	provider.ambiguousProtection = true
	provider.ambiguousTerminate = true
	controller := newTestController(t, "run", source, acquiredLeaders(), provider, nil)
	if err := controller.Cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(provider.protectionCalls) != 1 || provider.protectionCalls[0].id != "i-a" || provider.protectionCalls[0].protected {
		t.Fatalf("protection calls = %#v, want exact unprotect", provider.protectionCalls)
	}
	if len(provider.terminationCalls) != 1 || provider.terminationCalls[0].id != "i-a" || !provider.terminationCalls[0].decrement {
		t.Fatalf("termination calls = %#v, want exact decrement", provider.terminationCalls)
	}
	if provider.desired() != 0 || len(source.confirmed) != 1 || source.confirmed[0].Action != ActionFinishTermination {
		t.Fatalf("termination not confirmed: desired=%d confirmed=%#v", provider.desired(), source.confirmed)
	}
}

func TestControllerRetriesProviderTerminationConfirmationAfterMutationSucceeded(t *testing.T) {
	source := &fakeSource{
		snapshot:          terminationSnapshot("worker-a"),
		confirmedFailures: 1,
		proofs: map[string]TerminationProof{"worker-a": {
			WorkerID: "worker-a", ResourceID: "i-a", State: WorkerDisabled,
			LocalCleanupComplete: true,
		}},
	}
	provider := newFakeProvider(1)
	provider.state.Instances["i-a"] = ProviderInstance{ID: "i-a", ProtectedFromScaleIn: true}
	controller := newTestController(t, "run", source, acquiredLeaders(), provider, nil)
	if err := controller.Cycle(context.Background()); err == nil {
		t.Fatal("first cycle succeeded despite confirmation write failure")
	}
	if provider.desired() != 0 || len(provider.terminationCalls) != 1 {
		t.Fatalf("provider mutation did not complete before write failure: desired=%d terminations=%#v", provider.desired(), provider.terminationCalls)
	}
	if err := controller.Cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(provider.terminationCalls) != 1 {
		t.Fatalf("confirmation retry repeated provider termination: %#v", provider.terminationCalls)
	}
	if len(source.confirmed) != 1 || source.confirmed[0].WorkerID != "worker-a" || source.confirmed[0].ResourceID != "i-a" {
		t.Fatalf("confirmation retry = %#v", source.confirmed)
	}
}

func TestControllerTerminatesExactlyFencedLostWorkerAfterAuthorityRecovery(t *testing.T) {
	source := &fakeSource{
		snapshot: GroupSnapshot{
			Inputs: Inputs{TerminationCandidateID: "worker-lost", Workers: []Worker{{
				ID: "worker-lost", State: WorkerLost, FencedForTermination: true,
			}}},
			ResourceIDs: map[string]string{"worker-lost": "i-lost"},
		},
		proofs: map[string]TerminationProof{"worker-lost": {
			WorkerID: "worker-lost", ResourceID: "i-lost", State: WorkerLost,
			FencedForTermination: true,
		}},
	}
	provider := newFakeProvider(1)
	provider.state.Instances["i-lost"] = ProviderInstance{ID: "i-lost", ProtectedFromScaleIn: true}
	controller := newTestController(t, "run", source, acquiredLeaders(), provider, nil)
	if err := controller.Cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if provider.desired() != 0 || len(provider.terminationCalls) != 1 || provider.terminationCalls[0].id != "i-lost" {
		t.Fatalf("desired=%d terminations=%#v", provider.desired(), provider.terminationCalls)
	}
}

func TestControllerPersistsCooldownIntentBeforeAmbiguousProviderMutation(t *testing.T) {
	source := &fakeSource{snapshot: launchSnapshot()}
	provider := newFakeProvider(0)
	provider.desiredErr = errors.New("access denied")
	controller := newTestController(t, "run", source, acquiredLeaders(), provider, nil)
	if err := controller.Cycle(context.Background()); err == nil {
		t.Fatal("Cycle succeeded, want mutation failure")
	}
	if len(source.confirmed) != 0 {
		t.Fatalf("failed provider effect was confirmed: %#v", source.confirmed)
	}
	if len(source.intents) != 1 || source.intents[0].Action != ActionLaunch || source.intents[0].Desired != 1 {
		t.Fatalf("mutation intents = %#v, want durable launch target", source.intents)
	}
}

func TestControllerCountsProviderPendingAcrossPreEnrollmentCycles(t *testing.T) {
	policy := testPolicy()
	policy.MaxScaleOutPerCycle = 2
	planner := mustPlanner(t, policy)
	source := &fakeSource{snapshot: GroupSnapshot{Inputs: Inputs{Demand: Demand{
		Queued: []WorkloadBucket{workload(Capacity{MilliCPU: 2_000, MemoryBytes: 4_000, ScratchBytes: 10_000, VMSlots: 2, BuildExecutors: 1}, 3)},
	}}}}
	provider := newFakeProvider(0)
	controller, err := NewController(ControllerConfig{
		GroupID: "run", Interval: time.Second, InitialBackoff: time.Second, MaxBackoff: time.Second,
	}, planner, source, acquiredLeaders(), provider, nil, fixedClock{testNow}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := controller.Cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := controller.Cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := controller.Cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(provider.desiredTargets, []int{2, 3}) || provider.desired() != 3 {
		t.Fatalf("desired targets = %v desired=%d, want bounded pre-enrollment 2 then 3", provider.desiredTargets, provider.desired())
	}
}

func TestControllerAmbiguousLaunchDoesNotAmplifyNextCycle(t *testing.T) {
	source := &fakeSource{snapshot: launchSnapshot()}
	provider := newFakeProvider(0)
	provider.ambiguousDesired = true
	controller := newTestController(t, "run", source, acquiredLeaders(), provider, nil)
	if err := controller.Cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	provider.ambiguousDesired = false
	if err := controller.Cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(provider.desiredTargets, []int{1}) {
		t.Fatalf("response-loss retry amplified desired targets: %v", provider.desiredTargets)
	}
}

func TestMetricsDeadlineCannotBlockProviderAction(t *testing.T) {
	source := &fakeSource{snapshot: launchSnapshot()}
	provider := newFakeProvider(0)
	controller, err := NewController(ControllerConfig{
		GroupID: "run", Interval: time.Second, InitialBackoff: time.Second,
		MaxBackoff: time.Second, MetricsTimeout: 10 * time.Millisecond,
	}, mustPlanner(t, testPolicy()), source, acquiredLeaders(), provider, blockingMetrics{}, fixedClock{testNow}, nil)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if err := controller.Cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed > 200*time.Millisecond {
		t.Fatalf("metric publisher blocked cycle for %s", elapsed)
	}
	if provider.desired() != 1 {
		t.Fatalf("metric timeout prevented scaling, desired=%d", provider.desired())
	}
}

func TestMetricsProjectDemandCapsSupplyAndDrainAge(t *testing.T) {
	policy := testPolicy()
	policy.MaxWorkers = 1
	policy.MaxScaleOutPerCycle = 1
	planner := mustPlanner(t, policy)
	source := &fakeSource{snapshot: GroupSnapshot{
		Inputs: Inputs{UncertifiedRunLaunchAttestations: 2, Demand: Demand{Queued: []WorkloadBucket{
			workload(Capacity{MilliCPU: 2_000, MemoryBytes: 4_000, ScratchBytes: 10_000, VMSlots: 2, BuildExecutors: 1}, 3),
		}}},
	}}
	metrics := &fakeMetrics{}
	controller, err := NewController(ControllerConfig{
		GroupID: "run", Interval: time.Second, InitialBackoff: time.Second, MaxBackoff: time.Second,
		DrainTimeout: time.Minute,
	}, planner, source, acquiredLeaders(), newFakeProvider(0), metrics, fixedClock{testNow}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := controller.Cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := metrics.values[0]
	if got.UncappedRequired != 3 || got.UnmetDeficit != 3 || got.CapReason != CapMaximum || got.Desired != 1 || got.Supply != 0 || got.Pending != 0 || got.Billable != 0 || got.UncertifiedRunLaunchAttestations != 2 || !got.BootstrapPending || got.Action != ActionLaunch || got.DrainAge != 0 || got.DrainTimedOut || got.Outcome != OutcomePlanned {
		t.Fatalf("metric projection = %#v", got)
	}
}

func TestPresentDrainTimeoutIsObservableButNeverTerminatesWithoutProof(t *testing.T) {
	source := &fakeSource{snapshot: GroupSnapshot{
		Inputs:               Inputs{Workers: []Worker{{ID: "draining", State: WorkerDraining}}},
		ResourceIDs:          map[string]string{"draining": "i-draining"},
		OldestDrainStartedAt: testNow.Add(-5 * time.Minute),
	}}
	provider := newFakeProvider(1)
	provider.state.Instances["i-draining"] = ProviderInstance{ID: "i-draining", ProtectedFromScaleIn: true}
	metrics := &fakeMetrics{}
	controller, err := NewController(ControllerConfig{
		GroupID: "run", Interval: time.Second, InitialBackoff: time.Second, MaxBackoff: time.Second,
		DrainTimeout: time.Minute,
	}, mustPlanner(t, testPolicy()), source, acquiredLeaders(), provider, metrics, fixedClock{testNow}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := controller.Cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if provider.mutationCount() != 0 || len(metrics.values) != 1 || !metrics.values[0].DrainTimedOut || metrics.values[0].Outcome != OutcomeRetrying {
		t.Fatalf("provider mutations=%d metrics=%#v", provider.mutationCount(), metrics.values)
	}
}

func TestAbsentDisabledWorkerReconcilesReplacementAndConfirmsTermination(t *testing.T) {
	source := &fakeSource{
		snapshot: terminationSnapshot("worker-a"),
		proofs: map[string]TerminationProof{"worker-a": {
			WorkerID: "worker-a", ResourceID: "i-a", State: WorkerDisabled, LocalCleanupComplete: true,
		}},
	}
	provider := newFakeProvider(1) // victim is absent, but desired replacement remains.
	metrics := &fakeMetrics{}
	controller := newTestController(t, "run", source, acquiredLeaders(), provider, metrics)
	if err := controller.Cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(source.confirmed) != 1 || source.confirmed[0].WorkerID != "worker-a" || !reflect.DeepEqual(provider.desiredTargets, []int{0}) {
		t.Fatalf("absent termination reconciliation: confirmed=%#v desired_targets=%v", source.confirmed, provider.desiredTargets)
	}
	if len(metrics.values) != 2 || metrics.values[1].Outcome != OutcomeConfirmed {
		t.Fatalf("absent termination metrics: %#v", metrics.values)
	}
}

func TestTerminatingInstanceWaitsWithoutRepeatingTermination(t *testing.T) {
	source := &fakeSource{
		snapshot: terminationSnapshot("worker-a"),
		proofs: map[string]TerminationProof{"worker-a": {
			WorkerID: "worker-a", ResourceID: "i-a", State: WorkerDisabled, LocalCleanupComplete: true,
		}},
	}
	provider := newFakeProvider(0)
	provider.state.Instances["i-a"] = ProviderInstance{ID: "i-a", Lifecycle: "Terminating:Wait"}
	metrics := &fakeMetrics{}
	controller := newTestController(t, "run", source, acquiredLeaders(), provider, metrics)
	if err := controller.Cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(provider.terminationCalls) != 0 || len(source.confirmed) != 0 || len(metrics.values) != 1 || metrics.values[0].Outcome != OutcomePlanned {
		t.Fatalf("termination calls=%v confirmed=%v metrics=%v", provider.terminationCalls, source.confirmed, metrics.values)
	}
}

func TestProviderAbsentActiveWorkerDoesNotSupplyCapacity(t *testing.T) {
	source := &fakeSource{snapshot: GroupSnapshot{Inputs: Inputs{
		Demand:  Demand{Queued: []WorkloadBucket{workload(Capacity{MilliCPU: 1}, 1)}},
		Workers: []Worker{{ID: "stale", State: WorkerActive, ActivatedAt: testNow.Add(-time.Hour)}},
	}, ResourceIDs: map[string]string{"stale": "i-gone"}}}
	provider := newFakeProvider(0)
	controller := newTestController(t, "run", source, acquiredLeaders(), provider, nil)
	if err := controller.Cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if provider.desired() != 1 {
		t.Fatalf("provider desired=%d, stale active DB row was counted as supply", provider.desired())
	}
}

func TestIndependentControllersIsolateFailures(t *testing.T) {
	runProvider := newFakeProvider(0)
	buildProvider := newFakeProvider(0)
	run := newTestController(t, "run", &fakeSource{snapshotErr: errors.New("run query failed")}, acquiredLeaders(), runProvider, nil)
	buildSource := &fakeSource{snapshot: launchSnapshot()}
	build := newTestController(t, "build", buildSource, acquiredLeaders(), buildProvider, nil)
	if err := run.Cycle(context.Background()); err == nil {
		t.Fatal("run controller succeeded, want isolated failure")
	}
	if err := build.Cycle(context.Background()); err != nil {
		t.Fatalf("build controller was affected by run failure: %v", err)
	}
	if runProvider.callCount() != 0 || buildProvider.desired() != 1 {
		t.Fatalf("providers run calls=%d build desired=%d", runProvider.callCount(), buildProvider.desired())
	}
}

func TestRunRetriesWithBoundedBackoff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	sleeper := &recordingSleeper{cancel: cancel, cancelAfter: 3}
	source := &fakeSource{snapshot: launchSnapshot(), snapshotFailures: 2}
	controller := newTestControllerWithSleeper(t, source, newFakeProvider(0), sleeper)
	err := controller.Run(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error = %v, want canceled", err)
	}
	want := []time.Duration{time.Second, 2 * time.Second, 10 * time.Second}
	if !reflect.DeepEqual(sleeper.delays, want) {
		t.Fatalf("retry delays = %v, want %v", sleeper.delays, want)
	}
}

func newTestController(t *testing.T, groupID string, source SnapshotSource, leaders LeaderElector, provider Provider, metrics MetricsPublisher) *Controller {
	t.Helper()
	controller, err := NewController(ControllerConfig{
		GroupID: groupID, Interval: 10 * time.Second, InitialBackoff: time.Second, MaxBackoff: 4 * time.Second,
	}, mustPlanner(t, testPolicy()), source, leaders, provider, metrics, fixedClock{testNow}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return controller
}

func newTestControllerWithSleeper(t *testing.T, source SnapshotSource, provider Provider, sleeper Sleeper) *Controller {
	t.Helper()
	controller, err := NewController(ControllerConfig{
		GroupID: "run", Interval: 10 * time.Second, InitialBackoff: time.Second, MaxBackoff: 4 * time.Second,
	}, mustPlanner(t, testPolicy()), source, acquiredLeaders(), provider, nil, fixedClock{testNow}, sleeper)
	if err != nil {
		t.Fatal(err)
	}
	return controller
}

func launchSnapshot() GroupSnapshot {
	return GroupSnapshot{Inputs: Inputs{Demand: Demand{Queued: []WorkloadBucket{workload(Capacity{MilliCPU: 1}, 1)}}}}
}

func terminationSnapshot(workerID string) GroupSnapshot {
	return GroupSnapshot{Inputs: Inputs{
		Workers:                []Worker{{ID: workerID, State: WorkerDisabled, LocalCleanupComplete: true}},
		TerminationCandidateID: workerID,
	}, ResourceIDs: map[string]string{workerID: "i-a"}}
}

type fixedClock struct{ now time.Time }

func (clock fixedClock) Now() time.Time { return clock.now }

type fakeLease struct{}

func (fakeLease) Release() error { return nil }

type fakeLeaders struct {
	acquired bool
	err      error
}

func acquiredLeaders() *fakeLeaders { return &fakeLeaders{acquired: true} }

func (leader *fakeLeaders) TryAcquire(context.Context, string) (LeaderLease, bool, error) {
	if leader.err != nil {
		return nil, false, leader.err
	}
	if !leader.acquired {
		return nil, false, nil
	}
	return fakeLease{}, true, nil
}

type fakeSource struct {
	mu                sync.Mutex
	snapshot          GroupSnapshot
	snapshotErr       error
	snapshotFailures  int
	snapshotCalls     int
	proofs            map[string]TerminationProof
	drained           []string
	intents           []ConfirmedAction
	confirmed         []ConfirmedAction
	confirmedFailures int
}

func (source *fakeSource) Snapshot(context.Context, string) (GroupSnapshot, error) {
	source.mu.Lock()
	defer source.mu.Unlock()
	source.snapshotCalls++
	if source.snapshotFailures > 0 {
		source.snapshotFailures--
		return GroupSnapshot{}, errors.New("temporary query failure")
	}
	return source.snapshot, source.snapshotErr
}

func (source *fakeSource) MarkDraining(_ context.Context, _ string, workerID string) error {
	source.mu.Lock()
	defer source.mu.Unlock()
	source.drained = append(source.drained, workerID)
	proof := source.proofs[workerID]
	proof.WorkerID = workerID
	proof.State = WorkerDraining
	if source.proofs == nil {
		source.proofs = make(map[string]TerminationProof)
	}
	source.proofs[workerID] = proof
	return nil
}

func (source *fakeSource) TerminationProof(_ context.Context, _ string, workerID string) (TerminationProof, error) {
	source.mu.Lock()
	defer source.mu.Unlock()
	proof, exists := source.proofs[workerID]
	if !exists {
		return TerminationProof{}, errors.New("proof not found")
	}
	return proof, nil
}

func (source *fakeSource) ClaimTermination(ctx context.Context, groupID, workerID string) (TerminationProof, error) {
	return source.TerminationProof(ctx, groupID, workerID)
}

func (source *fakeSource) RecordConfirmed(_ context.Context, _ string, action ConfirmedAction) error {
	source.mu.Lock()
	defer source.mu.Unlock()
	if source.confirmedFailures > 0 {
		source.confirmedFailures--
		return errors.New("temporary confirmation write failure")
	}
	source.confirmed = append(source.confirmed, action)
	return nil
}

func (source *fakeSource) RecordMutationIntent(_ context.Context, _ string, action ConfirmedAction) error {
	source.mu.Lock()
	defer source.mu.Unlock()
	source.intents = append(source.intents, action)
	return nil
}

type protectionCall struct {
	id        string
	protected bool
}

type terminationCall struct {
	id        string
	decrement bool
}

type fakeProvider struct {
	mu                  sync.Mutex
	state               ProviderState
	describeCalls       int
	desiredTargets      []int
	protectionCalls     []protectionCall
	terminationCalls    []terminationCall
	desiredErr          error
	describeErr         error
	ambiguousDesired    bool
	ambiguousProtection bool
	ambiguousTerminate  bool
}

func newFakeProvider(desired int) *fakeProvider {
	return &fakeProvider{state: ProviderState{Desired: desired, Instances: make(map[string]ProviderInstance)}}
}

func (provider *fakeProvider) Describe(context.Context, string) (ProviderState, error) {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	provider.describeCalls++
	if provider.describeErr != nil {
		return ProviderState{}, provider.describeErr
	}
	instances := make(map[string]ProviderInstance, len(provider.state.Instances))
	maps.Copy(instances, provider.state.Instances)
	return ProviderState{Desired: provider.state.Desired, Instances: instances}, nil
}

func (provider *fakeProvider) SetDesired(_ context.Context, _ string, desired int) error {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	provider.desiredTargets = append(provider.desiredTargets, desired)
	if provider.desiredErr != nil {
		return provider.desiredErr
	}
	provider.state.Desired = desired
	if provider.ambiguousDesired {
		return errors.New("response lost")
	}
	return nil
}

func (provider *fakeProvider) SetScaleInProtection(_ context.Context, _, id string, protected bool) error {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	provider.protectionCalls = append(provider.protectionCalls, protectionCall{id: id, protected: protected})
	instance := provider.state.Instances[id]
	instance.ProtectedFromScaleIn = protected
	provider.state.Instances[id] = instance
	if provider.ambiguousProtection {
		return errors.New("response lost")
	}
	return nil
}

func (provider *fakeProvider) Terminate(_ context.Context, _ string, id string, decrement bool) error {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	provider.terminationCalls = append(provider.terminationCalls, terminationCall{id: id, decrement: decrement})
	delete(provider.state.Instances, id)
	if decrement && provider.state.Desired > 0 {
		provider.state.Desired--
	}
	if provider.ambiguousTerminate {
		return errors.New("response lost")
	}
	return nil
}

func (provider *fakeProvider) desired() int {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	return provider.state.Desired
}

func (provider *fakeProvider) callCount() int {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	return provider.describeCalls + len(provider.desiredTargets) + len(provider.protectionCalls) + len(provider.terminationCalls)
}

func (provider *fakeProvider) mutationCount() int {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	return len(provider.desiredTargets) + len(provider.protectionCalls) + len(provider.terminationCalls)
}

type fakeMetrics struct {
	err    error
	values []FleetMetrics
}

type blockingMetrics struct{}

func (blockingMetrics) Publish(ctx context.Context, _ FleetMetrics) error {
	<-ctx.Done()
	return ctx.Err()
}

func (metrics *fakeMetrics) Publish(_ context.Context, value FleetMetrics) error {
	metrics.values = append(metrics.values, value)
	return metrics.err
}

type recordingSleeper struct {
	delays      []time.Duration
	cancel      context.CancelFunc
	cancelAfter int
}

func (sleeper *recordingSleeper) Sleep(ctx context.Context, delay time.Duration) error {
	sleeper.delays = append(sleeper.delays, delay)
	if len(sleeper.delays) >= sleeper.cancelAfter {
		sleeper.cancel()
		return ctx.Err()
	}
	return nil
}
