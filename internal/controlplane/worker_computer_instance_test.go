package controlplane

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func TestRuntimeReconcileTargetProjectsActionAndComputerAuthority(t *testing.T) {
	runtimeID := pgvalue.UUID(uuid.NewV7())
	baseComputerDiskVersionID := pgvalue.UUID(uuid.NewV7())
	for _, test := range []struct {
		name       string
		action     computer.ReconcileAction
		wantAction string
		wantTarget bool
	}{
		{name: "prepare", action: computer.ReconcilePrepare, wantAction: workerapi.RuntimeReconcilePrepare, wantTarget: true},
		{name: "close", action: computer.ReconcileClose, wantAction: workerapi.RuntimeReconcileClose},
		{name: "reclaim", action: computer.ReconcileReclaim, wantAction: workerapi.RuntimeReconcileReclaim},
	} {
		t.Run(test.name, func(t *testing.T) {
			row := initializingComputerSourceRow(t)
			row.ID, row.WorkerEpoch = runtimeID, 7
			row.PreparationDiskVersionID = baseComputerDiskVersionID
			item, err := runtimeReconcileTarget(t.Context(), nil, computer.ReconcileTarget{Instance: row, Action: test.action})
			if err != nil {
				t.Fatal(err)
			}
			if item.ID != pgvalue.UUIDString(runtimeID) || item.WorkerEpoch != 7 || item.Action != test.wantAction || (item.Source.Computer != nil) != test.wantTarget || item.Capture != nil {
				t.Fatalf("item = %#v", item)
			}
		})
	}
}

func TestRuntimeReconcileTargetProjectsEveryAction(t *testing.T) {
	for _, action := range []computer.ReconcileAction{computer.ReconcilePrepare, computer.ReconcileCapture, computer.ReconcileClose, computer.ReconcileReclaim} {
		if runtimeReconcileActions[action] == "" {
			t.Fatalf("action %d has no worker projection", action)
		}
	}
}

func TestRuntimeReconcileTargetDoesNotReplayOpenedCheckpoint(t *testing.T) {
	row := committedComputerSourceRow(t)
	row.ID = pgvalue.UUID(uuid.NewV7())
	row.WorkerEpoch = 7
	row.DesiredState = "ready"
	row.ObservedState = "ready"
	row.AdmissionState = "open"
	row.DesiredVersion = 2
	row.ObservedDesiredVersion = 1
	row.SourceCheckpointID = pgvalue.UUID(uuid.NewV7())
	row.SourceDiskVersionID = row.PreparationDiskVersionID
	item, err := runtimeReconcileTarget(t.Context(), nil, computer.ReconcileTarget{Instance: row, Action: computer.ReconcilePrepare})
	if err != nil {
		t.Fatal(err)
	}
	if item.Source.Restore != nil || item.Source.Computer == nil {
		t.Fatalf("checkpoint provenance triggered restore: %+v", item)
	}
}

func TestComputerInstanceResponsePreservesActualCPUShape(t *testing.T) {
	row := db.ComputerInstance{
		VMVCPUCount:     3,
		CPUConfigDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	}
	response := computerInstanceResponse(row)
	if response.VMVCPUCount != row.VMVCPUCount || response.CPUConfigDigest != row.CPUConfigDigest {
		t.Fatalf("response CPU shape = %d/%q, want %d/%q", response.VMVCPUCount, response.CPUConfigDigest, row.VMVCPUCount, row.CPUConfigDigest)
	}
}

// The owner's cleanup proof is the stored reclaim evidence: it must encode
// exactly as the worker contract's proof, with the same method vocabulary.
func TestComputerCleanupProofEncodesAsWorkerProof(t *testing.T) {
	if computer.CleanupSessionClosed != workerapi.RuntimeCleanupSessionClosed || computer.CleanupHostReconciled != workerapi.RuntimeCleanupHostReconciled || computer.CleanupNotMaterialized != workerapi.RuntimeCleanupNotMaterialized {
		t.Fatal("cleanup proof methods differ from the worker contract")
	}
	proof := workerapi.RuntimeCleanupProof{Method: workerapi.RuntimeCleanupHostReconciled, CompletedAt: time.Date(2026, time.September, 30, 12, 0, 0, 123, time.UTC)}
	worker, err := json.Marshal(proof)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := json.Marshal(computerCleanupProof(&proof))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(worker, owner) {
		t.Fatalf("owner proof %s differs from worker proof %s", owner, worker)
	}
	if computerCleanupProof(nil) != nil {
		t.Fatal("absent proof became a proof")
	}
}
