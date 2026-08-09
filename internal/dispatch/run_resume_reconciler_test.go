package dispatch

import (
	"context"
	"testing"

	"github.com/helmrdotdev/helmr/internal/db"
)

type recordingRunResumeRecoverer struct {
	limit int32
}

func (r *recordingRunResumeRecoverer) RecoverExpiredRunResumes(_ context.Context, limit int32) ([]db.RecoverExpiredRunResumesRow, error) {
	r.limit = limit
	return nil, nil
}

type recordingRunResumeLock struct {
	guard *recordingRunResumeLockGuard
}

func (l recordingRunResumeLock) TryLock(context.Context) (RunResumeRecoveryLockGuard, bool, error) {
	return l.guard, true, nil
}

type recordingRunResumeLockGuard struct {
	unlocked bool
}

func (g *recordingRunResumeLockGuard) Unlock(context.Context) error {
	g.unlocked = true
	return nil
}

func TestRunResumeReconcilerRecoversExpiredResumesUnderLock(t *testing.T) {
	recoverer := &recordingRunResumeRecoverer{}
	guard := &recordingRunResumeLockGuard{}
	reconciler, err := NewRunResumeReconciler(recoverer, recordingRunResumeLock{guard: guard}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := reconciler.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if recoverer.limit != defaultRunResumeRecoveryLimit {
		t.Fatalf("recovery limit = %d, want %d", recoverer.limit, defaultRunResumeRecoveryLimit)
	}
	if !guard.unlocked {
		t.Fatal("run resume recovery lock was not released")
	}
}
