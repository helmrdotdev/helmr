package worker

import (
	"context"
	"errors"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
	"uuid"
)

type allocationDiscoveryTest struct {
	mu    sync.Mutex
	items []workerapi.AllocationIdentity
	err   error
}

func (c *allocationDiscoveryTest) ListAllocations(_ context.Context, after *workerapi.AllocationIdentity) (workerapi.AllocationListResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return workerapi.AllocationListResponse{}, c.err
	}
	index := 0
	if after != nil {
		index = len(c.items)
		for i, identity := range c.items {
			if identity == *after {
				index = i + 1
				break
			}
		}
	}
	if index == len(c.items) {
		return workerapi.AllocationListResponse{}, nil
	}
	return workerapi.AllocationListResponse{Allocations: c.items[index : index+1]}, nil
}

type allocationOwnerTest struct {
	calls          atomic.Int64
	done           atomic.Bool
	fail           atomic.Bool
	finishedSignal chan struct{}
}

func (o *allocationOwnerTest) Run(context.Context) error {
	o.calls.Add(1)
	if o.fail.Load() {
		return errors.New("physical cleanup uncertain")
	}
	o.done.Store(true)
	if o.finishedSignal != nil {
		close(o.finishedSignal)
	}
	return nil
}
func (o *allocationOwnerTest) Finished() bool { return o.done.Load() }
func allocationTestIdentity(kind string) workerapi.AllocationIdentity {
	return workerapi.AllocationIdentity{Kind: kind, EnvironmentID: uuid.NewV7().String(), OwnerID: uuid.NewV7().String(), InstanceID: uuid.NewV7().String(), Epoch: 1}
}
func TestAllocationConsumerBoundsCustodyAndDiscoversWhileFull(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client := &allocationDiscoveryTest{items: []workerapi.AllocationIdentity{allocationTestIdentity("computer"), allocationTestIdentity("computer"), allocationTestIdentity("preparation")}}
		owners := []*allocationOwnerTest{{}, {}}
		for _, owner := range owners {
			owner.fail.Store(true)
		}
		created := 0
		consumer, err := NewAllocationConsumer(client, 2, time.Millisecond, func(workerapi.AllocationIdentity, func(context.Context) error) (AllocationOwner, error) {
			owner := owners[created]
			created++
			return owner, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		var jobs sync.WaitGroup
		for range 2 {
			if err := consumer.discoverPage(ctx); err != nil {
				t.Fatal(err)
			}
			w, ok, err := consumer.Claim(ctx, allowConsumerAdmission{})
			if err != nil || !ok {
				t.Fatalf("claim: %v %v", ok, err)
			}
			jobs.Go(func() { _ = w(ctx) })
		}
		synctest.Wait()
		for range 8 {
			if err := consumer.discoverPage(ctx); err != nil {
				t.Fatal(err)
			}
		}
		if len(consumer.entries) != 2 || len(consumer.pending) != 1 {
			t.Fatal("unbounded custody or blocked discovery")
		}
		if _, ok, err := consumer.Claim(ctx, allowConsumerAdmission{}); err != nil || ok || created != 2 {
			t.Fatal("exceeded local owner bound")
		}
		client.mu.Lock()
		client.items = nil
		client.err = errors.New("discovery unavailable")
		client.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
		synctest.Wait()
		for _, owner := range owners {
			if owner.calls.Load() < 2 {
				t.Fatal("cleanup stopped behind discovery")
			}
			owner.fail.Store(false)
		}
		time.Sleep(30 * time.Millisecond)
		synctest.Wait()
		jobs.Wait()
		if len(consumer.entries) != 0 {
			t.Fatal("finished owners retained")
		}
	})
}

func TestAllocationConsumerConcurrentClaimsInstallOwnerOnce(t *testing.T) {
	client := &allocationDiscoveryTest{items: []workerapi.AllocationIdentity{allocationTestIdentity("computer")}}
	var created atomic.Int64
	consumer, _ := NewAllocationConsumer(client, 1, time.Millisecond, func(workerapi.AllocationIdentity, func(context.Context) error) (AllocationOwner, error) {
		created.Add(1)
		return &allocationOwnerTest{}, nil
	})
	if err := consumer.discoverPage(t.Context()); err != nil {
		t.Fatal(err)
	}
	var jobs sync.WaitGroup
	work := make(chan Work, 8)
	for range 8 {
		jobs.Go(func() {
			w, ok, err := consumer.Claim(t.Context(), allowConsumerAdmission{})
			if err != nil {
				t.Error(err)
			}
			if ok {
				work <- w
			}
		})
	}
	jobs.Wait()
	close(work)
	if created.Load() != 1 || len(work) != 1 {
		t.Fatal("duplicate physical owners")
	}
	client.mu.Lock()
	client.items = nil
	client.mu.Unlock()
	for w := range work {
		if err := w(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if len(consumer.entries) != 0 {
		t.Fatal("retained completed custody")
	}
}

type blockedAllocationDiscovery struct {
	identity         workerapi.AllocationIdentity
	block            atomic.Bool
	started, release chan struct{}
}

func (c *blockedAllocationDiscovery) ListAllocations(ctx context.Context, _ *workerapi.AllocationIdentity) (workerapi.AllocationListResponse, error) {
	if c.block.Load() {
		close(c.started)
		select {
		case <-c.release:
		case <-ctx.Done():
			return workerapi.AllocationListResponse{}, ctx.Err()
		}
	}
	return workerapi.AllocationListResponse{Allocations: []workerapi.AllocationIdentity{c.identity}}, nil
}
func TestAllocationConsumerStalePageCannotRecreateFinishedOwner(t *testing.T) {

	client := &blockedAllocationDiscovery{identity: allocationTestIdentity("computer"), started: make(chan struct{}), release: make(chan struct{})}
	owner := &allocationOwnerTest{finishedSignal: make(chan struct{})}
	var created atomic.Int64
	consumer, _ := NewAllocationConsumer(client, 1, time.Millisecond, func(workerapi.AllocationIdentity, func(context.Context) error) (AllocationOwner, error) {
		created.Add(1)
		return owner, nil
	})
	if err := consumer.discoverPage(t.Context()); err != nil {
		t.Fatal(err)
	}
	work, ok, err := consumer.Claim(t.Context(), allowConsumerAdmission{})
	if err != nil || !ok {
		t.Fatal(err)
	}
	consumer.after = nil
	client.block.Store(true)
	pageDone := make(chan error, 1)
	go func() { pageDone <- consumer.discoverPage(t.Context()) }()
	<-client.started
	workDone := make(chan error, 1)
	go func() { workDone <- work(t.Context()) }()
	<-owner.finishedSignal
	if !owner.Finished() {
		t.Fatal("stop ACK did not complete independently")
	}
	close(client.release)
	if err := <-pageDone; err != nil {
		t.Fatal(err)
	}
	if err := <-workDone; err != nil {
		t.Fatal(err)
	}
	if _, ok, err := consumer.Claim(t.Context(), allowConsumerAdmission{}); err != nil || ok || created.Load() != 1 {
		t.Fatal("stale response created second owner")
	}

}

type allocationHealthProbe struct{ unhealthy atomic.Bool }

func (p *allocationHealthProbe) Probe(context.Context) (HostHealth, error) {
	health := healthyHost(time.Now())
	if p.unhealthy.Load() {
		health.KVMHealthy = false
	}
	return health, nil
}

type cleanupAdmissionOwner struct {
	checkRegistry func()
	stopped       func()
	admit         func(context.Context) error
	probe         *allocationHealthProbe
	calls         int
	done          chan struct{}
	finished      atomic.Bool
}

func (o *cleanupAdmissionOwner) Finished() bool { return o.finished.Load() }
func (o *cleanupAdmissionOwner) Run(ctx context.Context) error {
	o.calls++
	if o.checkRegistry != nil {
		o.checkRegistry()
	}
	if o.calls == 1 {
		if err := o.admit(ctx); err != nil {
			return err
		}
		o.probe.unhealthy.Store(true)
		return errors.New("physical close uncertain")
	}
	if o.calls == 2 {
		return errors.New("stop receipt lost")
	}
	if o.stopped != nil {
		o.stopped()
	}
	o.finished.Store(true)
	close(o.done)
	return nil
}

func TestAllocationSupervisorRetriesCleanupWhenHostBecomesUnhealthy(t *testing.T) {
	probe := &allocationHealthProbe{}
	gate, err := NewHardAdmission(HardAdmissionConfig{Probe: probe, DiskFloorBytes: 1, FDHeadroom: 1})
	if err != nil {
		t.Fatal(err)
	}
	owner := &cleanupAdmissionOwner{probe: probe, done: make(chan struct{})}
	var factories atomic.Int64
	client := &allocationDiscoveryTest{items: []workerapi.AllocationIdentity{allocationTestIdentity("computer")}}
	owner.stopped = func() { client.mu.Lock(); client.items = nil; client.mu.Unlock() }
	consumer, err := NewAllocationConsumer(client, 1, time.Millisecond, func(_ workerapi.AllocationIdentity, admit func(context.Context) error) (AllocationOwner, error) {
		factories.Add(1)
		owner.admit = admit
		return owner, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	supervisor, err := New(Config{ControlPlane: &testControlPlane{}, Recover: emptyPhysicalRecovery, AdmissionEvaluator: gate, PollEvery: time.Millisecond, Background: []BackgroundSpec{{Name: "discovery", DrainEligible: true, Run: consumer.RunDiscovery}}, Consumers: []ConsumerSpec{{Name: "allocation", Concurrency: 1, ContinueDuringDrain: true, Consumer: consumer}}})
	if err != nil {
		t.Fatal(err)
	}
	owner.checkRegistry = func() {
		if supervisor.registry.snapshot().Active["allocation"] != 1 {
			t.Error("custody absent from registry between retries")
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- supervisor.Run(ctx) }()
	select {
	case <-owner.done:
	case <-time.After(time.Second):
		t.Fatal("unhealthy host blocked retained cleanup")
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if factories.Load() != 1 || owner.calls != 3 {
		t.Fatal("cleanup replaced its owner or lost receipt retry")
	}
}

func TestAllocationHealthObservationRecoversWithoutDeliveredOwner(t *testing.T) {
	probe := &allocationHealthProbe{}
	probe.unhealthy.Store(true)
	gate, err := NewHardAdmission(HardAdmissionConfig{Probe: probe, DiskFloorBytes: 1, FDHeadroom: 1})
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(Config{ControlPlane: &testControlPlane{}, Recover: emptyPhysicalRecovery, AdmissionEvaluator: gate})
	if err != nil {
		t.Fatal(err)
	}
	denied := s.observation(t.Context(), StatusActive, RecoveryEvidence{})
	if denied.VMPausedReason == "" {
		t.Fatal("unhealthy host did not pause")
	}
	// No owner or claim can refresh the gate after the prior allocation retires.
	probe.unhealthy.Store(false)
	recovered := s.observation(t.Context(), StatusActive, RecoveryEvidence{})
	if recovered.VMPausedReason != "" || recovered.RunPausedReason != "" {
		t.Fatal("stale pause prevented new delivery after health recovery")
	}
}
