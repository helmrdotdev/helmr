package controlplane

import (
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/jackc/pgx/v5/pgxpool"
	"strings"
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
	object, err := server.cas.Put(t.Context(), computer.DiskMediaType, strings.NewReader("opaque terminal disk fixture:"+capture.Receipt.OperationID+":"+marker))
	if err != nil {
		t.Fatal(err)
	}
	var logicalBytes int64
	if err := pool.QueryRow(t.Context(), `SELECT reserved_guest_ephemeral_disk_bytes FROM runtime_instances WHERE id=$1`, uuid.MustParse(capture.Receipt.Fence.RuntimeInstanceID)).Scan(&logicalBytes); err != nil {
		t.Fatal(err)
	}
	capture.Disk = workerapi.CheckpointComputer{ComputerID: capture.Receipt.Fence.WorkspaceID, LogicalBytes: logicalBytes, Artifact: workerapi.CheckpointArtifact{Digest: object.Digest, SizeBytes: object.SizeBytes, MediaType: object.MediaType}}
	if err := server.registerRunFinalization(t.Context(), worker, workerapi.RegisterRunFinalizationRequest{Lease: lease, OperationID: capture.Receipt.OperationID, Disk: capture.Disk}); err != nil {
		t.Fatalf("register finalization disk: %v", err)
	}
}

func (f *actorCheckpointFixture) registerFinalizationDisk(t *testing.T, capture *workerapi.TaskWorkspaceCapture, marker string) {
	t.Helper()
	registerFinalizationTestDisk(t, f.Pool, f.server, f.worker, f.fence(), capture, marker)
}
