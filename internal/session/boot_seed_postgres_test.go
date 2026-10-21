package session_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/artifact"
	"github.com/helmrdotdev/helmr/internal/bootseed"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/computercheckpoint"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/dispatch"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
)

func bootSeedFixture(t *testing.T) (*preparationPublicationTest, *dispatch.Allocator, uuid.UUID) {
	t.Helper()
	f, _, a := preparationAllocationFixture(t)
	p, key := preparationPublicationTestFor(t, f)
	root := p.capture(t, key)
	if err := p.publisher.Publish(t.Context(), *f.host(), p.ref, root, "qualified image"); err != nil {
		t.Fatal(err)
	}
	if err := computer.ObserveImageBuildStopped(t.Context(), f.pool, *f.host(), preparationIdentity(p.ref)); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE environments SET current_deployment_id=$2 WHERE id=$1`, f.env, f.deployment)
	if err := db.RunTx(t.Context(), f.pool, func(tx pgx.Tx) error { return computer.QueueDeploymentBootSeeds(t.Context(), tx, f.env, f.deployment) }); err != nil {
		t.Fatal(err)
	}
	var id uuid.UUID
	if err := f.pool.QueryRow(t.Context(), `SELECT id FROM boot_seed_builds WHERE environment_id=$1 AND deployment_id=$2 AND status='queued'`, f.env, f.deployment).Scan(&id); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE worker_pools SET optional_cpu_millis=capacity_cpu_millis,optional_memory_bytes=capacity_memory_bytes,optional_vm_slots=max_vm_slots,optional_max_tasks=max_vm_slots,optional_disk_bytes=1073741824,optional_io_bytes_per_second=1048576 WHERE id=(SELECT worker_pool_id FROM worker_hosts WHERE id=$1)`, f.worker)
	return p, a, id
}
func bootSeedReceipt(r computer.Allocation) dispatch.HostAllocation {
	return dispatch.HostAllocation{Kind: "seed", EnvironmentID: r.EnvironmentID, OwnerID: r.OwnerID, InstanceID: r.InstanceID, Epoch: r.Epoch}
}

func TestBootSeedAllocationRetainsCapacityUntilPhysicalStop(t *testing.T) {
	p, a, id := bootSeedFixture(t)
	f := p.f
	r, err := a.AllocateBootSeed(t.Context(), f.env, id, f.worker)
	if err != nil {
		t.Fatal(err)
	}
	if r.Shape.CPUMillis != 1000 || r.DeliveredAt != nil || r.ExpiresAt != nil {
		t.Fatalf("shape/early authority: %+v", r)
	}
	delivered, err := a.DeliverBootSeed(t.Context(), *f.host(), bootSeedReceipt(r))
	if err != nil {
		t.Fatal(err)
	}
	again, err := a.DeliverBootSeed(t.Context(), *f.host(), bootSeedReceipt(r))
	if err != nil || !bytes.Equal(delivered.Executor.ChannelCredential, again.Executor.ChannelCredential) {
		t.Fatalf("unstable delivery: %v", err)
	}
	other, _ := dispatch.NewAllocator(f.pool, bytes.Repeat([]byte{20}, 32), nil)
	if _, err := other.DeliverBootSeed(t.Context(), *f.host(), bootSeedReceipt(r)); !errors.Is(err, dispatch.ErrDenied) {
		t.Fatalf("credential authority: %v", err)
	}
	if err := computer.FailBootSeedBuild(t.Context(), f.pool, *f.host(), delivered.Executor, "test_failure"); err != nil {
		t.Fatal(err)
	}
	second := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO boot_seed_builds(environment_id,id,deployment_id,image_spec_id,image_id,cpu_millis,memory_bytes,status) SELECT environment_id,$3,deployment_id,image_spec_id,image_id,1000,memory_bytes,'queued' FROM boot_seed_builds WHERE environment_id=$1 AND id=$2`, f.env, id, second)
	if _, err := a.AllocateBootSeed(t.Context(), f.env, second, f.worker); !errors.Is(err, dispatch.ErrNotReady) {
		t.Fatalf("failed but unfenced seed released capacity: %v", err)
	}
	if err := dispatch.ObserveBootSeedStopped(t.Context(), f.pool, *f.host(), bootSeedReceipt(r)); err != nil {
		t.Fatal(err)
	}
	if _, err := a.AllocateBootSeed(t.Context(), f.env, second, f.worker); err != nil {
		t.Fatalf("stopped seed retained capacity: %v", err)
	}
}

func TestBootSeedPublicationAndRetention(t *testing.T) {
	p, a, id := bootSeedFixture(t)
	f := p.f
	r, err := a.AllocateBootSeed(t.Context(), f.env, id, f.worker)
	if err != nil {
		t.Fatal(err)
	}
	delivery, err := a.DeliverBootSeed(t.Context(), *f.host(), bootSeedReceipt(r))
	if err != nil {
		t.Fatal(err)
	}
	publisher, err := computer.NewBootSeedPublisher(f.pool, p.remote)
	if err != nil {
		t.Fatal(err)
	}
	put := func(media, content string) cas.Descriptor {
		t.Helper()
		o, err := p.remote.Put(t.Context(), media, bytes.NewBufferString(content))
		if err != nil {
			t.Fatal(err)
		}
		return cas.Descriptor{Digest: o.Digest, SizeBytes: o.SizeBytes, MediaType: o.MediaType}
	}
	// Content must be owned by the exact deployment before it can authorize an
	// eligible seed. Runtime selection is rechecked separately at actual admission.
	program := computercheckpoint.Program{Runtime: computercheckpoint.Object(put(artifact.RuntimeArtifactMediaType, "runtime")), Artifact: computercheckpoint.Object(put(artifact.ProgramArtifactMediaType, "program"))}
	var org uuid.UUID
	if err := f.pool.QueryRow(t.Context(), `SELECT org_id FROM environments WHERE id=$1`, f.env).Scan(&org); err != nil {
		t.Fatal(err)
	}
	if err := db.RunTx(t.Context(), f.pool, func(tx pgx.Tx) error {
		o := program.Artifact
		if _, err := db.New(tx).UpsertCasObject(t.Context(), db.UpsertCasObjectParams{OrgID: pgvalue.UUID(org), Digest: o.Digest, SizeBytes: o.SizeBytes, MediaType: o.MediaType}); err != nil {
			return err
		}
		_, err := tx.Exec(t.Context(), `INSERT INTO deployment_objects(environment_id,deployment_id,org_id,project_id,digest,size_bytes,media_type) SELECT id,$2,org_id,project_id,$3,$4,$5 FROM environments WHERE id=$1`, f.env, f.deployment, o.Digest, o.SizeBytes, o.MediaType)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var diskRoot disk.VersionRoot
	if err := f.pool.QueryRow(t.Context(), `SELECT `+computer.DiskRootColumns+` FROM images i JOIN disk_roots r ON (r.environment_id,r.id)=(i.environment_id,i.root_id) WHERE i.environment_id=$1 AND i.image_build_id=$2`, f.env, p.ref.ImageBuildID).Scan(computer.DiskRootFields(&diskRoot)...); err != nil {
		t.Fatal(err)
	}
	root, err := diskRoot.Digest()
	if err != nil {
		t.Fatal(err)
	}
	logical := diskRoot.LogicalBytes
	config, err := json.Marshal(bootseed.Content{Contract: "helmr.boot-seed.v1", RuntimeID: r.Shape.VMPlatformID, CPUConfig: r.Shape.CPUConfigDigest, VCPUs: r.Shape.VCPUCount, MemoryMiB: r.Shape.MemoryBytes >> 20, ScratchMiB: r.Shape.ScratchBytes >> 20, ComputerRoot: root, ComputerBytes: logical, Program: &program})
	if err != nil {
		t.Fatal(err)
	}
	manifest := bootseed.Manifest{EnvironmentID: f.env, BuildID: id, Config: config, MemoryBytes: r.Shape.MemoryBytes, ScratchBytes: r.Shape.ScratchBytes, MemoryHeaderDigest: sha256sum.DigestBytes([]byte("header")), VMState: put(cas.BootSeedVMStateMediaType, "state"), Memory: put(cas.BootSeedMemoryMediaType, "memory"), ScratchDisk: put(cas.BootSeedScratchMediaType, "scratch")}
	proof := bootseed.Qualification{Clones: [2]vm.BootSeedIdentity{{BootID: uuid.NewV7().String(), RandomDigest: sha256sum.DigestBytes([]byte("one"))}, {BootID: uuid.NewV7().String(), RandomDigest: sha256sum.DigestBytes([]byte("two"))}}}
	bad := proof
	bad.Clones[1] = bad.Clones[0]
	if err := publisher.Publish(t.Context(), *f.host(), delivery.Executor, manifest, bad); !errors.Is(err, computer.ErrInvalidInput) {
		t.Fatalf("shared clone identity accepted: %v", err)
	}
	for range 2 {
		if err := publisher.Publish(t.Context(), *f.host(), delivery.Executor, manifest, proof); err != nil {
			t.Fatalf("publish/reply replay: %v", err)
		}
	}
	changed := proof
	changed.Clones[0].BootID = uuid.NewV7().String()
	if err := publisher.Publish(t.Context(), *f.host(), delivery.Executor, manifest, changed); !errors.Is(err, computer.ErrConflict) {
		t.Fatalf("publication replay changed proof: %v", err)
	}
	// Selection must bind the admitted Program, root and physical shape; an
	// incompatible artifact is optional and leaves ordinary boot available.
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computers c SET image_id=i.id,initial_root_id=i.root_id,initial_root_digest=decode(substring($4::text from 8),'hex')
 FROM images i JOIN disk_roots r ON (r.environment_id,r.id)=(i.environment_id,i.root_id)
 WHERE c.environment_id=$1 AND c.id=$2 AND i.environment_id=c.environment_id AND i.image_build_id=$3;
 UPDATE computer_leases SET status='acquiring',initialized_at=NULL WHERE environment_id=$1 AND computer_id=$2`, pgx.QueryExecModeSimpleProtocol, f.env, f.computer, p.ref.ImageBuildID, root)
	identity := computer.LeaseIdentity{EnvironmentID: f.env, ComputerID: f.computer, InstanceID: f.computer, Epoch: 1}
	selectSeed := func(program computercheckpoint.Program, want bool) {
		t.Helper()
		got, err := computer.SelectBootSeed(t.Context(), f.pool, *f.host(), identity, f.deployment, program)
		if err != nil || (got != nil) != want || (got != nil && got.BuildID != id) {
			t.Fatalf("seed selection=%v want=%v err=%v", got, want, err)
		}
	}
	mismatched := program
	mismatched.Artifact.Digest = sha256sum.DigestBytes([]byte("another program"))
	selectSeed(mismatched, false)
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computer_leases SET reserved_memory_bytes=reserved_memory_bytes*2 WHERE environment_id=$1 AND computer_id=$2`, f.env, f.computer)
	selectSeed(program, false)
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computer_leases SET reserved_memory_bytes=reserved_memory_bytes/2 WHERE environment_id=$1 AND computer_id=$2`, f.env, f.computer)
	selectSeed(program, true)
	selectSeed(program, true)
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computer_leases SET disk_only_recovery=true WHERE environment_id=$1 AND computer_id=$2`, f.env, f.computer)
	selectSeed(program, false)
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computer_leases SET disk_only_recovery=false WHERE environment_id=$1 AND computer_id=$2`, f.env, f.computer)
	// A newer promoted deployment makes this payload obsolete, but neither the
	// builder nor a selected Computer may lose objects before physical closure.
	newer := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO deployments(environment_id,id,bundle_digest) VALUES($1,$2,$3); UPDATE environments SET current_deployment_id=$2 WHERE id=$1`, pgx.QueryExecModeSimpleProtocol, f.env, newer, sha256sum.DigestBytes([]byte("new deployment")))
	reconcile := func() {
		t.Helper()
		if err := computer.ReconcileDiskRetention(t.Context(), f.pool); err != nil {
			t.Fatal(err)
		}
	}
	count := func(want int) {
		t.Helper()
		var got int
		if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM boot_seed_objects WHERE environment_id=$1 AND build_id=$2`, f.env, id).Scan(&got); err != nil || got != want {
			t.Fatalf("objects=%d want=%d err=%v", got, want, err)
		}
	}
	reconcile()
	count(3)
	if err := dispatch.ObserveBootSeedStopped(t.Context(), f.pool, *f.host(), bootSeedReceipt(r)); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computer_leases SET boot_seed_build_id=$2 WHERE environment_id=$1 AND computer_id=$3 AND epoch=1`, f.env, id, f.computer)
	reconcile()
	count(3)
	if err := dispatch.ObserveLeaseStopped(t.Context(), f.pool, *f.host(), identity, uuid.Nil(), uuid.NewV7()); err != nil {
		t.Fatalf("optional seed mismatch blocked physical stop: %v", err)
	}
	var stoppedWithoutExclusion bool
	if err := f.pool.QueryRow(t.Context(), `SELECT fenced_at IS NOT NULL AND boot_seed_failed_at IS NULL FROM computer_leases WHERE environment_id=$1 AND computer_id=$2 AND epoch=1`, f.env, f.computer).Scan(&stoppedWithoutExclusion); err != nil || !stoppedWithoutExclusion {
		t.Fatalf("mismatched hint affected exact stop: %v %v", stoppedWithoutExclusion, err)
	}
	for range 2 {
		if err := dispatch.ObserveLeaseStopped(t.Context(), f.pool, *f.host(), identity, uuid.Nil(), id); err != nil {
			t.Fatalf("selected seed failure/reply replay: %v", err)
		}
	}
	var failed bool
	if err := f.pool.QueryRow(t.Context(), `SELECT boot_seed_failed_at IS NOT NULL FROM computer_leases WHERE environment_id=$1 AND computer_id=$2 AND epoch=1`, f.env, f.computer).Scan(&failed); err != nil || !failed {
		t.Fatalf("selected failure not retained: %v %v", failed, err)
	}
	// A fresh allocation for the same Computer may ordinary-boot, but must not
	// select its failed seed again. Publication remains eligible globally.
	next := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO computer_leases(environment_id,computer_id,epoch,worker_host_id,worker_epoch,expires_at,allocation_id,channel_credential_digest,delivered_at,reserved_cpu_millis,reserved_memory_bytes,reserved_scratch_bytes,vm_platform_id,vm_vcpu_count,cpu_config_digest,status)
 SELECT environment_id,computer_id,2,worker_host_id,worker_epoch,clock_timestamp()+interval '1 hour',$3,channel_credential_digest,clock_timestamp(),reserved_cpu_millis,reserved_memory_bytes,reserved_scratch_bytes,vm_platform_id,vm_vcpu_count,cpu_config_digest,'acquiring'
 FROM computer_leases WHERE environment_id=$1 AND computer_id=$2 AND epoch=1;
 UPDATE session_processes SET computer_lease_epoch=2 WHERE environment_id=$1 AND computer_id=$2`, pgx.QueryExecModeSimpleProtocol, f.env, f.computer, next)
	identity.InstanceID, identity.Epoch = next, 2
	selectSeed(program, false)
	var eligible bool
	if err := f.pool.QueryRow(t.Context(), `SELECT status='succeeded' AND payload_retired_at IS NULL FROM boot_seed_builds WHERE environment_id=$1 AND id=$2`, f.env, id).Scan(&eligible); err != nil || !eligible {
		t.Fatalf("worker failure revoked shared publication: %v %v", eligible, err)
	}
	reconcile()
	count(0)
	if err := publisher.Publish(context.Background(), *f.host(), delivery.Executor, manifest, proof); err != nil {
		t.Fatalf("retired metadata lost publication receipt: %v", err)
	}
}

func TestBootSeedHostRecoveryRetainsUnknownQualificationVM(t *testing.T) {
	p, a, id := bootSeedFixture(t)
	f := p.f
	if _, err := a.AllocateBootSeed(t.Context(), f.env, id, f.worker); err != nil {
		t.Fatal(err)
	}
	host := recoveringComputerHost(t, f.fixture)
	// Qualification children have fresh identities, distinct from the allocation.
	if err := dispatch.ObserveRecoveredHost(t.Context(), f.pool, host, []uuid.UUID{uuid.NewV7()}); err != nil {
		t.Fatal(err)
	}
	assertFenced := func(want bool) {
		t.Helper()
		var got bool
		if err := f.pool.QueryRow(t.Context(), `SELECT fenced_at IS NOT NULL FROM boot_seed_builds WHERE environment_id=$1 AND id=$2`, f.env, id).Scan(&got); err != nil || got != want {
			t.Fatalf("build fenced=%v want=%v err=%v", got, want, err)
		}
	}
	assertFenced(false)
	if err := dispatch.ObserveRecoveredHost(t.Context(), f.pool, host, []uuid.UUID{}); err != nil {
		t.Fatal(err)
	}
	assertFenced(true)
}

func TestBootSeedBacklogDoesNotCreatePlanningDemand(t *testing.T) {
	p, _, _ := bootSeedFixture(t)
	f := p.f
	rows, err := db.New(f.pool).ListAllocationPlanningDemand(t.Context(), db.ListAllocationPlanningDemandParams{RegionID: "test", WorkerGroupID: pgvalue.UUID(f.group), RowLimit: 100})
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.Kind == "seed" {
			t.Fatalf("optional seed created demand: %+v", row)
		}
	}
}

func TestBootSeedQueuedDemandDoesNotPreventIdleDrain(t *testing.T) {
	p, _, id := bootSeedFixture(t)
	f := p.f
	present, err := workergroup.HasQueuedDemand(t.Context(), f.pool, f.group)
	if err != nil || present {
		t.Fatalf("optional seed blocked idle drain: %v %v", present, err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE boot_seed_builds SET status='failed',error_code='fixture-failed' WHERE id=$1`, id)
	present, err = workergroup.HasQueuedDemand(t.Context(), f.pool, f.group)
	if err != nil || present {
		t.Fatalf("failed seed still prevents idle drain: %v %v", present, err)
	}
}

func TestBootSeedRetentionBoundsUnpromotedGenerations(t *testing.T) {
	for _, promoted := range []bool{false, true} {
		t.Run(fmt.Sprint(promoted), func(t *testing.T) {
			p, _, id := bootSeedFixture(t)
			f := p.f
			first, last := uuid.NewV7(), uuid.NewV7()
			firstSeed, lastSeed := uuid.NewV7(), uuid.NewV7()
			dbtest.MustExec(t, t.Context(), f.pool, `UPDATE deployments SET created_at='2026-01-01' WHERE environment_id=$1;
 INSERT INTO deployments(environment_id,id,bundle_digest,created_at) VALUES($1,$2,$4,'2026-01-02'),($1,$3,$5,'2026-01-02');
 INSERT INTO boot_seed_builds(environment_id,id,deployment_id,image_spec_id,image_id,cpu_millis,memory_bytes,status)
 SELECT environment_id,$6::uuid,$2::uuid,image_spec_id,image_id,cpu_millis,memory_bytes,'queued' FROM boot_seed_builds WHERE id=$8
 UNION ALL SELECT environment_id,$7::uuid,$3::uuid,image_spec_id,image_id,cpu_millis,memory_bytes,'queued' FROM boot_seed_builds WHERE id=$8`, pgx.QueryExecModeSimpleProtocol, f.env, first, last, sha256sum.DigestBytes([]byte("first")), sha256sum.DigestBytes([]byte("last")), firstSeed, lastSeed, id)
			if !promoted {
				dbtest.MustExec(t, t.Context(), f.pool, `UPDATE environments SET current_deployment_id=NULL WHERE id=$1`, f.env)
			}
			if err := computer.ReconcileDiskRetention(t.Context(), f.pool); err != nil {
				t.Fatal(err)
			}
			for seed, want := range map[uuid.UUID]bool{id: !promoted, firstSeed: true, lastSeed: false} {
				var retired bool
				if err := f.pool.QueryRow(t.Context(), `SELECT payload_retired_at IS NOT NULL FROM boot_seed_builds WHERE id=$1`, seed).Scan(&retired); err != nil || retired != want {
					t.Fatalf("seed %s retired=%v want=%v: %v", seed, retired, want, err)
				}
			}
		})
	}
}
