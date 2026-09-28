package dispatch

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type recordingRunLeaseRecoverer struct {
	mu                            sync.Mutex
	executionLimit, instanceLimit int32
	executionErr, instanceErr     error
}

func (r *recordingRunLeaseRecoverer) ReconcileComputerInstances(_ context.Context, limit int32) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.instanceLimit = limit
	return 0, r.instanceErr
}
func (r *recordingRunLeaseRecoverer) RecoverRunExecutionLeases(_ context.Context, limit int32) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.executionLimit = limit
	return 0, r.executionErr
}
func TestRunLeaseReconcilerBoundsBothOwnersAndJoinsErrors(t *testing.T) {
	executionErr, instanceErr := errors.New("execution recovery failed"), errors.New("instance recovery failed")
	recoverer := &recordingRunLeaseRecoverer{executionErr: executionErr, instanceErr: instanceErr}
	guard := &recordingRunLeaseLockGuard{}
	reconciler, err := NewRunLeaseReconciler(recoverer, recordingRunLeaseLock{guard}, nil)
	if err != nil {
		t.Fatal(err)
	}
	reconciler.limit = 101
	err = reconciler.reconcile(t.Context())
	if !errors.Is(err, executionErr) || !errors.Is(err, instanceErr) {
		t.Fatalf("recovery errors: %v", err)
	}
	if recoverer.executionLimit != 51 || recoverer.instanceLimit != 50 {
		t.Fatalf("bounds: %d/%d", recoverer.executionLimit, recoverer.instanceLimit)
	}
	if !guard.unlocked {
		t.Fatal("recovery lock retained")
	}
}

type concurrentLaneRecoverer struct{ instanceStarted chan struct{} }

func (r concurrentLaneRecoverer) RecoverRunExecutionLeases(ctx context.Context, _ int32) (int, error) {
	select {
	case <-r.instanceStarted:
		return 1, nil
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}
func (r concurrentLaneRecoverer) ReconcileComputerInstances(context.Context, int32) (int, error) {
	close(r.instanceStarted)
	return 0, nil
}
func TestSlowExecutionRecoveryDoesNotBlockInstanceRecovery(t *testing.T) {
	reconciler, err := NewRunLeaseReconciler(concurrentLaneRecoverer{make(chan struct{})}, recordingRunLeaseLock{&recordingRunLeaseLockGuard{}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := reconciler.reconcile(ctx); err != nil {
		t.Fatal(err)
	}
}

type recordingRunLeaseLock struct{ guard *recordingRunLeaseLockGuard }

func (l recordingRunLeaseLock) TryLock(context.Context) (RunLeaseRecoveryLockGuard, bool, error) {
	return l.guard, true, nil
}

type recordingRunLeaseLockGuard struct{ unlocked bool }

func (g *recordingRunLeaseLockGuard) Unlock(context.Context) error { g.unlocked = true; return nil }
