package controlplane

import (
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/jackc/pgx/v5/pgxpool"
	"testing"
	"uuid"
)

func finalizationTestCAS(t *testing.T) cas.Store {
	t.Helper()
	store, err := cas.NewFile(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return store
}

// These bytes exercise the Control Plane's opaque object metadata contract.
// Disk encryption and restoration are exercised separately by the host producer.
func registerFinalizationTestDisk(t *testing.T, pool *pgxpool.Pool, server *Server, worker workerActor, lease workerapi.RunLeaseFence, capture *workerapi.TaskWorkspaceCapture, marker string) {
	t.Helper()
	root := retainedTestGeneration(t, pool, server, capture.Receipt.Fence.RuntimeInstanceID, computerPublicationKey("finalization", pgvalue.UUID(uuid.MustParse(lease.ID)), pgvalue.UUID(uuid.MustParse(capture.Receipt.OperationID))))
	capture.Disk = workerapi.CheckpointComputer{ComputerID: capture.Receipt.Fence.WorkspaceID, LogicalBytes: root.LogicalBytes, Root: root}
	if err := server.registerRunFinalization(t.Context(), worker, workerapi.RegisterRunFinalizationRequest{Lease: lease, OperationID: capture.Receipt.OperationID, Disk: capture.Disk}); err != nil {
		t.Fatalf("register finalization disk: %v", err)
	}
}

func (f *actorCheckpointFixture) registerFinalizationDisk(t *testing.T, capture *workerapi.TaskWorkspaceCapture, marker string) {
	t.Helper()
	registerFinalizationTestDisk(t, f.Pool, f.server, f.worker, f.fence(), capture, marker)
}
