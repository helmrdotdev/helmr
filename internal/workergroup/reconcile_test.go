package workergroup

import (
	"context"
	"testing"

	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
)

type recordingReconcileStore struct {
	calls      []string
	reconciled []db.ReconcileWorkerGroupParams
}

func (s *recordingReconcileStore) LockWorkerGroupsForReconciliation(_ context.Context, arg db.LockWorkerGroupsForReconciliationParams) ([]string, error) {
	s.calls = append(s.calls, "lock-present")
	return arg.DesiredIds, nil
}

func (s *recordingReconcileStore) ReconcileWorkerGroup(_ context.Context, arg db.ReconcileWorkerGroupParams) (db.ReconcileWorkerGroupRow, error) {
	s.calls = append(s.calls, "reconcile")
	s.reconciled = append(s.reconciled, arg)
	return db.ReconcileWorkerGroupRow{}, nil
}

func (s *recordingReconcileStore) LockAbsentWorkerGroups(context.Context, db.LockAbsentWorkerGroupsParams) ([]string, error) {
	s.calls = append(s.calls, "lock-absent")
	return nil, nil
}

func (s *recordingReconcileStore) DrainAbsentWorkerGroups(context.Context, db.DrainAbsentWorkerGroupsParams) ([]db.DrainAbsentWorkerGroupsRow, error) {
	s.calls = append(s.calls, "drain-absent")
	return nil, nil
}

func TestReconcileProjectsDesiredGroupAfterLock(t *testing.T) {
	store := &recordingReconcileStore{}
	err := Reconcile(context.Background(), store, "us-east-1", []Desired{{
		Spec:                  Spec{ID: "run-workers", Name: "Run workers", AllowsRun: true},
		Capacity:              Capacity{MilliCPU: 1000, MemoryBytes: 1024, GuestEphemeralDiskBytes: 1024, VMSlots: 1},
		ObservationTTLSeconds: 120,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if got := store.calls; len(got) != 4 || got[0] != "lock-present" || got[1] != "reconcile" || got[2] != "lock-absent" || got[3] != "drain-absent" {
		t.Fatalf("calls = %#v", got)
	}
	if len(store.reconciled) != 1 {
		t.Fatalf("reconciled = %#v", store.reconciled)
	}
	group := store.reconciled[0]
	if group.ID != "run-workers" || group.RegionID != "us-east-1" || group.ProtocolVersion != auth.WorkerProtocolVersion || group.RequiredCpuMillis != 1000 || group.RequiredVmSlots != 1 {
		t.Fatalf("reconciled group = %#v", group)
	}
}

func TestReconcileRejectsDuplicateGroupIDsBeforeStoreAccess(t *testing.T) {
	store := &recordingReconcileStore{}
	err := Reconcile(context.Background(), store, "us-east-1", []Desired{
		{Spec: Spec{ID: "duplicate", AllowsRun: true}, ObservationTTLSeconds: 120},
		{Spec: Spec{ID: " duplicate ", AllowsBuild: true}, ObservationTTLSeconds: 120},
	})
	if err == nil {
		t.Fatal("Reconcile() accepted duplicate worker groups")
	}
	if len(store.calls) != 0 {
		t.Fatalf("store calls = %#v", store.calls)
	}
}
