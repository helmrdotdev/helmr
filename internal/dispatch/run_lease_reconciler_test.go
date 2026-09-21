package dispatch

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/helmrdotdev/helmr/internal/db"
)

type recordingRunLeaseRecoverer struct {
	mu           sync.Mutex
	freshLimit   int32
	resumeLimit  int32
	runtimeLimit int32
	runtimeErr   error
	freshCount   int
	freshErr     error
	resumeErr    error
}

func (r *recordingRunLeaseRecoverer) RecoverExpiredRuntimeReservations(_ context.Context, limit int32) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.runtimeLimit = limit
	return 0, r.runtimeErr
}

func (r *recordingRunLeaseRecoverer) RecoverRunExecutionLeases(_ context.Context, limit int32) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.freshLimit = limit
	return r.freshCount, r.freshErr
}

func (r *recordingRunLeaseRecoverer) RecoverExpiredRunResumes(_ context.Context, limit int32) ([]db.RecoverExpiredRunResumesRow, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.resumeLimit = limit
	return nil, r.resumeErr
}

func TestRunLeaseReconcilerDoesNotLetOneLaneStarveTheOther(t *testing.T) {
	freshErr := errors.New("fresh recovery failed")
	resumeErr := errors.New("resume recovery failed")
	runtimeErr := errors.New("runtime recovery failed")
	recoverer := &recordingRunLeaseRecoverer{
		freshCount: 1, freshErr: freshErr, resumeErr: resumeErr, runtimeErr: runtimeErr,
	}
	reconciler, err := NewRunLeaseReconciler(
		recoverer,
		recordingRunLeaseLock{guard: &recordingRunLeaseLockGuard{}},
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	err = reconciler.reconcile(context.Background())
	if !errors.Is(err, freshErr) || !errors.Is(err, resumeErr) || !errors.Is(err, runtimeErr) {
		t.Fatalf("reconcile error = %v, want all three lane errors", err)
	}
	if recoverer.freshLimit != 34 || recoverer.resumeLimit != 33 || recoverer.runtimeLimit != 33 {
		t.Fatalf("recovery limits fresh=%d resume=%d runtime=%d, want 34/33/33", recoverer.freshLimit, recoverer.resumeLimit, recoverer.runtimeLimit)
	}
}

func TestRunLeaseReconcilerReservesCapacityForResumeBacklog(t *testing.T) {
	recoverer := &recordingRunLeaseRecoverer{freshCount: 100}
	reconciler, err := NewRunLeaseReconciler(
		recoverer,
		recordingRunLeaseLock{guard: &recordingRunLeaseLockGuard{}},
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	reconciler.limit = 100
	if err := reconciler.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if recoverer.freshLimit != 34 || recoverer.resumeLimit != 33 || recoverer.runtimeLimit != 33 {
		t.Fatalf("recovery limits fresh=%d resume=%d runtime=%d, want 34/33/33", recoverer.freshLimit, recoverer.resumeLimit, recoverer.runtimeLimit)
	}
}

type concurrentLaneRecoverer struct {
	resumeStarted chan struct{}
}

func (r concurrentLaneRecoverer) RecoverRunExecutionLeases(ctx context.Context, _ int32) (int, error) {
	select {
	case <-r.resumeStarted:
		return 1, nil
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

func (r concurrentLaneRecoverer) RecoverExpiredRuntimeReservations(context.Context, int32) (int, error) {
	return 0, nil
}

func (r concurrentLaneRecoverer) RecoverExpiredRunResumes(context.Context, int32) ([]db.RecoverExpiredRunResumesRow, error) {
	close(r.resumeStarted)
	return nil, nil
}

func TestRunLeaseReconcilerDoesNotLetSlowFreshRecoveryConsumeResumeTime(t *testing.T) {
	reconciler, err := NewRunLeaseReconciler(
		concurrentLaneRecoverer{resumeStarted: make(chan struct{})},
		recordingRunLeaseLock{guard: &recordingRunLeaseLockGuard{}},
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := reconciler.reconcile(ctx); err != nil {
		t.Fatal(err)
	}
}

type recordingRunLeaseLock struct {
	guard *recordingRunLeaseLockGuard
}

func (l recordingRunLeaseLock) TryLock(context.Context) (RunLeaseRecoveryLockGuard, bool, error) {
	return l.guard, true, nil
}

type recordingRunLeaseLockGuard struct {
	unlocked bool
}

func (g *recordingRunLeaseLockGuard) Unlock(context.Context) error {
	g.unlocked = true
	return nil
}

func TestRunLeaseReconcilerRecoversFreshAndResumeLanesUnderOneLock(t *testing.T) {
	recoverer := &recordingRunLeaseRecoverer{freshCount: 17}
	guard := &recordingRunLeaseLockGuard{}
	reconciler, err := NewRunLeaseReconciler(recoverer, recordingRunLeaseLock{guard: guard}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := reconciler.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if recoverer.freshLimit != (defaultRunLeaseRecoveryLimit+2)/3 {
		t.Fatalf("fresh recovery limit = %d, want %d", recoverer.freshLimit, (defaultRunLeaseRecoveryLimit+2)/3)
	}
	if recoverer.resumeLimit != defaultRunLeaseRecoveryLimit/3 {
		t.Fatalf("resume recovery limit = %d, want %d", recoverer.resumeLimit, defaultRunLeaseRecoveryLimit/3)
	}
	if !guard.unlocked {
		t.Fatal("Run lease recovery lock was not released")
	}
}
