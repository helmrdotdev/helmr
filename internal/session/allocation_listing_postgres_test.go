package session_test

import (
	"errors"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/dispatch"

	"github.com/helmrdotdev/helmr/internal/computer"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
)

func TestHostAllocationListingRetainsUndeliveredClosedPreparation(t *testing.T) {
	f, p, a := preparationAllocationFixture(t)
	allocation, err := a.AllocateImageBuild(t.Context(), f.env, p.ID, f.worker)
	if err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE worker_groups SET status='draining' WHERE id=$1;
 UPDATE image_builds SET status='failed',error_code='executor_lost' WHERE environment_id=$2 AND id=$3`, pgx.QueryExecModeSimpleProtocol, f.group, f.env, p.ID)
	records, err := dispatch.ListHostAllocations(t.Context(), f.pool, *f.host(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var found *dispatch.HostAllocation
	for i := range records {
		if records[i].Kind == "preparation" && records[i].OwnerID == p.ID {
			found = &records[i]
		}
	}
	if found == nil || found.InstanceID != allocation.InstanceID || found.Epoch != allocation.Epoch || found.EnvironmentID != f.env {
		t.Fatalf("closed allocation disappeared: %+v", records)
	}
	if _, err := a.DeliverImageBuild(t.Context(), *f.host(), computer.ImageBuildIdentity{EnvironmentID: f.env, ImageBuildID: p.ID, InstanceID: allocation.InstanceID, Epoch: allocation.Epoch}); !errors.Is(err, dispatch.ErrAllocationClosed) {
		t.Fatalf("closed preparation delivered: %v", err)
	}
	var untouched bool
	if err := f.pool.QueryRow(t.Context(), `SELECT delivered_at IS NULL AND executor_expires_at IS NULL AND fenced_at IS NULL FROM image_builds WHERE environment_id=$1 AND id=$2`, f.env, p.ID).Scan(&untouched); err != nil || !untouched {
		t.Fatalf("listing granted authority or released custody: %v %v", untouched, err)
	}
	if err := computer.ObserveImageBuildStopped(t.Context(), f.pool, *f.host(), computer.ImageBuildIdentity{EnvironmentID: found.EnvironmentID, ImageBuildID: found.OwnerID, Epoch: found.Epoch, InstanceID: found.InstanceID}); err != nil {
		t.Fatal(err)
	}
	records, err = dispatch.ListHostAllocations(t.Context(), f.pool, *f.host(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range records {
		if r.Kind == "preparation" && r.OwnerID == p.ID {
			t.Fatal("physically stopped allocation still listed")
		}
	}
}

func TestHostAllocationListingRejectsForeignAndStaleAuthority(t *testing.T) {
	f, _, _ := preparationAllocationFixture(t)
	for _, change := range []func(*workergroup.HostPrincipal){
		func(h *workergroup.HostPrincipal) { h.Epoch++ },
		func(h *workergroup.HostPrincipal) { h.HostClaimVersion++ },
		func(h *workergroup.HostPrincipal) { h.GroupClaimVersion++ },
		func(h *workergroup.HostPrincipal) { h.GroupID = uuid.NewV7() },
		func(h *workergroup.HostPrincipal) { h.HostID = uuid.NewV7() },
	} {
		host := *f.host()
		change(&host)
		records, err := dispatch.ListHostAllocations(t.Context(), f.pool, host, nil)
		if err == nil || len(records) != 0 {
			t.Fatalf("invalid authority exposed records: %+v %v", records, err)
		}
	}
}

func TestHostAllocationListingIncludesLostComputerAndAdvancesCursor(t *testing.T) {
	f, a, computerID := freshComputerAllocationFixture(t)
	allocation, err := a.AllocateFreshLease(t.Context(), f.env, computerID, 1, f.worker)
	if err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computer_leases SET status='lost' WHERE environment_id=$1 AND computer_id=$2 AND epoch=1`, f.env, computerID)
	records, err := dispatch.ListHostAllocations(t.Context(), f.pool, *f.host(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var found *dispatch.HostAllocation
	for i := range records {
		if records[i].Kind == "computer" && records[i].OwnerID == computerID {
			found = &records[i]
		}
	}
	if found == nil || found.InstanceID != allocation.InstanceID || found.Epoch != 1 {
		t.Fatalf("lost undelivered Computer disappeared: %+v", records)
	}
	if _, err := a.DeliverLease(t.Context(), *f.host(), computer.LeaseIdentity{EnvironmentID: f.env, ComputerID: computerID, Epoch: 1, InstanceID: allocation.InstanceID}); !errors.Is(err, dispatch.ErrAllocationClosed) {
		t.Fatalf("lost allocation delivered: %v", err)
	}
	next, err := dispatch.ListHostAllocations(t.Context(), f.pool, *f.host(), &records[len(records)-1])
	if err != nil || len(next) != 0 {
		t.Fatalf("cursor did not advance: %+v %v", next, err)
	}
	if err := dispatch.ObserveLeaseStopped(t.Context(), auditpolicy.Policy{
		RetentionSeconds: 3600,
	}, f.pool, *f.host(), computer.LeaseIdentity{EnvironmentID: found.EnvironmentID, ComputerID: found.OwnerID, Epoch: found.Epoch, InstanceID: found.InstanceID}, uuid.Nil(), uuid.Nil()); err != nil {
		t.Fatal(err)
	}
	records, err = dispatch.ListHostAllocations(t.Context(), f.pool, *f.host(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range records {
		if r.Kind == "computer" && r.OwnerID == computerID {
			t.Fatal("physically stopped Computer still listed")
		}
	}
}

func TestHostAllocationListingPagesBeyondOneHundred(t *testing.T) {
	f, _, _ := preparationAllocationFixture(t)
	// Populate custody history directly: this test exercises enumeration, not
	// placement or capacity admission. Every synthetic Computer has its own lease.
	for range 101 {
		computerID := uuid.NewV7()
		dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO computers(environment_id,id,initial_root_id,initial_root_digest,origin_template_id,origin_deployment_id,cpu_millis,memory_bytes,disk_bytes,storage_reservation_bytes) SELECT environment_id,$2,initial_root_id,initial_root_digest,origin_template_id,origin_deployment_id,cpu_millis,memory_bytes,disk_bytes,storage_reservation_bytes FROM computers WHERE environment_id=$1 LIMIT 1;
 INSERT INTO computer_leases(environment_id,computer_id,epoch,worker_host_id,worker_epoch,expires_at,status,allocation_id,channel_credential_digest,reserved_memory_bytes,reserved_scratch_bytes,vm_platform_id,vm_vcpu_count,cpu_config_digest,delivered_at,initialized_at)
 SELECT environment_id,$2,epoch,worker_host_id,worker_epoch,expires_at,status,$2,channel_credential_digest,reserved_memory_bytes,reserved_scratch_bytes,vm_platform_id,vm_vcpu_count,cpu_config_digest,delivered_at,initialized_at FROM computer_leases WHERE environment_id=$1 LIMIT 1`, pgx.QueryExecModeSimpleProtocol, f.env, computerID)
	}
	seen := map[uuid.UUID]bool{}
	var cursor *dispatch.HostAllocation
	pages := 0
	for {
		records, err := dispatch.ListHostAllocations(t.Context(), f.pool, *f.host(), cursor)
		if err != nil {
			t.Fatal(err)
		}
		if len(records) == 0 {
			break
		}
		if len(records) > 100 {
			t.Fatalf("unbounded page: %d", len(records))
		}
		pages++
		if pages > 3 {
			t.Fatal("cursor did not finish")
		}
		for _, r := range records {
			if seen[r.InstanceID] {
				t.Fatalf("duplicate instance: %s", r.InstanceID)
			}
			seen[r.InstanceID] = true
		}
		cursor = &records[len(records)-1]
	}
	if len(seen) != 102 || pages != 2 {
		t.Fatalf("incomplete scan: %d instances, %d pages", len(seen), pages)
	}
}

func TestPreparationDeliveryRejectsWrongPhysicalIdentity(t *testing.T) {
	f, p, a := preparationAllocationFixture(t)
	r, err := a.AllocateImageBuild(t.Context(), f.env, p.ID, f.worker)
	if err != nil {
		t.Fatal(err)
	}
	correct := computer.ImageBuildIdentity{EnvironmentID: f.env, ImageBuildID: p.ID, Epoch: r.Epoch, InstanceID: r.InstanceID}
	for _, change := range []func(*computer.ImageBuildIdentity){func(i *computer.ImageBuildIdentity) { i.Epoch++ }, func(i *computer.ImageBuildIdentity) { i.InstanceID = uuid.NewV7() }} {
		wrong := correct
		change(&wrong)
		if _, err := a.DeliverImageBuild(t.Context(), *f.host(), wrong); !errors.Is(err, dispatch.ErrDenied) {
			t.Fatalf("wrong identity delivered: %v", err)
		}
	}
	var untouched bool
	if err := f.pool.QueryRow(t.Context(), `SELECT delivered_at IS NULL AND executor_expires_at IS NULL FROM image_builds WHERE environment_id=$1 AND id=$2`, f.env, p.ID).Scan(&untouched); err != nil || !untouched {
		t.Fatalf("wrong delivery consumed lifetime: %v %v", untouched, err)
	}
}

func TestHostAllocationListingKeepsRetainedComputerIdentityAndCursor(t *testing.T) {
	f := newFixture(t)
	before, err := dispatch.ListHostAllocations(t.Context(), f.pool, *f.host(), nil)
	if err != nil || len(before) != 1 || before[0].Kind != "computer" {
		t.Fatalf("initial physical identity: %+v %v", before, err)
	}
	identity := before[0]
	request := f.captureRequest()
	beginCapture(t, f, request)
	// Publication may retain the physical source VM until optional reclamation.
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE checkpoints SET status='ready',manifest=decode('01','hex'),vm_platform_id=(SELECT vm_platform_id FROM computer_leases WHERE environment_id=$2 AND computer_id=$3 AND epoch=1),ready_at=clock_timestamp() WHERE environment_id=$2 AND id=$1`, request.CheckpointID, f.env, f.computer)
	var kind string
	if err := f.pool.QueryRow(t.Context(), `SELECT kind FROM (`+db.AllocationUsageSQL+`) allocation_usage WHERE environment_id=$1 AND owner_id=$2`, f.env, f.computer).Scan(&kind); err != nil || kind != "retained" {
		t.Fatalf("ready source lost retained capacity classification: %q %v", kind, err)
	}
	fresh, err := dispatch.ListHostAllocations(t.Context(), f.pool, *f.host(), nil)
	if err != nil || len(fresh) != 1 || fresh[0] != identity {
		t.Fatalf("checkpoint changed complete physical identity: before=%+v after=%+v err=%v", identity, fresh, err)
	}
	// A cursor issued before publication must not repeat the retained Computer.
	// It must also stay before preparation/seed in the physical protocol's order.
	for _, cursorKind := range []string{"computer", "preparation", "seed"} {
		cursor := identity
		cursor.Kind = cursorKind
		next, err := dispatch.ListHostAllocations(t.Context(), f.pool, *f.host(), &cursor)
		if err != nil || len(next) != 0 {
			t.Fatalf("retained source repeated after %q cursor: %+v %v", cursorKind, next, err)
		}
	}
	lease := computer.LeaseIdentity{EnvironmentID: identity.EnvironmentID, ComputerID: identity.OwnerID, Epoch: identity.Epoch, InstanceID: identity.InstanceID}
	if err := dispatch.ObserveLeaseStopped(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, *f.host(), lease, uuid.Nil(), uuid.Nil()); err != nil {
		t.Fatal(err)
	}
	stopped, err := dispatch.ListHostAllocations(t.Context(), f.pool, *f.host(), nil)
	if err != nil || len(stopped) != 0 {
		t.Fatalf("fenced retained source still listed: %+v %v", stopped, err)
	}
}
