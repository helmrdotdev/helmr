package executor

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func runtimeSaveFixture(t *testing.T) (*runtimeComputerSaves, *saveHostFixture, func() (bool, error)) {
	t.Helper()
	owner := &runtimeComputerSaves{}
	f := &saveHostFixture{runtime: uuid.NewV7().String(), computer: uuid.NewV7().String()}
	start := func() (bool, error) {
		return owner.start(t.Context(), f, f, workerapi.ComputerSaveBeginRequest{OrgID: uuid.NewV7().String(), WorkspaceMountID: uuid.NewV7().String()}, f.runtime, f.computer, func(context.Context) (computerSaveCapture, error) { return saveHostCapture{f}, nil })
	}
	return owner, f, start
}

func TestRuntimeComputerSavesCoalescesConcurrentRequests(t *testing.T) {
	owner, f, start := runtimeSaveFixture(t)
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

func TestRuntimeComputerSavesSettlesBeforeNextSequence(t *testing.T) {
	owner, f, start := runtimeSaveFixture(t)
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

type saveCutSession struct {
	fakeGuestSession
	fixture *saveHostFixture
}

func (s saveCutSession) SnapshotLimits() (vm.SnapshotLimits, error) { return vm.SnapshotLimits{}, nil }
func (s saveCutSession) PauseComputer(context.Context) (*vm.ComputerSnapshot, error) {
	_ = s.fixture.step("terminal-cut")
	return &vm.ComputerSnapshot{}, nil
}

func TestManagedMountSettlesSaveBeforeTerminalCut(t *testing.T) {
	f, pending := newSaveHostFixture(t, "ack")
	if err := pending.Wait(t.Context()); err == nil {
		t.Fatal("expected lost acknowledgement")
	}
	session := newManagedWorkspaceMountSession(saveCutSession{fixture: f})
	session.saves.pending = pending
	session.saves.sequence = 1
	borrowed := &borrowedRunSession{parent: session}
	if _, err := borrowed.PauseComputer(t.Context()); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	n := len(f.steps)
	if n < 2 || f.steps[n-2] != "release" || f.steps[n-1] != "terminal-cut" {
		t.Fatalf("cut before settlement: %v", f.steps)
	}
}

func TestManagedMountRefusesCutWhenSaveCannotSettle(t *testing.T) {
	f, pending := newSaveHostFixture(t, "capture")
	if err := pending.Wait(t.Context()); err == nil {
		t.Fatal("expected capture failure")
	}
	session := newManagedWorkspaceMountSession(saveCutSession{fixture: f})
	session.saves.pending = pending
	if _, err := session.PauseComputer(t.Context()); err == nil {
		t.Fatal("cut despite uncertain source")
	}
	for _, step := range f.steps {
		if step == "terminal-cut" {
			t.Fatal("called physical cut")
		}
	}
}

type saveStopSession struct {
	fakeGuestSession
	stop func(context.Context) error
}

func (s saveStopSession) Close(ctx context.Context) error { return s.stop(ctx) }

func TestManagedMountStopsAfterSaveSettlementDeadline(t *testing.T) {
	f, pending := newSaveHostFixture(t, "blocked")
	<-f.blocked
	stopped := false
	physical := saveStopSession{stop: func(ctx context.Context) error {
		if ctx.Err() != nil {
			t.Fatal("physical stop inherited expired settlement deadline")
		}
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("physical stop unbounded")
		}
		stopped = true
		close(f.joined)
		return nil
	}}
	session := newManagedWorkspaceMountSession(physical)
	session.saves.pending = pending
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer cancel()
	if err := session.Close(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("lost settlement error: %v", err)
	}
	if !stopped {
		t.Fatal("source not stopped")
	}
	if err := session.saves.Quiesce(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestManagedMountRetriesPhysicalCloseJoinTimeout(t *testing.T) {
	calls := 0
	session := newManagedWorkspaceMountSession(saveStopSession{stop: func(context.Context) error {
		calls++
		if calls == 1 {
			return context.DeadlineExceeded
		}
		return nil
	}})
	if err := session.Close(t.Context()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	if err := session.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := session.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("physical close calls: %d", calls)
	}
}
