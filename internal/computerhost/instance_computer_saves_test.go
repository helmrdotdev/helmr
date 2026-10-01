package computerhost

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func instanceSaveFixture(t *testing.T) (*instanceComputerSaves, *saveHostFixture, func() (bool, error)) {
	t.Helper()
	owner := &instanceComputerSaves{}
	f := &saveHostFixture{instance: uuid.NewV7().String(), computer: uuid.NewV7().String()}
	start := func() (bool, error) {
		return owner.start(t.Context(), f, f, workerapi.ComputerSaveBeginRequest{EnvironmentID: uuid.NewV7().String(), ComputerInstanceID: f.instance, WriterGeneration: 2}, f.instance, f.computer, func(context.Context) (computerSaveCapture, error) { return saveHostCapture{f}, nil })
	}
	return owner, f, start
}

func TestInstanceComputerSavesCoalescesConcurrentRequests(t *testing.T) {
	owner, f, start := instanceSaveFixture(t)
	f.blocked = make(chan struct{})
	f.joined = make(chan struct{})
	if ok, err := start(); !ok || err != nil {
		t.Fatal(err)
	}
	<-f.blocked
	var callers sync.WaitGroup
	for range 20 {
		callers.Go(func() {
			if ok, err := start(); ok || err != nil {
				t.Errorf("parallel admission %v %v", ok, err)
			}
		})
	}
	callers.Wait()
	if owner.sequence != 1 {
		t.Fatal("allocated duplicate sequence")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer cancel()
	if err := owner.Quiesce(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("join: %v", err)
	}
	if ok, err := start(); ok || err == nil {
		t.Fatal("admitted after quiescence started")
	}
	close(f.joined)
	if err := owner.Quiesce(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestInstanceComputerSavesSettlesBeforeNextSequence(t *testing.T) {
	owner, f, start := instanceSaveFixture(t)
	f.fail = "commit"
	if ok, err := start(); !ok || err != nil {
		t.Fatal(err)
	}
	first := owner.pending
	if err := first.Wait(t.Context()); err == nil {
		t.Fatal("expected ambiguous commit")
	}
	if ok, err := start(); ok || err == nil {
		t.Fatal("replaced uncertain operation")
	}
	if err := owner.settle(t.Context()); err != nil {
		t.Fatal(err)
	}
	if ok, err := start(); !ok || err != nil {
		t.Fatal(err)
	}
	second := owner.pending
	if second.request.Sequence != 2 || second.request.SaveID == first.request.SaveID {
		t.Fatal("invalid successor identity")
	}
	if err := second.Wait(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := owner.Quiesce(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(f.requests) != 3 || f.requests[0] != f.requests[1] {
		t.Fatal("did not replay original commit before successor")
	}
}

type saveCutMachine struct {
	fakeGuestMachine
	fixture *saveHostFixture
}

func (s saveCutMachine) Close(context.Context) error {
	return s.fixture.step("physical-close")
}

func TestManagedMountSettlesSaveBeforePhysicalRelease(t *testing.T) {
	f, pending := newSaveHostFixture(t, "ack")
	if err := pending.Wait(t.Context()); err == nil {
		t.Fatal("expected lost acknowledgement")
	}
	machine := newInstanceMount(saveCutMachine{fixture: f})
	machine.saves.pending = pending
	machine.saves.sequence = 1
	if err := machine.ReleaseCheckpointSource(t.Context()); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	n := len(f.steps)
	if n < 2 || f.steps[n-2] != "release" || f.steps[n-1] != "physical-close" {
		t.Fatalf("physical release before settlement: %v", f.steps)
	}
}

func TestManagedMountReportsUnsettledSaveOnPhysicalRelease(t *testing.T) {
	f, pending := newSaveHostFixture(t, "capture")
	if err := pending.Wait(t.Context()); err == nil {
		t.Fatal("expected capture failure")
	}
	machine := newInstanceMount(saveCutMachine{fixture: f})
	machine.saves.pending = pending
	if err := machine.ReleaseCheckpointSource(t.Context()); err == nil {
		t.Fatal("unsettled save was not reported on physical release")
	}
	closes := 0
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, step := range f.steps {
		if step == "physical-close" {
			closes++
		}
	}
	if closes != 1 {
		t.Fatalf("physical closes = %d, want 1", closes)
	}
}

type saveStopMachine struct {
	fakeGuestMachine
	stop func(context.Context) error
}

func (s saveStopMachine) Close(ctx context.Context) error { return s.stop(ctx) }

// A save producer still running when the release deadline expires keeps the
// machine running; a later release closes it once the producer has finished.
func TestManagedMountDefersPhysicalReleaseUntilSaveOwnerJoins(t *testing.T) {
	f, pending := newSaveHostFixture(t, "blocked")
	<-f.blocked
	stops := 0
	machine := newInstanceMount(saveStopMachine{stop: func(context.Context) error {
		stops++
		return nil
	}})
	machine.saves.pending = pending
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer cancel()
	if err := machine.ReleaseCheckpointSource(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("release before the save owner joined = %v", err)
	}
	if stops != 0 {
		t.Fatal("machine closed while its save producer was still running")
	}
	if released, err := machine.CheckpointReleaseResult(t.Context()); !released || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("recorded release = %v, %v", released, err)
	}
	close(f.joined)
	if err := machine.ReleaseCheckpointSource(t.Context()); err != nil {
		t.Fatal(err)
	}
	if stops != 1 {
		t.Fatalf("physical stops = %d, want 1", stops)
	}
}

// A joined save whose settlement ran out of time still gets a bounded
// physical stop, and the settlement error stays visible.
func TestManagedMountStopsAfterJoinedSaveSettlementDeadline(t *testing.T) {
	_, pending := newSaveHostFixture(t, "capture")
	if err := pending.Wait(t.Context()); err == nil {
		t.Fatal("expected capture failure")
	}
	stopped := false
	machine := newInstanceMount(saveStopMachine{stop: func(ctx context.Context) error {
		if ctx.Err() != nil {
			t.Fatal("physical stop inherited expired settlement deadline")
		}
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("physical stop unbounded")
		}
		stopped = true
		return nil
	}})
	machine.saves.pending = pending
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := machine.Close(ctx); err == nil {
		t.Fatal("lost settlement error")
	}
	if !stopped {
		t.Fatal("joined source not stopped")
	}
}

func TestManagedMountRetriesPhysicalCloseJoinTimeout(t *testing.T) {
	calls := 0
	machine := newInstanceMount(saveStopMachine{stop: func(context.Context) error {
		calls++
		if calls == 1 {
			return context.DeadlineExceeded
		}
		return nil
	}})
	if err := machine.Close(t.Context()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	if err := machine.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := machine.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("physical close calls: %d", calls)
	}
}
