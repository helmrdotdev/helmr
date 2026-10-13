package session_test

import (
	"bytes"
	"errors"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/session"

	"github.com/helmrdotdev/helmr/internal/dispatch"

	"github.com/helmrdotdev/helmr/internal/computer"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/secret"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/jackc/pgx/v5"
)

func preparationAllocationFixture(t *testing.T) (preparationFixture, computer.ImageBuild, *dispatch.Allocator) {
	t.Helper()
	f := preparationFixtureFor(t, newAdmissionFixture(t))
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE worker_groups SET primary_pool_id=(SELECT worker_pool_id FROM worker_hosts WHERE id=$1) WHERE id=$2;
 UPDATE worker_hosts SET observed_at=clock_timestamp(),epoch_cpu_millis=8000,epoch_memory_bytes=8589934592,epoch_guest_ephemeral_disk_bytes=8589934592,max_vm_slots=8,max_vm_starts=8 WHERE id=$1;
 UPDATE environments SET max_resident_computers=1,max_cpu_millis=2000,max_memory_bytes=1073741824 WHERE id=$3;
 UPDATE template_definitions SET cpu_millis=500 WHERE environment_id=$3`, pgx.QueryExecModeSimpleProtocol, f.worker, f.group, f.env)
	a, err := dispatch.NewAllocator(f.pool, bytes.Repeat([]byte{19}, 32), nil)
	if err != nil {
		t.Fatal(err)
	}
	return f, f.attach(t, f.waiter(t)), a
}

func TestAllocationPreparationUsesResourcesWithoutComputerCount(t *testing.T) {
	f, p, a := preparationAllocationFixture(t)
	r, err := a.AllocateImageBuild(t.Context(), f.env, p.ID, f.worker)
	if err != nil {
		t.Fatal(err)
	}
	if r.Shape.CPUMillis != 1000 || r.Shape.MemoryBytes != 512<<20 || r.Shape.ScratchBytes != 1<<30 || r.DeliveredAt != nil || r.ExpiresAt != nil {
		t.Fatalf("incorrect reservation or premature execution lease: %+v", r)
	}
	// Historical lookup is independent of current provisioning authority.
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE worker_hosts SET status='lost',lost_at=clock_timestamp() WHERE id=$1;
 UPDATE image_builds SET status='failed',error_code='executor_lost' WHERE environment_id=$2 AND id=$3`, pgx.QueryExecModeSimpleProtocol, f.worker, f.env, p.ID)
	got, err := a.AllocateImageBuild(t.Context(), f.env, p.ID, f.worker)
	if err != nil || got.InstanceID != r.InstanceID || got.Shape != r.Shape || got.Status != "failed" || got.DeliveredAt != nil {
		t.Fatalf("retry replaced allocation: %+v %v", got, err)
	}
}

func TestAllocationPreparationCapacityFailureRollsBack(t *testing.T) {
	f, p, a := preparationAllocationFixture(t)
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE environments SET max_cpu_millis=1999 WHERE id=$1`, f.env)
	if _, err := a.AllocateImageBuild(t.Context(), f.env, p.ID, f.worker); !errors.Is(err, dispatch.ErrNotReady) {
		t.Fatalf("overcommitted physical CPU: %v", err)
	}
	var untouched bool
	if err := f.pool.QueryRow(t.Context(), `SELECT status='queued' AND worker_host_id IS NULL AND reserved_cpu_millis IS NULL FROM image_builds WHERE environment_id=$1 AND id=$2`, f.env, p.ID).Scan(&untouched); err != nil || !untouched {
		t.Fatalf("rejected reservation leaked: %v %v", untouched, err)
	}
}

func TestAllocationPreparationDeliveryIsStableAndCannotResurrect(t *testing.T) {
	f, p, a := preparationAllocationFixture(t)
	r, err := a.AllocateImageBuild(t.Context(), f.env, p.ID, f.worker)
	if err != nil {
		t.Fatal(err)
	}
	wrong, _ := dispatch.NewAllocator(f.pool, bytes.Repeat([]byte{20}, 32), nil)
	if _, err := wrong.DeliverImageBuild(t.Context(), *f.host(), computer.ImageBuildIdentity{EnvironmentID: f.env, ImageBuildID: p.ID, InstanceID: r.InstanceID, Epoch: r.Epoch}); !errors.Is(err, dispatch.ErrDenied) {
		t.Fatalf("mismatched key accepted: %v", err)
	}
	other := *f.host()
	other.Epoch++
	if _, err := a.DeliverImageBuild(t.Context(), other, computer.ImageBuildIdentity{EnvironmentID: f.env, ImageBuildID: p.ID, InstanceID: r.InstanceID, Epoch: r.Epoch}); !errors.Is(err, dispatch.ErrDenied) && !errors.Is(err, dispatch.ErrNotReady) {
		t.Fatalf("wrong epoch accepted: %v", err)
	}
	var untouched bool
	if err := f.pool.QueryRow(t.Context(), `SELECT delivered_at IS NULL AND executor_expires_at IS NULL FROM image_builds WHERE environment_id=$1 AND id=$2`, f.env, p.ID).Scan(&untouched); err != nil || !untouched {
		t.Fatalf("failed delivery changed authority: %v %v", untouched, err)
	}
	first, err := a.DeliverImageBuild(t.Context(), *f.host(), computer.ImageBuildIdentity{EnvironmentID: f.env, ImageBuildID: p.ID, InstanceID: r.InstanceID, Epoch: r.Epoch})
	if err != nil {
		t.Fatal(err)
	}
	restarted, _ := dispatch.NewAllocator(f.pool, bytes.Repeat([]byte{19}, 32), nil)
	again, err := restarted.DeliverImageBuild(t.Context(), *f.host(), computer.ImageBuildIdentity{EnvironmentID: f.env, ImageBuildID: p.ID, InstanceID: r.InstanceID, Epoch: r.Epoch})
	if err != nil {
		t.Fatal(err)
	}
	if first.InstanceID != r.InstanceID || !bytes.Equal(first.Executor.ChannelCredential, again.Executor.ChannelCredential) || !first.ExpiresAt.Equal(*again.ExpiresAt) || !first.DeliveredAt.Equal(*again.DeliveredAt) {
		t.Fatalf("redelivery changed identity or authority")
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE image_builds SET executor_expires_at=clock_timestamp()-interval '1 second' WHERE environment_id=$1 AND id=$2`, f.env, p.ID)
	if _, err := a.DeliverImageBuild(t.Context(), *f.host(), computer.ImageBuildIdentity{EnvironmentID: f.env, ImageBuildID: p.ID, InstanceID: r.InstanceID, Epoch: r.Epoch}); !errors.Is(err, dispatch.ErrAllocationClosed) {
		t.Fatalf("expired delivery resurrected: %v", err)
	}
}

func freshComputerAllocationFixture(t *testing.T) (fixture, *dispatch.Allocator, uuid.UUID) {
	t.Helper()
	f, _, a := preparationAllocationFixture(t)
	p, key := preparationPublicationTestFor(t, f)
	root := p.captureCapacity(t, key, disk.SeedCapacity)
	if err := p.publisher.Publish(t.Context(), *f.host(), p.ref, root, "certified"); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE environments SET max_cpu_millis=8000,max_memory_bytes=8589934592,max_resident_computers=3 WHERE id=$1`, f.env)
	admitted, err := session.Start(t.Context(), f.pool, nil, f.caller(), f.startRequest("fresh-allocation"))
	if err != nil {
		t.Fatal(err)
	}
	var computerID uuid.UUID
	if err := f.pool.QueryRow(t.Context(), `SELECT computer_id FROM sessions WHERE environment_id=$1 AND id=$2`, f.env, admitted.SessionID).Scan(&computerID); err != nil {
		t.Fatal(err)
	}
	return f.fixture, a, computerID
}

func TestAllocationFreshComputerRequiresExactDeliveredReadyReceipt(t *testing.T) {
	f, a, computerID := freshComputerAllocationFixture(t)
	r, err := a.AllocateFreshLease(t.Context(), f.env, computerID, 1, f.worker)
	if err != nil {
		t.Fatal(err)
	}
	identity := computer.LeaseIdentity{EnvironmentID: f.env, ComputerID: computerID, InstanceID: r.InstanceID, Epoch: r.Epoch}
	if r.DeliveredAt != nil || r.InitializedAt != nil || r.ExpiresAt != nil {
		t.Fatal("premature authority")
	}
	if err := dispatch.ObserveFreshLeaseReady(t.Context(), f.pool, *f.host(), identity, r.Shape, r.BaseVersion); !errors.Is(err, dispatch.ErrNotReady) {
		t.Fatalf("ready before delivery: %v", err)
	}
	if err := dispatch.ExpireLease(t.Context(), f.pool, f.env, computerID, 1); err != nil {
		t.Fatal(err)
	}
	first, err := a.DeliverLease(t.Context(), *f.host(), identity)
	if err != nil {
		t.Fatal(err)
	}
	again, err := a.DeliverLease(t.Context(), *f.host(), identity)
	if err != nil || first.ChannelCredential != again.ChannelCredential || !first.ExpiresAt.Equal(*again.ExpiresAt) {
		t.Fatalf("delivery changed authority: %v", err)
	}
	wrong := r.Shape
	wrong.VCPUCount++
	if err := dispatch.ObserveFreshLeaseReady(t.Context(), f.pool, *f.host(), identity, wrong, r.BaseVersion); !errors.Is(err, dispatch.ErrDenied) {
		t.Fatalf("wrong shape accepted: %v", err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE worker_groups SET status='paused' WHERE id=$1`, f.group)
	if err := dispatch.ObserveFreshLeaseReady(t.Context(), f.pool, *f.host(), identity, r.Shape, r.BaseVersion); !errors.Is(err, dispatch.ErrNotReady) {
		t.Fatalf("paused initial activation: %v", err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE worker_groups SET status='active' WHERE id=$1`, f.group)
	if err := dispatch.ObserveFreshLeaseReady(t.Context(), f.pool, *f.host(), identity, r.Shape, r.BaseVersion); err != nil {
		t.Fatal(err)
	}
	got, err := a.AllocateFreshLease(t.Context(), f.env, computerID, 1, f.worker)
	if err != nil || got.Status != "active" || got.InitializedAt == nil {
		t.Fatalf("missing ready receipt: %+v %v", got, err)
	}
	if err := dispatch.ObserveLeaseStopped(t.Context(), f.pool, *f.host(), identity, uuid.Nil()); err != nil {
		t.Fatal(err)
	}
	if _, err := a.AllocateFreshLease(t.Context(), f.env, computerID, 2, f.worker); !errors.Is(err, dispatch.ErrNotReady) {
		t.Fatalf("initialized Computer cold booted: %v", err)
	}
}

func TestAllocationDiscoveryAdvancesPastFullHostPage(t *testing.T) {
	f, p, a := preparationAllocationFixture(t)
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE worker_hosts SET epoch_cpu_millis=500 WHERE id=$1`, f.worker)
	var last uuid.UUID
	for range 100 {
		last = uuid.NewV7()
		dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO worker_hosts SELECT (jsonb_populate_record(NULL::worker_hosts,to_jsonb(h)||jsonb_build_object('id',$2::text,'resource_id',$2::text))).* FROM worker_hosts h WHERE h.id=$1`, f.worker, last)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE worker_hosts SET epoch_cpu_millis=8000 WHERE id=$1`, last)
	next, more, err := a.Reconcile(t.Context(), dispatch.AllocationPosition{})
	if err != nil || !more {
		t.Fatalf("first page: %v %v", more, err)
	}
	if _, err := dispatch.ReadImageBuildAllocation(t.Context(), f.pool, f.env, p.ID); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("allocated on full host: %v", err)
	}
	_, more, err = a.Reconcile(t.Context(), next)
	if err != nil || more {
		t.Fatalf("last page: %v %v", more, err)
	}
	got, err := dispatch.ReadImageBuildAllocation(t.Context(), f.pool, f.env, p.ID)
	if err != nil || got.HostID != last {
		t.Fatalf("later supply starved: %+v %v", got, err)
	}
}

func TestAllocationRestoreRetainsCheckpointShapeAndSingleWriter(t *testing.T) {
	cp := readyCheckpointFixture(t)
	f := cp.f
	a, err := dispatch.NewAllocator(f.pool, bytes.Repeat([]byte{19}, 32), nil)
	if err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE worker_groups SET primary_pool_id=(SELECT worker_pool_id FROM worker_hosts WHERE id=$1) WHERE id=$2;
 UPDATE worker_hosts SET observed_at=clock_timestamp(),epoch_cpu_millis=8000,epoch_memory_bytes=8589934592,epoch_guest_ephemeral_disk_bytes=8589934592,max_vm_slots=8,max_vm_starts=8 WHERE id=$1;
 UPDATE environments SET max_resident_computers=1,max_cpu_millis=2000,max_memory_bytes=1073741824 WHERE id=$3`, pgx.QueryExecModeSimpleProtocol, f.worker, f.group, f.env)
	if _, err := a.AllocateRestoredLease(t.Context(), f.env, cp.manifest.CheckpointID, 2, f.worker); !errors.Is(err, dispatch.ErrNotReady) {
		t.Fatalf("source still owns VM: %v", err)
	}
	source := computer.LeaseIdentity{EnvironmentID: f.env, ComputerID: f.computer, Epoch: 1, InstanceID: cp.manifest.InstanceID}
	if err := dispatch.ObserveLeaseStopped(t.Context(), f.pool, *f.host(), source, uuid.Nil()); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE worker_hosts SET per_vm_guest_ephemeral_disk_bytes=536870912 WHERE id=$1`, f.worker)
	if _, err := a.AllocateRestoredLease(t.Context(), f.env, cp.manifest.CheckpointID, 2, f.worker); !errors.Is(err, dispatch.ErrNotReady) {
		t.Fatalf("undersized scratch admitted: %v", err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE worker_hosts SET per_vm_guest_ephemeral_disk_bytes=1073741824 WHERE id=$1`, f.worker)
	r, err := a.AllocateRestoredLease(t.Context(), f.env, cp.manifest.CheckpointID, 2, f.worker)
	if err != nil {
		t.Fatal(err)
	}
	if r.RestoredFrom == nil || *r.RestoredFrom != cp.save || r.BaseVersion != cp.save.String() || r.Shape.VCPUCount != int64(cp.manifest.Runtime.VMVCPUCount) || r.ExpiresAt != nil {
		t.Fatalf("wrong restore reservation: %+v", r)
	}
	if _, err := a.AllocateFreshLease(t.Context(), f.env, f.computer, 2, f.worker); !errors.Is(err, dispatch.ErrConflict) {
		t.Fatalf("restore receipt became fresh: %v", err)
	}
	identity := computer.LeaseIdentity{EnvironmentID: f.env, ComputerID: f.computer, Epoch: 2, InstanceID: r.InstanceID}
	if _, err := a.DeliverLease(t.Context(), *f.host(), identity); err != nil {
		t.Fatal(err)
	}
	if err := dispatch.ObserveFreshLeaseReady(t.Context(), f.pool, *f.host(), identity, r.Shape, r.BaseVersion); !errors.Is(err, dispatch.ErrDenied) {
		t.Fatalf("restore skipped checkpoint activation: %v", err)
	}
	if _, err := a.AllocateRestoredLease(t.Context(), f.env, cp.manifest.CheckpointID, 3, f.worker); !errors.Is(err, dispatch.ErrNotReady) {
		t.Fatalf("target duplicate allocated: %v", err)
	}
	if err := dispatch.ObserveLeaseStopped(t.Context(), f.pool, *f.host(), identity, uuid.Nil()); err != nil {
		t.Fatal(err)
	}
	replacement, err := a.AllocateRestoredLease(t.Context(), f.env, cp.manifest.CheckpointID, 3, f.worker)
	if err != nil || replacement.InstanceID == r.InstanceID || replacement.RestoredFrom == nil || *replacement.RestoredFrom != cp.save {
		t.Fatalf("lost target retry: %+v %v", replacement, err)
	}
	exposed := checkpointImageExposure(t, f)
	if _, err := exposed.secrets.Revoke(t.Context(), f.env, exposed.secretID, "revoke-restore"); err != nil {
		t.Fatal(err)
	}
	target := computer.LeaseIdentity{EnvironmentID: f.env, ComputerID: f.computer, InstanceID: replacement.InstanceID, Epoch: 3}
	if _, err := a.DeliverLease(t.Context(), *f.host(), target); !errors.Is(err, dispatch.ErrAllocationClosed) {
		t.Fatalf("revoked restore delivery not terminal: %v", err)
	}
	if err := dispatch.ObserveLeaseStopped(t.Context(), f.pool, *f.host(), target, uuid.Nil()); err != nil {
		t.Fatal(err)
	}
}

func TestAllocationPreparationConcurrentQuotaIsAtomic(t *testing.T) {
	f, p, a := preparationAllocationFixture(t)
	otherSpec := uuid.NewV7()
	otherComputer := f.waiter(t)
	dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO image_specs SELECT (jsonb_populate_record(NULL::image_specs,to_jsonb(s)||jsonb_build_object('id',$3::text,'spec_digest','sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff'))).* FROM image_specs s WHERE environment_id=$1 AND id=$2;
 INSERT INTO templates(environment_id,id,key) VALUES($1,$3,'other');
 INSERT INTO template_definitions SELECT (jsonb_populate_record(NULL::template_definitions,to_jsonb(d)||jsonb_build_object('template_id',$3::text,'image_spec_id',$3::text))).* FROM template_definitions d WHERE environment_id=$1 AND deployment_id=$2;
 UPDATE computers SET image_spec_id=$3 WHERE environment_id=$1 AND id=$4`, pgx.QueryExecModeSimpleProtocol, f.env, f.deployment, otherSpec, otherComputer)
	second := f.attach(t, otherComputer)
	otherHost := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO worker_hosts SELECT (jsonb_populate_record(NULL::worker_hosts,to_jsonb(h)||jsonb_build_object('id',$2::text,'resource_id',$2::text))).* FROM worker_hosts h WHERE h.id=$1`, f.worker, otherHost)
	start := make(chan struct{})
	results := make(chan error, 2)
	for _, job := range []struct{ preparation, host uuid.UUID }{{p.ID, f.worker}, {second.ID, otherHost}} {
		go func() {
			<-start
			_, err := a.AllocateImageBuild(t.Context(), f.env, job.preparation, job.host)
			results <- err
		}()
	}
	close(start)
	successes := 0
	for range 2 {
		err := <-results
		if err == nil {
			successes++
		} else if !errors.Is(err, dispatch.ErrNotReady) {
			t.Fatal(err)
		}
	}
	if successes != 1 {
		t.Fatalf("quota admitted %d executors", successes)
	}
	var reserved int64
	if err := f.pool.QueryRow(t.Context(), `SELECT sum(reserved_cpu_millis) FROM image_builds WHERE environment_id=$1 AND worker_host_id IS NOT NULL AND fenced_at IS NULL`, f.env).Scan(&reserved); err != nil || reserved != 1000 {
		t.Fatalf("reservation total %d: %v", reserved, err)
	}
}

func TestAllocationPreparationStopDoesNotRequireDeliveredCredential(t *testing.T) {
	for _, delivered := range []bool{false, true} {
		t.Run(map[bool]string{false: "never-delivered", true: "delivery-response-lost"}[delivered], func(t *testing.T) {
			f, p, a := preparationAllocationFixture(t)
			r, err := a.AllocateImageBuild(t.Context(), f.env, p.ID, f.worker)
			if err != nil {
				t.Fatal(err)
			}
			if delivered {
				if _, err := a.DeliverImageBuild(t.Context(), *f.host(), computer.ImageBuildIdentity{EnvironmentID: f.env, ImageBuildID: p.ID, InstanceID: r.InstanceID, Epoch: r.Epoch}); err != nil {
					t.Fatal(err)
				}
			}
			dbtest.MustExec(t, t.Context(), f.pool, `UPDATE image_builds SET deadline_at=clock_timestamp() WHERE environment_id=$1 AND id=$2`, f.env, p.ID)
			if _, err := a.DeliverImageBuild(t.Context(), *f.host(), computer.ImageBuildIdentity{EnvironmentID: f.env, ImageBuildID: p.ID, InstanceID: r.InstanceID, Epoch: r.Epoch}); !errors.Is(err, dispatch.ErrAllocationClosed) {
				t.Fatalf("deadline is not terminal: %v", err)
			}
			if err := dispatch.ExpireImageBuild(t.Context(), f.pool, f.env, p.ID); err != nil {
				t.Fatal(err)
			}
			identity := computer.ImageBuildIdentity{EnvironmentID: f.env, ImageBuildID: p.ID, InstanceID: r.InstanceID, Epoch: r.Epoch}
			wrong := identity
			wrong.InstanceID = uuid.NewV7()
			if err := computer.ObserveImageBuildStopped(t.Context(), f.pool, *f.host(), wrong); !errors.Is(err, computer.ErrDenied) {
				t.Fatalf("wrong stop accepted: %v", err)
			}
			for range 2 {
				if err := computer.ObserveImageBuildStopped(t.Context(), f.pool, *f.host(), identity); err != nil {
					t.Fatal(err)
				}
			}
			var held bool
			if err := f.pool.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM image_builds WHERE environment_id=$1 AND id=$2 AND fenced_at IS NULL)`, f.env, p.ID).Scan(&held); err != nil || held {
				t.Fatalf("unreturned grant stranded custody: %v %v", held, err)
			}
		})
	}
}

func TestAllocationDeliveryDistinguishesPauseFromDrain(t *testing.T) {
	f, p, a := preparationAllocationFixture(t)
	r, err := a.AllocateImageBuild(t.Context(), f.env, p.ID, f.worker)
	if err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE worker_groups SET status='paused' WHERE id=$1`, f.group)
	if _, err := a.DeliverImageBuild(t.Context(), *f.host(), computer.ImageBuildIdentity{EnvironmentID: f.env, ImageBuildID: p.ID, InstanceID: r.InstanceID, Epoch: r.Epoch}); !errors.Is(err, dispatch.ErrNotReady) {
		t.Fatalf("paused delivery not retryable: %v", err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE worker_groups SET status='draining' WHERE id=$1`, f.group)
	if _, err := a.DeliverImageBuild(t.Context(), *f.host(), computer.ImageBuildIdentity{EnvironmentID: f.env, ImageBuildID: p.ID, InstanceID: r.InstanceID, Epoch: r.Epoch}); !errors.Is(err, dispatch.ErrAllocationClosed) {
		t.Fatalf("drained delivery not terminal: %v", err)
	}
	if err := computer.ObserveImageBuildStopped(t.Context(), f.pool, *f.host(), computer.ImageBuildIdentity{EnvironmentID: f.env, ImageBuildID: p.ID, InstanceID: r.InstanceID, Epoch: r.Epoch}); err != nil {
		t.Fatal(err)
	}
}

func TestAllocationComputerDeliveryDistinguishesPauseFromDrain(t *testing.T) {
	f, a, computerID := freshComputerAllocationFixture(t)
	r, err := a.AllocateFreshLease(t.Context(), f.env, computerID, 1, f.worker)
	if err != nil {
		t.Fatal(err)
	}
	identity := computer.LeaseIdentity{EnvironmentID: f.env, ComputerID: computerID, InstanceID: r.InstanceID, Epoch: 1}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE worker_groups SET status='paused' WHERE id=$1`, f.group)
	if _, err := a.DeliverLease(t.Context(), *f.host(), identity); !errors.Is(err, dispatch.ErrNotReady) {
		t.Fatalf("paused delivery not retryable: %v", err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE worker_groups SET status='draining' WHERE id=$1`, f.group)
	if _, err := a.DeliverLease(t.Context(), *f.host(), identity); !errors.Is(err, dispatch.ErrAllocationClosed) {
		t.Fatalf("drained delivery not terminal: %v", err)
	}
	if err := dispatch.ObserveLeaseStopped(t.Context(), f.pool, *f.host(), identity, uuid.Nil()); err != nil {
		t.Fatal(err)
	}
}

func TestAllocationRestoreDiscoversCompatibleNonPrimaryPool(t *testing.T) {
	cp := readyCheckpointFixture(t)
	f := cp.f
	a, err := dispatch.NewAllocator(f.pool, bytes.Repeat([]byte{19}, 32), nil)
	if err != nil {
		t.Fatal(err)
	}
	target, primary := uuid.NewV7(), uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO worker_pools SELECT (jsonb_populate_record(NULL::worker_pools,to_jsonb(p)||jsonb_build_object('id',$4::text,'name','future-primary'))).* FROM worker_pools p JOIN worker_hosts h ON h.worker_pool_id=p.id WHERE h.id=$1;
 UPDATE worker_groups SET primary_pool_id=$4 WHERE id=$2;
 INSERT INTO worker_hosts SELECT (jsonb_populate_record(NULL::worker_hosts,to_jsonb(h)||jsonb_build_object('id',$5::text,'resource_id',$5::text,'per_vm_guest_ephemeral_disk_bytes',2147483648))).* FROM worker_hosts h WHERE h.id=$1;
 UPDATE worker_hosts SET observed_at=clock_timestamp(),epoch_cpu_millis=8000,epoch_memory_bytes=8589934592,epoch_guest_ephemeral_disk_bytes=8589934592,max_vm_slots=8,max_vm_starts=8 WHERE id IN ($1,$5);
 UPDATE environments SET max_resident_computers=1,max_cpu_millis=2000,max_memory_bytes=1073741824 WHERE id=$3`, pgx.QueryExecModeSimpleProtocol, f.worker, f.group, f.env, primary, target)
	f.enqueue(t, "restore-demand")
	if err := dispatch.ObserveLeaseStopped(t.Context(), f.pool, *f.host(), computer.LeaseIdentity{EnvironmentID: f.env, ComputerID: f.computer, InstanceID: cp.manifest.InstanceID, Epoch: 1}, uuid.Nil()); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE worker_hosts SET status='draining',draining_at=clock_timestamp(),drain_reason='admin' WHERE id=$1`, f.worker)
	if _, more, err := a.Reconcile(t.Context(), dispatch.AllocationPosition{}); err != nil || more {
		t.Fatalf("restore discovery: %v %v", more, err)
	}
	got, err := dispatch.ReadLeaseAllocation(t.Context(), f.pool, f.env, f.computer, 2)
	if err != nil || got.HostID != target || got.Shape.ScratchBytes != 1<<30 {
		t.Fatalf("compatible non-primary capacity ignored: %+v %v", got, err)
	}
}

func TestAllocationHostCapacitySerializesDifferentEnvironments(t *testing.T) {
	f, p, a := preparationAllocationFixture(t)
	otherEnv := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE worker_hosts SET epoch_cpu_millis=2000 WHERE id=$3;
 INSERT INTO environments SELECT (jsonb_populate_record(NULL::environments,to_jsonb(e)||jsonb_build_object('id',$2::text,'slug','other','is_default',false,'current_deployment_id',NULL))).* FROM environments e WHERE e.id=$1;
 INSERT INTO deployments SELECT (jsonb_populate_record(NULL::deployments,to_jsonb(d)||jsonb_build_object('environment_id',$2::text))).* FROM deployments d WHERE d.environment_id=$1;
 INSERT INTO image_specs SELECT (jsonb_populate_record(NULL::image_specs,to_jsonb(s)||jsonb_build_object('environment_id',$2::text))).* FROM image_specs s WHERE s.environment_id=$1;
 INSERT INTO templates(environment_id,id,key) SELECT $2,id,key FROM templates WHERE environment_id=$1;
 INSERT INTO template_definitions SELECT (jsonb_populate_record(NULL::template_definitions,to_jsonb(d)||jsonb_build_object('environment_id',$2::text))).* FROM template_definitions d WHERE d.environment_id=$1;
 INSERT INTO image_builds(environment_id,id,image_spec_id,idempotency_key,deadline_at) SELECT $2,id,image_spec_id,idempotency_key,deadline_at FROM image_builds WHERE environment_id=$1`, pgx.QueryExecModeSimpleProtocol, f.env, otherEnv, f.worker)
	start := make(chan struct{})
	results := make(chan error, 2)
	for _, env := range []uuid.UUID{f.env, otherEnv} {
		go func() { <-start; _, err := a.AllocateImageBuild(t.Context(), env, p.ID, f.worker); results <- err }()
	}
	close(start)
	successes := 0
	for range 2 {
		err := <-results
		if err == nil {
			successes++
		} else if !errors.Is(err, dispatch.ErrNotReady) {
			t.Fatal(err)
		}
	}
	if successes != 1 {
		t.Fatalf("Host capacity admitted %d executors", successes)
	}
}

func TestAllocationRevokedImageRequiresPhysicalStop(t *testing.T) {
	for _, delivered := range []bool{false, true} {
		t.Run(map[bool]string{false: "before-delivery", true: "before-ready"}[delivered], func(t *testing.T) {
			f, a, computerID := freshComputerAllocationFixture(t)
			r, err := a.AllocateFreshLease(t.Context(), f.env, computerID, 1, f.worker)
			if err != nil {
				t.Fatal(err)
			}
			identity := computer.LeaseIdentity{EnvironmentID: f.env, ComputerID: computerID, InstanceID: r.InstanceID, Epoch: 1}
			if delivered {
				if _, err := a.DeliverLease(t.Context(), *f.host(), identity); err != nil {
					t.Fatal(err)
				}
			}
			var secretID uuid.UUID
			if err := f.pool.QueryRow(t.Context(), `SELECT x.secret_id FROM computers c JOIN images i ON (i.environment_id,i.id)=(c.environment_id,c.image_id)
 JOIN secret_exposures x ON (x.environment_id,x.image_build_id)=(i.environment_id,i.image_build_id) WHERE c.environment_id=$1 AND c.id=$2 LIMIT 1`, f.env, computerID).Scan(&secretID); err != nil {
				t.Fatal(err)
			}
			store, err := secret.New(db.New(f.pool), f.pool, bytes.Repeat([]byte{17}, 32))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.Revoke(t.Context(), f.env, secretID, "revoke-before-start"); err != nil {
				t.Fatal(err)
			}
			wrong := identity
			wrong.InstanceID = uuid.NewV7()
			if _, err := a.DeliverLease(t.Context(), *f.host(), wrong); !errors.Is(err, dispatch.ErrDenied) {
				t.Fatalf("wrong identity was not denied: %v", err)
			}
			if _, err := a.DeliverLease(t.Context(), *f.host(), identity); !errors.Is(err, dispatch.ErrAllocationClosed) {
				t.Fatalf("revoked delivery not terminal: %v", err)
			}
			if err := dispatch.ObserveFreshLeaseReady(t.Context(), f.pool, *f.host(), identity, r.Shape, r.BaseVersion); !errors.Is(err, dispatch.ErrAllocationClosed) {
				t.Fatalf("revoked ready not terminal: %v", err)
			}
			if err := dispatch.ObserveLeaseStopped(t.Context(), f.pool, *f.host(), identity, uuid.Nil()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestAllocationInitialReceiptSurvivesPayloadRetirement(t *testing.T) {
	f := newFixture(t)
	a, err := dispatch.NewAllocator(f.pool, bytes.Repeat([]byte{19}, 32), nil)
	if err != nil {
		t.Fatal(err)
	}
	before, err := a.AllocateFreshLease(t.Context(), f.env, f.computer, 1, f.worker)
	if err != nil {
		t.Fatal(err)
	}
	var root, key uuid.UUID
	var pack string
	if err := f.pool.QueryRow(t.Context(), `SELECT r.id,r.page_key_id,r.pack_digest FROM computers c JOIN disk_roots r ON (r.environment_id,r.id)=(c.environment_id,c.initial_root_id) WHERE c.environment_id=$1 AND c.id=$2`, f.env, f.computer).Scan(&root, &key, &pack); err != nil {
		t.Fatal(err)
	}
	// Fixture the retention owner's completed physical-stop/deletion transition.
	// Reclaim real graph/key rows to prove receipt reads do not consult payload.
	dbtest.MustExec(t, t.Context(), f.pool, `
 UPDATE computer_leases SET status='released',fenced_at=clock_timestamp(),fence_evidence='fixture physical stop' WHERE environment_id=$1 AND computer_id=$2;
 UPDATE computers SET deleted_at=clock_timestamp(),initial_root_id=NULL,initial_payload_retired_at=clock_timestamp() WHERE environment_id=$1 AND id=$2;
 DELETE FROM disk_roots WHERE environment_id=$1 AND id=$3;
 DELETE FROM disk_objects WHERE environment_id=$1 AND digest=$4;
 UPDATE disk_encryption_keys SET retired_at=clock_timestamp(),wrapped_key=NULL WHERE id=$5;
 `, pgx.QueryExecModeSimpleProtocol, f.env, f.computer, root, pack, key)
	after, err := a.AllocateFreshLease(t.Context(), f.env, f.computer, 1, f.worker)
	if err != nil || after.BaseVersion != before.BaseVersion || after.InstanceID != before.InstanceID || after.Shape != before.Shape || after.Status != "released" {
		t.Fatalf("historical initial allocation changed: before=%+v after=%+v error=%v", before, after, err)
	}
	if _, err := a.AllocateFreshLease(t.Context(), f.env, f.computer, 2, f.worker); err == nil {
		t.Fatal("retired initial payload authorized a new allocation")
	}
	if _, err := bindComputerLeaseDiskForTest(t.Context(), f.pool, *f.host(), f.env, f.computer, 1, key); !errors.Is(err, session.ErrDenied) {
		t.Fatalf("historical initial evidence authorized materialization: %v", err)
	}
}

func TestAllocationDeliveredComputerBecomesReadyAfterHostDrain(t *testing.T) {
	f, a, computerID := freshComputerAllocationFixture(t)
	r, err := a.AllocateFreshLease(t.Context(), f.env, computerID, 1, f.worker)
	if err != nil {
		t.Fatal(err)
	}
	identity := computer.LeaseIdentity{EnvironmentID: f.env, ComputerID: computerID, InstanceID: r.InstanceID, Epoch: r.Epoch}
	first, err := a.DeliverLease(t.Context(), *f.host(), identity)
	if err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE worker_hosts SET status='draining',draining_at=clock_timestamp(),drain_reason='admin' WHERE id=$1`, f.worker)
	// A lost delivery response recovers the same live execution grant.
	replay, err := a.DeliverLease(t.Context(), *f.host(), identity)
	if err != nil || replay.ChannelCredential != first.ChannelCredential || !replay.ExpiresAt.Equal(*first.ExpiresAt) {
		t.Fatalf("delivery replay = %+v, %v", replay.Allocation, err)
	}
	if err := dispatch.ObserveFreshLeaseReady(t.Context(), f.pool, *f.host(), identity, r.Shape, r.BaseVersion); err != nil {
		t.Fatalf("admitted materialization rejected by Host drain: %v", err)
	}
	ready, err := dispatch.ReadLeaseAllocation(t.Context(), f.pool, f.env, computerID, r.Epoch)
	if err != nil || ready.InitializedAt == nil || ready.Status != "active" {
		t.Fatalf("ready allocation = %+v, %v", ready, err)
	}
	if err := dispatch.ObserveFreshLeaseReady(t.Context(), f.pool, *f.host(), identity, r.Shape, r.BaseVersion); err != nil {
		t.Fatalf("ready receipt replay: %v", err)
	}
}

func TestAllocationUndeliveredComputerStartsAfterHostDrain(t *testing.T) {
	f, a, computerID := freshComputerAllocationFixture(t)
	r, err := a.AllocateFreshLease(t.Context(), f.env, computerID, 1, f.worker)
	if err != nil {
		t.Fatal(err)
	}
	identity := computer.LeaseIdentity{EnvironmentID: f.env, ComputerID: computerID, InstanceID: r.InstanceID, Epoch: r.Epoch}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE worker_hosts SET status='draining',draining_at=clock_timestamp(),drain_reason='admin' WHERE id=$1`, f.worker)
	for _, pause := range []string{"admission_paused_reason"} {
		dbtest.MustExec(t, t.Context(), f.pool, `UPDATE worker_hosts SET `+pause+`='disk_floor' WHERE id=$1`, f.worker)
		if _, err := a.DeliverLease(t.Context(), *f.host(), identity); !errors.Is(err, dispatch.ErrNotReady) {
			t.Fatalf("drain bypassed temporary health denial (%s): %v", pause, err)
		}
		dbtest.MustExec(t, t.Context(), f.pool, `UPDATE worker_hosts SET `+pause+`=NULL WHERE id=$1`, f.worker)
	}
	first, err := a.DeliverLease(t.Context(), *f.host(), identity)
	if err != nil {
		t.Fatalf("committed allocation lost first delivery: %v", err)
	}
	replay, err := a.DeliverLease(t.Context(), *f.host(), identity)
	if err != nil || replay.ChannelCredential != first.ChannelCredential || !replay.ExpiresAt.Equal(*first.ExpiresAt) {
		t.Fatalf("delivery retry changed grant: %v", err)
	}
	if _, err := computer.CaptureProxyTrust(t.Context(), f.pool, *f.host(), r.InstanceID); err != nil {
		t.Fatalf("acquiring proxy trust on drain: %v", err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE worker_hosts SET admission_paused_reason='disk_floor' WHERE id=$1`, f.worker)
	if _, err := computer.CaptureProxyTrust(t.Context(), f.pool, *f.host(), r.InstanceID); err == nil {
		t.Fatal("drain bypassed acquiring health gate")
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE worker_hosts SET admission_paused_reason=NULL WHERE id=$1`, f.worker)
	if err := dispatch.ObserveFreshLeaseReady(t.Context(), f.pool, *f.host(), identity, r.Shape, r.BaseVersion); err != nil {
		t.Fatal(err)
	}
	if err := dispatch.ObserveLeaseStopped(t.Context(), f.pool, *f.host(), identity, uuid.Nil()); err != nil {
		t.Fatal(err)
	}
}

func TestAllocationUndeliveredPreparationStartsAfterHostDrain(t *testing.T) {
	f, p, a := preparationAllocationFixture(t)
	r, err := a.AllocateImageBuild(t.Context(), f.env, p.ID, f.worker)
	if err != nil {
		t.Fatal(err)
	}
	identity := computer.ImageBuildIdentity{EnvironmentID: f.env, ImageBuildID: p.ID, InstanceID: r.InstanceID, Epoch: r.Epoch}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE worker_hosts SET status='draining',draining_at=clock_timestamp(),drain_reason='admin' WHERE id=$1`, f.worker)
	for _, pause := range []string{"admission_paused_reason"} {
		dbtest.MustExec(t, t.Context(), f.pool, `UPDATE worker_hosts SET `+pause+`='disk_floor' WHERE id=$1`, f.worker)
		if _, err := a.DeliverImageBuild(t.Context(), *f.host(), identity); !errors.Is(err, dispatch.ErrNotReady) {
			t.Fatalf("drain bypassed temporary health denial (%s): %v", pause, err)
		}
		dbtest.MustExec(t, t.Context(), f.pool, `UPDATE worker_hosts SET `+pause+`=NULL WHERE id=$1`, f.worker)
	}
	first, err := a.DeliverImageBuild(t.Context(), *f.host(), identity)
	if err != nil {
		t.Fatalf("committed preparation lost first delivery: %v", err)
	}
	replay, err := a.DeliverImageBuild(t.Context(), *f.host(), identity)
	if err != nil || !bytes.Equal(replay.Executor.ChannelCredential, first.Executor.ChannelCredential) || !replay.ExpiresAt.Equal(*first.ExpiresAt) {
		t.Fatalf("preparation retry changed grant: %v", err)
	}
	if _, err := computer.RenewImageBuild(t.Context(), f.pool, *f.host(), first.Executor); err != nil {
		t.Fatalf("preparation renewal on drain: %v", err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE image_builds SET executor_expires_at=clock_timestamp()-interval '1 second' WHERE environment_id=$1 AND id=$2`, f.env, p.ID)
	if _, err := a.DeliverImageBuild(t.Context(), *f.host(), identity); !errors.Is(err, dispatch.ErrAllocationClosed) {
		t.Fatalf("drain revived expired preparation: %v", err)
	}
}

func TestAllocationHostDrainRejectsNewAssignment(t *testing.T) {
	t.Run("computer", func(t *testing.T) {
		f, a, computerID := freshComputerAllocationFixture(t)
		dbtest.MustExec(t, t.Context(), f.pool, `UPDATE worker_hosts SET status='draining',draining_at=clock_timestamp(),drain_reason='admin' WHERE id=$1`, f.worker)
		if _, err := a.AllocateFreshLease(t.Context(), f.env, computerID, 1, f.worker); err == nil {
			t.Fatal("new Computer assigned to draining Host")
		}
	})
	t.Run("preparation", func(t *testing.T) {
		f, p, a := preparationAllocationFixture(t)
		dbtest.MustExec(t, t.Context(), f.pool, `UPDATE worker_hosts SET status='draining',draining_at=clock_timestamp(),drain_reason='admin' WHERE id=$1`, f.worker)
		if _, err := a.AllocateImageBuild(t.Context(), f.env, p.ID, f.worker); err == nil {
			t.Fatal("new computer.ImageBuild assigned to draining Host")
		}
	})
}

func TestAllocationPreparationRemainsInDrainReadinessUntilFenced(t *testing.T) {
	f, p, a := preparationAllocationFixture(t)
	r, err := a.AllocateImageBuild(t.Context(), f.env, p.ID, f.worker)
	if err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE worker_hosts SET status='draining',draining_at=clock_timestamp(),drain_reason='admin' WHERE id=$1`, f.worker)
	check := func(want int32) {
		t.Helper()
		status, err := db.New(f.pool).GetWorkerHostStatus(t.Context(), db.GetWorkerHostStatusParams{ID: pgvalue.UUID(f.worker), WorkerGroupID: pgvalue.UUID(f.group), ObservationFreshnessSeconds: 60})
		if err != nil || status.ActiveInstances != want {
			t.Fatalf("drain custody count: got %d want %d: %v", status.ActiveInstances, want, err)
		}
	}
	check(2)
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE image_builds SET status='failed',error_code='executor_lost' WHERE environment_id=$1 AND id=$2`, f.env, p.ID)
	check(2)
	if err := computer.ObserveImageBuildStopped(t.Context(), f.pool, *f.host(), computer.ImageBuildIdentity{EnvironmentID: f.env, ImageBuildID: p.ID, InstanceID: r.InstanceID, Epoch: r.Epoch}); err != nil {
		t.Fatal(err)
	}
	check(1)
}

// testAllocator builds an allocator with the fixtures' channel key over the
// given database and Secret trust issuer.
func testAllocator(t *testing.T, database db.TxDB, trust dispatch.ImageBuildTrustIssuer) *dispatch.Allocator {
	t.Helper()
	a, err := dispatch.NewAllocator(database, bytes.Repeat([]byte{19}, 32), trust)
	if err != nil {
		t.Fatal(err)
	}
	return a
}
