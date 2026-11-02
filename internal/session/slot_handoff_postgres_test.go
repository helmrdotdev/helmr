package session_test

import (
	"errors"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/dispatch"
	agentv1 "github.com/helmrdotdev/helmr/internal/proto/agent/v1"
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/helmrdotdev/helmr/internal/session/sessiontest"
)

func TestOneSlotCheckpointHandoffAllocationLineage(t *testing.T) {
	f, allocator, b := freshComputerAllocationFixture(t)
	ctx := t.Context()
	dbtest.MustExec(t, ctx, f.pool, `UPDATE worker_hosts SET max_vm_slots=1,max_vm_starts=1 WHERE id=$1`, f.worker)
	// Finish the fixture image builder so A is the only physical owner.
	var build computer.ImageBuildIdentity
	if err := f.pool.QueryRow(ctx, `SELECT environment_id,id,allocation_id,executor_epoch FROM image_builds WHERE worker_host_id=$1 AND fenced_at IS NULL`, f.worker).Scan(&build.EnvironmentID, &build.ImageBuildID, &build.InstanceID, &build.Epoch); err != nil {
		t.Fatal(err)
	}
	if err := computer.ObserveImageBuildStopped(ctx, f.pool, *f.host(), build); err != nil {
		t.Fatal(err)
	}
	cp := checkpointStorageForFixture(t, f)
	if err := cp.checkpoints.RegisterCheckpoint(ctx, cp.ref, cp.manifest); err != nil {
		t.Fatal(err)
	}
	cp.upload(t)
	if err := cp.publish(t, cp.save, cp.manifest.Disk); err != nil {
		t.Fatal(err)
	}
	if err := cp.checkpoints.CompleteCheckpoint(ctx, cp.ref, cp.manifest); err != nil {
		t.Fatal(err)
	}
	// B can be assigned while A retains its source. Delivery follows the stop receipt.
	grant, err := allocator.AllocateFreshLease(ctx, f.env, b, 1, f.worker)
	if err != nil {
		t.Fatal("B grant:", err)
	}
	// A has no runnable work by the time its resume attempt arrives. B's
	// competing grant must still produce a definitive handoff, not a retry.
	_, err = session.PrepareComputerSourceAbort(ctx, f.limits, f.pool, *f.host(), f.env, cp.manifest.CheckpointID, 1, sessiontest.ChannelCredential, &agentv1.ComputerSessionReceipt{CheckpointId: cp.manifest.CheckpointID.String(), DesiredVersion: cp.manifest.ControlVersion, Frozen: true})
	if !errors.Is(err, computer.ErrRetainedHostCapacity) {
		t.Fatalf("expected definitive denial for A after B was assigned, got %v", err)
	}
	policy := auditpolicy.Policy{RetentionSeconds: 3600}
	if err := dispatch.ObserveLeaseStopped(ctx, policy, f.pool, *f.host(), leaseIdentity(f), uuid.Nil(), uuid.Nil()); err != nil {
		t.Fatal(err)
	}
	bID := computer.LeaseIdentity{EnvironmentID: f.env, ComputerID: b, InstanceID: grant.InstanceID, Epoch: 1}
	if _, err := allocator.DeliverLease(ctx, *f.host(), bID); err != nil {
		t.Fatal(err)
	}
	if err := dispatch.ObserveFreshLeaseReady(ctx, f.pool, *f.host(), bID, grant.Shape, grant.BaseVersion); err != nil {
		t.Fatal(err)
	}
	if err := dispatch.ObserveLeaseStopped(ctx, policy, f.pool, *f.host(), bID, uuid.Nil(), uuid.Nil()); err != nil {
		t.Fatal(err)
	}
	// Restore A on the same host and dispatch a new Turn in its original Session.
	target, err := allocator.AllocateRestoredLease(ctx, f.env, cp.manifest.CheckpointID, 2, f.worker)
	if err != nil {
		t.Fatal("restore A:", err)
	}
	if target.RestoredFrom == nil || *target.RestoredFrom != cp.save || target.BaseVersion != cp.save.String() {
		t.Fatal("A restored from different state")
	}
	lease := computer.LeaseIdentity{EnvironmentID: f.env, ComputerID: f.computer, Epoch: target.Epoch, InstanceID: target.InstanceID}
	delivery, err := allocator.DeliverLease(ctx, *f.host(), lease)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = bindComputerLeaseDiskForTest(ctx, f.pool, *f.host(), f.env, f.computer, target.Epoch, uuid.MustParse(cp.writer.ActiveKey)); err != nil {
		t.Fatal(err)
	}
	restore := computerRestoreFixture{checkpointStorageFixture: cp, host: *f.host(), epoch: target.Epoch, credential: delivery.ChannelCredential}
	installation := restore.prepare(t)
	if installation.BaseComputerDiskVersionId != cp.save.String() || installation.Capture.CheckpointId != cp.manifest.CheckpointID.String() {
		t.Fatal("A received a different checkpoint")
	}
	if err = session.ValidateComputerRestore(ctx, f.pool, *f.host(), f.env, installation, restoreReceipt(installation, false, false)); err != nil {
		t.Fatal(err)
	}
	if err = session.CommitComputerRestore(ctx, f.pool, *f.host(), f.env, installation, restoreReceipt(installation, true, false)); err != nil {
		t.Fatal(err)
	}
	acknowledgeComputerMembers(t, f, *f.host(), installation)
	if err = session.CompleteComputerRestore(ctx, policy, f.pool, *f.host(), f.env, installation, restoreReceipt(installation, true, true)); err != nil {
		t.Fatal(err)
	}
	admitted := f.enqueue(t, "A-after-B")
	dispatched, err := dispatchTurnForTest(ctx, f.pool, restore.execution())
	if err != nil || dispatched.TurnID != admitted.TurnID {
		t.Fatalf("A did not resume its own Session: %v %v", dispatched, err)
	}
}
